-- InsertKillEvent is the only write the ingest endpoint makes. A repeat
-- delivery collides on dedupe_key and returns no row, which the caller reports
-- as a duplicate rather than an error.
-- name: InsertKillEvent :one
insert into kill_events (
    dedupe_key, server_guid, payload,
    victim_agid, victim_name, victim_dino, victim_growth, victim_poi,
    victim_character, victim_role, victim_location, victim_is_admin,
    killer_agid, killer_name, killer_dino, killer_growth, killer_is_admin,
    killer_character, killer_role, killer_location,
    damage_type, credited, counts_death, kill_distance, time_of_day
) values (
    $1, $2, $3,
    $4, $5, $6, $7, $8,
    $9, $10, $11, $12,
    $13, $14, $15, $16, $17,
    $18, $19, $20,
    $21, $22, $23, $24, $25
)
on conflict (dedupe_key) do nothing
returning id;

-- NextUnratedEvents walks the queue in id order, which IS the rating order.
-- Elo is order-dependent, so this must never be parallelised across rows.
-- The payload and dedupe_key columns are deliberately NOT selected: neither
-- worker reads them, and the payload is the whole raw webhook. Shipping it 200
-- rows at a time cost 440 KB per pass against 208 KB for this projection.
-- name: NextUnratedEvents :many
select id, received_at, server_guid,
       victim_agid, victim_name, victim_dino, victim_growth, victim_poi,
       victim_character, victim_role, victim_location, victim_is_admin,
       killer_agid, killer_name, killer_dino, killer_growth, killer_is_admin,
       killer_character, killer_role, killer_location,
       damage_type, credited, counts_death, kill_distance, time_of_day,
       killer_rating_before, killer_rating_after,
       victim_rating_before, victim_rating_after,
       rated, posted
  from kill_events
 where not rated
 order by id
 limit $1;

-- MarkEventRated closes an event and records what it did to both ratings.
--
-- The four figures are stored rather than recomputed because the feed and
-- /stats need to show the Elo a kill moved, and only the applier -- walking the
-- events in order -- ever knows it. They are null for an event that moved
-- nothing.
-- name: MarkEventRated :exec
update kill_events
   set rated                = true,
       killer_rating_before = sqlc.narg(killer_rating_before),
       killer_rating_after  = sqlc.narg(killer_rating_after),
       victim_rating_before = sqlc.narg(victim_rating_before),
       victim_rating_after  = sqlc.narg(victim_rating_after)
 where id = $1;

-- name: NextUnpostedEvents :many
select id, received_at, server_guid,
       victim_agid, victim_name, victim_dino, victim_growth, victim_poi,
       victim_character, victim_role, victim_location, victim_is_admin,
       killer_agid, killer_name, killer_dino, killer_growth, killer_is_admin,
       killer_character, killer_role, killer_location,
       damage_type, credited, counts_death, kill_distance, time_of_day,
       killer_rating_before, killer_rating_after,
       victim_rating_before, victim_rating_after,
       rated, posted
  from kill_events
 where not posted and rated
 order by id
 limit $1;

-- name: MarkEventPosted :exec
update kill_events set posted = true where id = $1;

-- name: CountUnratedEvents :one
select count(*) from kill_events where not rated;

-- name: CountUnpostedEvents :one
select count(*) from kill_events where not posted;

-- PruneProcessedEvents ages out the RAW PAYLOAD of events both workers have
-- finished with, and keeps the row.
--
-- It used to delete the row, which quietly made two things impossible: showing
-- a player their whole history in /stats, and replaying a rule change against
-- anything older than the window. Both matter -- the rules have now been wrong
-- twice -- and a slim row is a few hundred bytes, so keeping every event
-- costs a megabyte or two a year while deleting it costs the ability to
-- explain a rating.
--
-- Nothing replays from the payload: the credit rule is derived from columns
-- (internal/kills/rules.go). What is lost here is forensic detail -- the exact
-- bytes the game sent -- which is worth having for a month and not worth
-- storing forever.
-- name: PruneProcessedEvents :execrows
update kill_events
   set payload = null
 where payload is not null
   and rated and posted
   and received_at < now() - make_interval(days => sqlc.arg(retention_days)::int);

-- ClaimRatingReplay takes ownership of a pending replay, if there is one.
--
-- THIS IS THE MUTUAL EXCLUSION for the whole replay, and it does not need an
-- advisory lock on top. internal/leader hands out no fencing token, so a
-- zombie leader can still believe it leads; two of them reaching here take the
-- row lock in turn, and the loser re-evaluates `completed_at is null` after
-- the winner commits, matches nothing, and skips. Stamping completion at CLAIM
-- time rather than afterwards is what makes that work.
--
-- An advisory lock keyed like the ratings job would be worse than useless: the
-- leader already holds that key as a session lock on its own connection, so a
-- pooled transaction asking for it would wait on its own process forever.
-- name: ClaimRatingReplay :one
update rating_replays
   set completed_at = now()
 where id = (select id from rating_replays
              where completed_at is null
              order by id
              limit 1)
returning id, reason, min_event_id;

-- name: MinEventID :one
select coalesce(min(id), 0)::bigint from kill_events;

-- name: MaxEventID :one
select coalesce(max(id), 0)::bigint from kill_events;

-- RequeueRatedEvents hands every event back to the rating applier.
--
-- The `where rated` is not decoration: without it this rewrites every row on
-- every replay, holding row locks against the feed for the duration. It must
-- NEVER touch `posted` -- clearing that would re-post the server's entire kill
-- history to Discord.
-- name: RequeueRatedEvents :execrows
update kill_events set rated = false where rated;

-- PlayerHistory is one page of a player's timeline, newest first.
--
-- Written as a union of two indexed halves rather than `killer_agid = $1 or
-- victim_agid = $1`, so each half uses its own index. The second half excludes
-- rows where the player is BOTH sides: an environmental death names the victim
-- as their own killer, and it would otherwise appear twice on the page.
--
-- max_id anchors the whole pagination to the moment the first page was drawn.
-- Without it a kill landing mid-browse shifts every row down and the reader
-- sees the same event on two pages -- which looks exactly like the bug a
-- player checking up on their rating is hoping to find.
-- name: PlayerHistory :many
select id, received_at, damage_type,
       victim_agid, victim_name, victim_dino,
       killer_agid, killer_name, killer_dino,
       killer_rating_before, killer_rating_after,
       victim_rating_before, victim_rating_after
  from (
    select k.* from kill_events k
     where k.killer_agid = sqlc.arg(agid) and k.id <= sqlc.arg(max_id)
    union all
    select k.* from kill_events k
     where k.victim_agid = sqlc.arg(agid) and k.id <= sqlc.arg(max_id)
       and k.killer_agid is distinct from sqlc.arg(agid)
  ) timeline
 order by id desc
 limit sqlc.arg(page_size) offset sqlc.arg(page_offset);

-- name: CountPlayerHistory :one
select count(*) from (
    select k.id from kill_events k
     where k.killer_agid = sqlc.arg(agid) and k.id <= sqlc.arg(max_id)
    union all
    select k.id from kill_events k
     where k.victim_agid = sqlc.arg(agid) and k.id <= sqlc.arg(max_id)
       and k.killer_agid is distinct from sqlc.arg(agid)
) timeline;
