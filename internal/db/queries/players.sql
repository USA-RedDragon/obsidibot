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

-- CreditKill is the killer's half of a rated kill.
-- name: CreditKill :exec
update players
   set kills        = kills + 1,
       rated_games  = rated_games + 1,
       rating       = $2,
       last_seen_at = now()
 where alderon_id = $1;

-- RecordRatedLoss is the victim's half of a rated kill: a death that also
-- moved their rating.
-- name: RecordRatedLoss :exec
update players
   set deaths       = deaths + 1,
       rated_games  = rated_games + 1,
       rating       = $2,
       last_seen_at = now()
 where alderon_id = $1;

-- RecordUnratedDeath is an environmental death. It counts against K/D and
-- leaves the rating alone: there is no opponent to take the points, and
-- inventing one would drain the pool and deflate every rating over time.
-- name: RecordUnratedDeath :exec
update players
   set deaths       = deaths + 1,
       last_seen_at = now()
 where alderon_id = $1;

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
