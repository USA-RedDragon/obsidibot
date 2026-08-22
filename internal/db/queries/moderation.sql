-- Moderation queries.
--
-- IDENTITY MATCHING RULE: a player warned by AGID before linking and by @user
-- after is ONE person with ONE record. Every count and lookup here therefore
-- takes BOTH identifiers and OR-matches them; callers resolve both sides
-- through player_links before calling. The sqlc.narg guards are load-bearing:
-- a plain `col = $2` with a NULL parameter matches nothing in SQL, so the
-- null-check must be explicit in each arm.

-- name: InsertWarn :one
insert into warns (alderon_id, discord_user_id, target_name, reason, issued_by_discord_id)
values (sqlc.narg('alderon_id'), sqlc.narg('discord_user_id'), sqlc.narg('target_name'),
        $1, $2)
returning *;

-- name: CountWarns :one
select count(*) from warns
 where (sqlc.narg('alderon_id')::text is not null and alderon_id = sqlc.narg('alderon_id'))
    or (sqlc.narg('discord_user_id')::text is not null and discord_user_id = sqlc.narg('discord_user_id'));

-- name: ListRecentWarns :many
select * from warns
 where (sqlc.narg('alderon_id')::text is not null and alderon_id = sqlc.narg('alderon_id'))
    or (sqlc.narg('discord_user_id')::text is not null and discord_user_id = sqlc.narg('discord_user_id'))
 order by created_at desc
 limit $1;

-- name: InsertBan :one
insert into bans (alderon_id, discord_user_id, target_name, reason, issued_by_discord_id, expires_at)
values (sqlc.narg('alderon_id'), sqlc.narg('discord_user_id'), sqlc.narg('target_name'),
        $1, $2, sqlc.narg('expires_at'))
returning *;

-- name: CountBans :one
select count(*) from bans
 where (sqlc.narg('alderon_id')::text is not null and alderon_id = sqlc.narg('alderon_id'))
    or (sqlc.narg('discord_user_id')::text is not null and discord_user_id = sqlc.narg('discord_user_id'));

-- name: ListRecentBans :many
select * from bans
 where (sqlc.narg('alderon_id')::text is not null and alderon_id = sqlc.narg('alderon_id'))
    or (sqlc.narg('discord_user_id')::text is not null and discord_user_id = sqlc.narg('discord_user_id'))
 order by created_at desc
 limit $1;

-- name: GetActiveBans :many
select * from bans
 where lifted_at is null
   and ((sqlc.narg('alderon_id')::text is not null and alderon_id = sqlc.narg('alderon_id'))
     or (sqlc.narg('discord_user_id')::text is not null and discord_user_id = sqlc.narg('discord_user_id')))
 order by created_at;

-- LiftBan is conditional on the row still being active, so two lifters -- an
-- admin's /unban and the expiry sweep -- cannot both claim it.
-- name: LiftBan :execrows
update bans
   set lifted_at   = now(),
       lift_reason = $2
 where id = $1 and lifted_at is null;

-- LiftUnenforcedBan lifts only if the scheduler has NOT enforced it yet; zero
-- rows tells /unban to re-read and take the RCON Unban path instead.
-- name: LiftUnenforcedBan :execrows
update bans
   set lifted_at   = now(),
       lift_reason = $2
 where id = $1 and lifted_at is null and enforced_at is null;

-- NextUnenforcedBans feeds the scheduler's enforce pass.
--
-- THE EXPIRY FILTER IS LOAD-BEARING: a ban that expired while unenforced
-- (target unlinked, RCON down for longer than the duration) must never be
-- enforced late -- that would Kick+Ban an innocent player seconds before the
-- lift pass unbans them. This filter was lost once in review and restored;
-- do not "simplify" it away.
--
-- unenforceable_reason rows are excluded: those hit a PERMANENT refusal
-- (admin target, over-long command) and retrying a refusal every tick is
-- noise, not enforcement.
-- name: NextUnenforcedBans :many
select * from bans
 where lifted_at is null
   and enforced_at is null
   and unenforceable_reason is null
   and alderon_id is not null
   and (expires_at is null or expires_at > now())
 order by created_at
 limit $1;

-- MarkBanEnforced's guard closes the /unban-vs-scheduler race: /unban can lift
-- an unenforced row while the scheduler is mid-enforce over RCON. Without the
-- guard the scheduler would then record enforcement on a LIFTED row -- a game
-- ban bound to a closed record, invisible to the audit (which walks active
-- bans) forever. Zero rows tells the scheduler to issue a compensating Unban.
-- name: MarkBanEnforced :execrows
update bans
   set enforced_at = now()
 where id = $1 and lifted_at is null;

-- name: MarkBanUnenforceable :exec
update bans
   set unenforceable_reason = $2
 where id = $1 and lifted_at is null;

-- name: NextExpiredBans :many
select * from bans
 where lifted_at is null
   and expires_at is not null
   and expires_at <= now()
 order by expires_at
 limit $1;

-- BackfillBanAgids attaches the AGID to bans recorded against a then-unlinked
-- Discord account once the player links, so the enforce pass can reach them.
-- The not-exists guard skips a person who ALSO has an active AGID ban, which
-- would trip bans_one_active_agid; ListBackfillBlocked finds those so the
-- caller can close them as superseded instead of leaving them in limbo
-- (unenforceable forever, holding the unenforced gauge red, and springing a
-- surprise re-ban when the AGID ban later expires and backfill finally
-- attaches).
-- name: BackfillBanAgids :execrows
update bans b
   set alderon_id = pl.alderon_id
  from player_links pl
 where pl.discord_user_id = b.discord_user_id
   and b.alderon_id is null
   and b.lifted_at is null
   and not exists (select 1 from bans b2
                    where b2.alderon_id = pl.alderon_id and b2.lifted_at is null);

-- name: ListBackfillBlocked :many
select b.id from bans b
  join player_links pl on pl.discord_user_id = b.discord_user_id
 where b.alderon_id is null
   and b.lifted_at is null
   and exists (select 1 from bans b2
                where b2.alderon_id = pl.alderon_id and b2.lifted_at is null);

-- NextAuditBans feeds the hourly audit-by-reassertion pass: every active,
-- enforced, enforceable ban with an AGID gets its Ban re-issued; the verified
-- "already banned" response is health, an unexpected success is a repair.
-- name: NextAuditBans :many
select * from bans
 where lifted_at is null
   and enforced_at is not null
   and unenforceable_reason is null
   and alderon_id is not null
 order by id
 limit $1;

-- CountUnenforcedActiveBans backs the alerting gauge. It deliberately does NOT
-- require alderon_id: unenforceable-because-unlinked is exactly what the gauge
-- should surface. It DOES exclude rows already flagged unenforceable -- those
-- are surfaced through /modstats and the flagging log line instead, and a
-- permanently red gauge nobody can action trains people to ignore it.
-- name: CountUnenforcedActiveBans :one
select count(*) from bans
 where lifted_at is null
   and enforced_at is null
   and unenforceable_reason is null
   and (expires_at is null or expires_at > now());
