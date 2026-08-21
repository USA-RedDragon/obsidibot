-- InsertKillEvent is the only write the ingest endpoint makes. A repeat
-- delivery collides on dedupe_key and returns no row, which the caller reports
-- as a duplicate rather than an error.
-- name: InsertKillEvent :one
insert into kill_events (
    dedupe_key, server_guid, payload,
    victim_agid, victim_name, victim_dino, victim_growth, victim_poi,
    killer_agid, killer_name, killer_dino, killer_growth, killer_is_admin,
    damage_type, credited, counts_death
) values (
    $1, $2, $3,
    $4, $5, $6, $7, $8,
    $9, $10, $11, $12, $13,
    $14, $15, $16
)
on conflict (dedupe_key) do nothing
returning id;

-- NextUnratedEvents walks the queue in id order, which IS the rating order.
-- Elo is order-dependent, so this must never be parallelised across rows.
-- name: NextUnratedEvents :many
select * from kill_events
 where not rated
 order by id
 limit $1;

-- name: MarkEventRated :exec
update kill_events set rated = true where id = $1;

-- name: NextUnpostedEvents :many
select * from kill_events
 where not posted
 order by id
 limit $1;

-- name: MarkEventPosted :exec
update kill_events set posted = true where id = $1;

-- name: CountUnratedEvents :one
select count(*) from kill_events where not rated;

-- name: CountUnpostedEvents :one
select count(*) from kill_events where not posted;

-- PruneProcessedEvents drops history that both workers are finished with. The
-- aggregates live on the player row, so this loses no stats -- only the
-- ability to replay a rule change against events older than the window.
-- name: PruneProcessedEvents :execrows
delete from kill_events
 where rated and posted
   and received_at < now() - make_interval(days => sqlc.arg(retention_days)::int);
