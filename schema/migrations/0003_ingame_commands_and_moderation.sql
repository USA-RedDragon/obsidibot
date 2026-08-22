-- Increment 2: in-game chat commands and moderation.
--
-- Two features share this migration. Feature 1 (in-game commands via the
-- PlayerCommand webhook) re-keys link_challenges so a challenge can be opened
-- from INSIDE the game, where no Discord user exists yet. Feature 2 adds
-- warns/bans with RCON enforcement and the guild settings they need.
--
-- The RCON behaviour this schema is built against was verified live during
-- planning, not read from documentation: the game's own timed ban writes a
-- corrupt bans.txt row (empty id field) that can never be lifted, so obsidibot
-- issues only PERMANENT game bans and owns expiry itself; `Ban` refuses
-- targets in Game.ini's ServerAdmins list (a PERMANENT condition -- the check
-- reads the list, not the in-game role); and `Unban` lifts bans however they
-- were loaded (command, reload, boot) EXCEPT when the target is currently a
-- listed admin.

begin;

-- ============================================================================
-- Feature 1: link_challenges re-keyed by the identity being claimed.
--
-- The old table was keyed by discord_user_id, which cannot represent an
-- in-game `!link` -- the player has an AGID but no Discord user until someone
-- runs /link confirm. Challenges are five-minute ephemera: dropping live ones
-- at deploy costs at most one retyped /link start, which is cheaper than
-- migrating rows that expire before the deploy finishes.
-- ============================================================================

drop table link_challenges;

create table link_challenges (
    alderon_id      text primary key,
    -- NULL means the challenge was initiated in game and is unclaimed: any
    -- Discord user presenting the whispered code may claim it. Non-null means
    -- a specific Discord user opened it with /link start. UNIQUE keeps one
    -- pending challenge per Discord user, as the old primary key did.
    discord_user_id text unique,
    player_name     text        not null,
    code_hash       bytea       not null,
    attempts        integer     not null default 0,
    created_at      timestamptz not null default now(),
    expires_at      timestamptz not null
);

create index link_challenges_expiry on link_challenges (expires_at);

-- ============================================================================
-- Feature 2: moderation.
--
-- Keyed like everything else on the ALDERON ID when one is known, but a
-- moderator must be able to act on a Discord account that has never linked --
-- so both identifier columns are nullable and at least one must be present.
-- No foreign keys to players/player_links: the whole point is the target may
-- be someone the bot has never seen.
--
-- A player may be warned by AGID before linking and by @user after: the SAME
-- person, one record. Every count and lookup therefore OR-matches both
-- identifiers, with callers resolving both sides through player_links first.
-- Note /link remove keeps these rows intact (they carry snapshots, not FKs);
-- if the same human is then warned by @user before re-linking, a second
-- record accrues until the identifiers reunite -- accepted.
-- ============================================================================

alter table guild_config
    add column mod_role_id          text,
    add column ban_feed_channel_id  text,
    add column warn_feed_channel_id text;

create table warns (
    id                   bigserial   primary key,
    alderon_id           text,
    discord_user_id      text,
    -- Display snapshot, so the feed and /modstats stay readable after a
    -- rename or an unlink.
    target_name          text,
    reason               text        not null,
    issued_by_discord_id text        not null,
    created_at           timestamptz not null default now(),
    constraint warns_target_present
        check (alderon_id is not null or discord_user_id is not null)
);

-- Both sides of the OR-match that /modstats and "warning #N" run.
create index warns_by_agid    on warns (alderon_id)      where alderon_id is not null;
create index warns_by_discord on warns (discord_user_id) where discord_user_id is not null;

create table bans (
    id                   bigserial   primary key,
    alderon_id           text,
    discord_user_id      text,
    target_name          text,
    reason               text        not null,
    issued_by_discord_id text        not null,
    created_at           timestamptz not null default now(),
    -- Null = permanent. The game NEVER receives this: its native timed ban is
    -- broken (verified live -- corrupt row, unliftable), so every game ban is
    -- placed permanent and the scheduler lifts it here when this passes.
    expires_at           timestamptz,
    -- When Kick+Ban landed over RCON. Null with lifted_at null means the
    -- scheduler still owes the game an enforcement (target offline at issue,
    -- RCON down, or no AGID known yet).
    enforced_at          timestamptz,
    -- Set ONLY after the game unban succeeded (or when no game ban was ever
    -- placed). Never before: a row marked lifted while the game still bans
    -- the player is a ban nobody can see or remove.
    lifted_at            timestamptz,
    lift_reason          text,
    -- Set when enforcement hit a PERMANENT refusal (today: the game's
    -- "Cannot ban an admin." for ServerAdmins-listed targets, or a command
    -- that can never fit the RCON length limit). The enforce and audit passes
    -- skip such rows instead of retrying a refusal every tick forever; the
    -- unenforced gauge still counts them, and /modstats and the /ban reply
    -- show the reason.
    unenforceable_reason text,
    constraint bans_target_present
        check (alderon_id is not null or discord_user_id is not null)
);

-- ONE ACTIVE BAN PER IDENTITY, enforced by the database exactly as
-- bank_ledger_one_inflight is: replicas race, application checks cannot win
-- that race, partial unique indexes cannot lose it. Two indexes because
-- either identifier may be the only one known.
create unique index bans_one_active_agid    on bans (alderon_id)      where lifted_at is null;
create unique index bans_one_active_discord on bans (discord_user_id) where lifted_at is null;

-- The scheduler's sweeps: enforcement backlog and expiry, both partial so the
-- planner never walks history.
create index bans_unenforced on bans (created_at)
    where lifted_at is null and enforced_at is null;
create index bans_expiring   on bans (expires_at)
    where lifted_at is null and expires_at is not null;

commit;
