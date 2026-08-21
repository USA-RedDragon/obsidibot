-- obsidibot's schema. Unlike the services that read a shared control plane,
-- obsidibot OWNS this database and applies its own DDL on startup.
--
-- The governing key is the ALDERON ID, not the Discord user id. Kills arrive
-- from the game carrying AGIDs and nothing else, so stats and balances have to
-- hang off the identity the game knows about. A Discord account is a label
-- attached to that identity later, and detaching it must not destroy anything:
-- that is why /link remove deletes one row in player_links and touches nothing
-- else.

begin;

-- guild_config holds the settings a server moderator owns. These are
-- deliberately NOT in obsidibot's config file: a moderator must be able to
-- move the kill feed without a redeploy.
create table guild_config (
    guild_id                text primary key,
    kill_feed_channel_id    text,
    leaderboard_channel_id  text,
    -- The persistent message the leaderboard tick edits in place. Cleared when
    -- the channel changes, and re-posted when an edit finds it deleted.
    leaderboard_message_id  text,
    updated_at              timestamptz not null default now()
);

-- players is every identity the server has ever reported, linked or not.
-- Unlinked players still accumulate stats and still appear on the leaderboard,
-- which is what makes the board a real server ranking on its first day rather
-- than an empty list waiting for people to run /link.
create table players (
    alderon_id      text primary key,
    last_known_name text        not null,
    first_seen_at   timestamptz not null default now(),
    last_seen_at    timestamptz not null default now(),
    -- No DEFAULT: the starting rating is rating.initial in the configuration,
    -- and a default here would be a second, silently diverging copy of it.
    rating          double precision not null,
    -- rated_games counts only CREDITED kills and rated losses, which is what
    -- selects the K factor. It is not kills + deaths: an environmental death
    -- moves neither the rating nor this counter.
    rated_games     integer     not null default 0,
    kills           integer     not null default 0,
    deaths          integer     not null default 0,
    -- decayed_at is how far inactivity decay has been applied, so a decay pass
    -- is idempotent and a missed day is not lost.
    decayed_at      timestamptz not null default now(),

    constraint players_counts_non_negative
        check (rated_games >= 0 and kills >= 0 and deaths >= 0)
);

-- The leaderboard reads this ordering directly.
create index players_ranking on players (rating desc, kills desc)
    where kills > 0 or deaths > 0;

-- player_links is strictly 1:1 in both directions, and it is the DATABASE that
-- says so: primary key one way, unique the other. Application logic checking
-- "is this taken?" before inserting would still race two concurrent /link
-- confirms; these two constraints cannot.
create table player_links (
    discord_user_id text primary key,
    alderon_id      text not null unique references players (alderon_id),
    linked_at       timestamptz not null default now()
);

-- link_challenges is a pending /link, one per Discord user and one per AGID.
--
-- code_hash is a SHA-256, never the code itself. The plaintext exists only in
-- the message the game delivers to the player: a code readable from this table
-- would let anyone with database access claim any in-game identity, which is
-- exactly what the challenge is supposed to prove they cannot do.
--
-- No foreign key to players: the whole point is that this AGID may be someone
-- the bot has never seen. The players row is created when the link is
-- confirmed.
create table link_challenges (
    discord_user_id text primary key,
    alderon_id      text        not null unique,
    -- Snapshotted when the challenge is issued, so confirming does not need the
    -- player to still be online. Requiring them to stay logged in while they
    -- alt-tab to Discord would fail the link for the most ordinary reason
    -- imaginable.
    player_name     text        not null,
    code_hash       bytea       not null,
    attempts        integer     not null default 0,
    created_at      timestamptz not null default now(),
    expires_at      timestamptz not null
);

create index link_challenges_expiry on link_challenges (expires_at);

-- kill_events is the append-only ingest queue.
--
-- The ingest endpoint may ONLY insert here. Elo is order-dependent — the same
-- kills applied in a different order give different ratings — so ratings are
-- computed by a single writer walking this table in id order. id is therefore
-- not just a key, it is the ordering authority, and any replay or backfill has
-- to respect it.
create table kill_events (
    id           bigserial   primary key,
    received_at  timestamptz not null default now(),

    -- Path of Titans sends no event id, so this is a SHA-256 of the payload
    -- bytes. It protects against a webhook retry at the cost of dropping two
    -- byte-identical kills; the ingest metric counts what it drops so the
    -- trade is observable rather than silent.
    dedupe_key   bytea       not null unique,
    server_guid  text        not null,
    payload      jsonb       not null,

    victim_agid    text   not null,
    victim_name    text   not null,
    victim_dino    text,
    victim_growth  double precision,
    victim_poi     text,

    -- Null for an environmental death: nobody killed them.
    killer_agid     text,
    killer_name     text,
    killer_dino     text,
    killer_growth   double precision,
    killer_is_admin boolean not null default false,

    damage_type  text    not null,

    -- credited is decided at ingest because it is a pure function of the
    -- payload: DT_ATTACK, a killer that is present and is not the victim, and
    -- not an admin. Storing it keeps the applier and the feed from each
    -- re-deriving it, and the raw payload is kept so a rule change can be
    -- replayed against history.
    credited     boolean not null,

    -- counts_death is SEPARATE from credited because three different questions
    -- are being asked of one event:
    --
    --   does it appear in the kill feed?  always -- it happened
    --   does it move Elo?                 only if credited
    --   does it count against K/D?        counts_death
    --
    -- Dying to thirst counts against you: surviving is part of playing. Being
    -- killed by an admin, or by your own hand, does not -- neither says
    -- anything about how you play, and an admin moderating a fight should not
    -- dent the record of whoever they stop.
    counts_death boolean not null,

    -- Two independent progress flags, because rating and posting are two
    -- different workers running at two different speeds.
    rated        boolean not null default false,
    posted       boolean not null default false
);

-- Partial indexes so each worker's "what is left?" query stays small even when
-- the table holds a month of history.
create index kill_events_unrated  on kill_events (id) where not rated;
create index kill_events_unposted on kill_events (id) where not posted;
-- Supports pruning processed events.
create index kill_events_processed on kill_events (received_at) where rated and posted;

create type bank_direction as enum ('deposit', 'withdraw');

-- The states a transfer moves through.
--
--   pending    row exists, no RCON command has been sent
--   in_flight  a command HAS been or is being sent; its outcome is unknown
--   applied    the transfer was confirmed and the balance moved
--   failed     the transfer provably did not happen; nothing moved
--   needs_review  the outcome could not be established, and a human must look
--
-- The distinction between failed and needs_review is the whole point. RCON has
-- no transactions and AddMarks is not idempotent, so a row whose command may or
-- may not have landed can never be retried automatically — retrying would mint
-- currency. Recovery observes; it never re-sends.
create type bank_state as enum ('pending', 'in_flight', 'applied', 'failed', 'needs_review');

create table bank_accounts (
    alderon_id text primary key references players (alderon_id),
    balance    bigint      not null default 0 check (balance >= 0),
    updated_at timestamptz not null default now()
);

create table bank_ledger (
    id              bigserial      primary key,
    alderon_id      text           not null references players (alderon_id),
    -- Snapshotted rather than joined through player_links, so the ledger stays
    -- readable after someone unlinks.
    discord_user_id text           not null,
    direction       bank_direction not null,
    -- What was asked for, after clamping to what the player and the bank held.
    amount          bigint         not null check (amount > 0),
    -- What the GAME says actually moved, which is not always the same: it
    -- clamps a removal at zero, so a player who spends marks between the
    -- balance read and the command moves less than was asked. Crediting
    -- `amount` in that case would create marks out of nothing.
    moved           bigint         check (moved >= 0),
    state           bank_state     not null,

    -- The marks reading taken immediately before the command, and the one
    -- taken to confirm it. Together they are the evidence for whether the
    -- transfer happened.
    marks_before    bigint,
    marks_after     bigint,

    -- Lets the worker that resolves the row edit the reply the user is looking
    -- at. Discord expires these after 15 minutes, after which the row itself
    -- is the only record.
    interaction_token text,

    verify_attempts integer     not null default 0,
    created_at      timestamptz not null default now(),
    sent_at         timestamptz,
    resolved_at     timestamptz,
    error           text
);

-- THIS INDEX IS THE "one operation at a time" GUARANTEE. It is a database
-- constraint rather than a process-local mutex precisely because obsidibot
-- runs as several replicas: a mutex would let two of them read the same marks
-- balance and both act on it.
create unique index bank_ledger_one_inflight on bank_ledger (alderon_id)
    where state in ('pending', 'in_flight');

-- Drives the per-user cooldown (max(created_at)) and the reconciler's sweep.
create index bank_ledger_recent on bank_ledger (alderon_id, created_at desc);
create index bank_ledger_unresolved on bank_ledger (created_at)
    where state in ('pending', 'in_flight');

commit;
