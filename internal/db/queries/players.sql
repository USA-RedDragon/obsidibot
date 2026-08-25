-- UpsertPlayerSeen records that we have heard of this identity. The initial
-- rating is a parameter rather than a column default so rating.initial in the
-- configuration stays the single source of that number.
-- name: UpsertPlayerSeen :exec
insert into players (alderon_id, last_known_name, rating)
values ($1, $2, $3)
on conflict (alderon_id) do update
    set last_known_name = excluded.last_known_name,
        last_seen_at    = now();

-- name: GetPlayer :one
select * from players where alderon_id = $1;

-- name: GetPlayerByDiscordID :one
select p.*
  from players p
  join player_links l on l.alderon_id = p.alderon_id
 where l.discord_user_id = $1;

-- UpsertPlayerSeenAt is UpsertPlayerSeen for the rating applier, which is
-- processing an event that happened at a KNOWN time rather than reacting to
-- something happening now.
--
-- The distinction only shows itself during a replay: with now(), rebuilding
-- history stamps every player as active this instant, which lies in /stats and
-- silently resets their decay clock for another grace period. In live
-- ingestion seen_at is the event's received_at, milliseconds from now(), so
-- nothing changes.
--
-- last_seen_at and first_seen_at are named EXPLICITLY on the insert path. Both
-- columns default to now(), so relying on the default would stamp a player
-- first created during a replay with the replay's own clock -- the exact bug
-- the conflict clause below avoids.
-- name: UpsertPlayerSeenAt :exec
insert into players (alderon_id, last_known_name, rating, first_seen_at, last_seen_at)
values ($1, $2, $3, sqlc.arg(seen_at), sqlc.arg(seen_at))
on conflict (alderon_id) do update
    set last_known_name = excluded.last_known_name,
        last_seen_at    = greatest(players.last_seen_at, sqlc.arg(seen_at));

-- CreditKill is the killer's half of a rated kill.
--
-- greatest() rather than assignment: a replay walks events oldest-first, and a
-- player's true last activity may be a bank operation or a link that happened
-- after their last kill. Monotonic means a replay can never drag that
-- backwards.
-- name: CreditKill :exec
update players
   set kills        = kills + 1,
       rated_games  = rated_games + 1,
       rating       = $2,
       last_seen_at = greatest(players.last_seen_at, sqlc.arg(seen_at))
 where alderon_id = $1;

-- RecordRatedLoss is the victim's half of a rated kill: a death that also
-- moved their rating.
-- name: RecordRatedLoss :exec
update players
   set deaths       = deaths + 1,
       rated_games  = rated_games + 1,
       rating       = $2,
       last_seen_at = greatest(players.last_seen_at, sqlc.arg(seen_at))
 where alderon_id = $1;

-- RecordUnratedDeath is a death nothing can be rated against -- the world, or
-- a kill the rules do not credit. It counts against K/D and leaves the rating
-- alone: there is no opponent to take the points, and inventing one would
-- drain the pool and deflate every rating over time.
-- name: RecordUnratedDeath :exec
update players
   set deaths       = deaths + 1,
       last_seen_at = greatest(players.last_seen_at, sqlc.arg(seen_at))
 where alderon_id = $1;

-- ResetPlayerAggregates rewinds every player to the starting state so the
-- rating applier can rebuild them from the events.
--
-- decayed_at and last_seen_at are deliberately UNTOUCHED: neither is derivable
-- from the event stream, and clearing them would either re-run decay from
-- scratch or mark the whole server active. The initial rating is a parameter
-- because rating.initial lives in configuration.
-- name: ResetPlayerAggregates :exec
update players
   set rating      = sqlc.arg(initial)::double precision,
       kills       = 0,
       deaths      = 0,
       rated_games = 0;

-- TopPlayers is the leaderboard ordering. Players with no record at all are
-- excluded so the board is not a list of people tied at the starting rating.
-- name: TopPlayers :many
select p.*, l.discord_user_id
  from players p
  left join player_links l on l.alderon_id = p.alderon_id
 where p.kills > 0 or p.deaths > 0
 order by p.rating desc, p.kills desc, p.alderon_id asc
 limit $1;

-- DecayCandidates lists ratings above the floor that have been idle past the
-- grace period and have not already been decayed up to now.
-- name: DecayCandidates :many
select * from players
 where rating > sqlc.arg(floor)::double precision
   and last_seen_at < now() - make_interval(days => sqlc.arg(grace_days)::int)
   and decayed_at   < now() - interval '1 day'
 order by alderon_id
 limit $1;

-- name: ApplyDecay :exec
update players
   set rating     = $2,
       decayed_at = now()
 where alderon_id = $1;
