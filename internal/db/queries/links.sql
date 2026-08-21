-- name: GetLinkByDiscordID :one
select * from player_links where discord_user_id = $1;

-- name: GetLinkByAlderonID :one
select * from player_links where alderon_id = $1;

-- name: CreateLink :exec
insert into player_links (discord_user_id, alderon_id) values ($1, $2);

-- name: DeleteLinkByDiscordID :execrows
delete from player_links where discord_user_id = $1;

-- UpsertChallenge replaces any challenge the caller already had, so running
-- /link start twice simply reissues rather than erroring.
-- name: UpsertChallenge :exec
insert into link_challenges (discord_user_id, alderon_id, player_name, code_hash, expires_at)
values ($1, $2, $3, $4, $5)
on conflict (discord_user_id) do update
    set alderon_id  = excluded.alderon_id,
        player_name = excluded.player_name,
        code_hash   = excluded.code_hash,
        attempts    = 0,
        created_at  = now(),
        expires_at  = excluded.expires_at;

-- GetLiveChallengeByAlderonID finds an unexpired challenge for an identity,
-- whoever started it. It is what stops one Discord user repeatedly issuing
-- challenges against somebody else's account: each attempt whispers a code to
-- that player in game, and without this the command is a spam button.
-- name: GetLiveChallengeByAlderonID :one
select * from link_challenges
 where alderon_id = $1 and expires_at > now();

-- name: GetChallenge :one
select * from link_challenges where discord_user_id = $1;

-- name: DeleteChallengeByAlderonID :exec
delete from link_challenges where alderon_id = $1;

-- name: IncrementChallengeAttempts :one
update link_challenges
   set attempts = attempts + 1
 where discord_user_id = $1
returning attempts;

-- name: DeleteChallenge :exec
delete from link_challenges where discord_user_id = $1;

-- name: DeleteExpiredChallenges :execrows
delete from link_challenges where expires_at < now();
