-- Correct the kill rules, and set up the replay that applies them to history.
--
-- Two assumptions in 0001 did not survive contact with the live server:
--
--   1. "an admin's kill is moderation" -- the game has a SEPARATE admin kill
--      function that names no killer, so an event naming an admin is an
--      ordinary kill. 22 of the first 48 events were discarded by this.
--   2. "an environmental death names no killer" -- it names the VICTIM as
--      their own killer, so the self-kill guard swallowed every thirst, fall
--      and impact death. 29 deaths in total went unrecorded.
--
-- NOTE FOR READERS OF 0001: the comment block above kill_events.counts_death
-- in 0001_initial_schema.sql is now WRONG. Applied migrations are immutable so
-- it cannot be corrected in place; internal/ingest/payload.go is the authority.

begin;

-- Rows 1 and 2 were ingested 44 minutes before 0002 added these columns, so
-- they hold backfill defaults that contradict their own payload. Repaired from
-- the payload while it is still there -- the pruner below now blanks it.
--
-- kill_distance stays NULL on the no-killer rows: that is the -1 "not
-- applicable" sentinel the ingest endpoint drops on purpose, not a gap.
update kill_events
   set victim_is_admin  = (payload->>'VictimIsAdmin')::boolean,
       victim_character = nullif(payload->>'DinosaurVictimName', ''),
       victim_role      = nullif(payload->>'VictimRole', ''),
       time_of_day      = nullif((payload->>'TimeOfDay')::int, 0)
 where payload is not null
   and (victim_is_admin is distinct from (payload->>'VictimIsAdmin')::boolean
        or (victim_character is null and nullif(payload->>'DinosaurVictimName', '') is not null)
        or (victim_role is null and nullif(payload->>'VictimRole', '') is not null)
        or (time_of_day is null and nullif((payload->>'TimeOfDay')::int, 0) is not null));

-- Bring the stored decisions in line with the new rules.
--
-- NOTHING READS THESE FOR CORRECTNESS. Both workers derive the answer from
-- damage_type/killer_agid/victim_agid at the point of use (see
-- internal/kills/rules.go), because a rolling deploy leaves an old binary
-- ingesting under the old rules for a minute or two and its rows must not be
-- taken at their word. These columns are a record of what the bot believed
-- when the row arrived, kept honest here so a human reading the table is not
-- misled.
update kill_events
   set credited = (damage_type = 'DT_ATTACK'
                   and killer_agid is not null
                   and btrim(killer_agid) <> ''
                   and btrim(killer_agid) <> btrim(victim_agid)),
       counts_death = true;

-- The Elo each side moved, so the feed and /stats can show it. Nullable
-- because an uncredited event moves nothing, and because every existing row is
-- backfilled by the replay rather than by this migration -- only the applier
-- knows the chain.
alter table kill_events
    add column killer_rating_before double precision,
    add column killer_rating_after  double precision,
    add column victim_rating_before double precision,
    add column victim_rating_after  double precision;

-- /stats now shows a player's whole history, so events are kept forever and
-- only the raw payload is aged out. Nothing replays from the payload -- the
-- derivation above uses columns -- so dropping it costs forensics, not
-- correctness.
alter table kill_events alter column payload drop not null;

-- A player's timeline is "killer_agid = me or victim_agid = me", newest first.
-- Neither column was indexed, because until now nothing queried by player.
create index kill_events_by_killer on kill_events (killer_agid, id desc)
    where killer_agid is not null;
create index kill_events_by_victim on kill_events (victim_agid, id desc);

-- The replay is requested here and performed by the rating applier, because the
-- reset needs rating.initial -- which lives in configuration and cannot be
-- known from SQL.
--
-- min_event_id is the PRUNE INTERLOCK. A replay rebuilds every aggregate from
-- the surviving events, so running one after history has been deleted silently
-- produces lower kills and a wrong Elo chain. The applier refuses if the oldest
-- surviving event is newer than this.
create table rating_replays (
    id           bigserial primary key,
    reason       text        not null,
    requested_at timestamptz not null default now(),
    completed_at timestamptz,
    min_event_id bigint
);

insert into rating_replays (reason, min_event_id)
values ('admin kills count, and environmental deaths count as deaths',
        (select min(id) from kill_events));

commit;
