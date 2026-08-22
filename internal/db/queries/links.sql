-- name: GetLinkByDiscordID :one
select * from player_links where discord_user_id = $1;

-- name: GetLinkByAlderonID :one
select * from player_links where alderon_id = $1;

-- name: CreateLink :exec
insert into player_links (discord_user_id, alderon_id) values ($1, $2);

-- name: DeleteLinkByDiscordID :execrows
delete from player_links where discord_user_id = $1;

-- UpsertChallenge opens or replaces the challenge for an identity. The
-- conflict target is the IDENTITY being claimed, not the claimant: an in-game
-- !link has no Discord user yet (discord_user_id null = unclaimed), and the
-- person in game is the identity's authority, so their reissue replaces even a
-- Discord-initiated challenge for the same AGID.
-- name: UpsertChallenge :exec
insert into link_challenges (alderon_id, discord_user_id, player_name, code_hash, expires_at)
values ($1, $2, $3, $4, $5)
on conflict (alderon_id) do update
    set discord_user_id = excluded.discord_user_id,
        player_name     = excluded.player_name,
        code_hash       = excluded.code_hash,
        attempts        = 0,
        created_at      = now(),
        expires_at      = excluded.expires_at;

-- name: GetChallengeByDiscordID :one
select * from link_challenges where discord_user_id = $1;

-- name: GetChallengeByAlderonID :one
select * from link_challenges where alderon_id = $1;

-- GetLiveChallengeByAlderonID backs /link start's stomp-guard: without it,
-- naming somebody else's identity would replace their pending challenge and
-- whisper them a fresh code -- a spam button pointed at whoever the caller
-- names.
-- name: GetLiveChallengeByAlderonID :one
select * from link_challenges where alderon_id = $1 and expires_at > now();

-- ListUnclaimedLiveChallenges is /link confirm's fallback for in-game-
-- initiated links: no Discord user owns them, so the code itself is the claim.
-- Bounded by players online, not by table growth -- expired rows are excluded
-- here and swept separately.
-- name: ListUnclaimedLiveChallenges :many
select * from link_challenges where discord_user_id is null and expires_at > now();

-- name: IncrementChallengeAttempts :one
update link_challenges
   set attempts = attempts + 1
 where alderon_id = $1
returning attempts;

-- name: DeleteChallengeByAlderonID :exec
delete from link_challenges where alderon_id = $1;

-- DeleteChallengeByDiscordID clears the caller's previous challenge before a
-- new one is opened against a DIFFERENT identity: unique(discord_user_id)
-- would otherwise refuse the upsert. Run in the same transaction as the
-- upsert it clears the way for.
-- name: DeleteChallengeByDiscordID :execrows
delete from link_challenges where discord_user_id = $1;

-- name: DeleteExpiredChallenges :execrows
delete from link_challenges where expires_at < now();
