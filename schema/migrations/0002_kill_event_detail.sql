-- Detail the kill feed needs, which 0001 did not capture.
--
-- Two reasons these were missing, both worth recording:
--
-- 1. The LIVE SERVER'S PAYLOAD DIFFERS FROM THE DOCUMENTATION. Alderon's
--    webhook docs name the victim's character `VictimCharacterName`; the server
--    actually sends `DinosaurVictimName`. The killer's side really is
--    `KillerCharacterName`, so the API is asymmetric and 0001 was built against
--    the documented half. The victim's character name has never been captured.
--
-- 2. `KillDistance`, `VictimRole` and `KillerRole` are sent and were not in the
--    documented payload at all.
--
-- kill_distance is in Unreal units (centimetres) -- verified against a real
-- event by recomputing it from the two reported coordinates, which agreed to
-- six significant figures. The feed renders it in metres. The COORDINATES
-- themselves are still never stored outside the raw payload and never
-- rendered; a distance is not a position.

begin;

alter table kill_events
    add column victim_character text,
    add column killer_character text,
    add column victim_role      text,
    add column killer_role      text,
    -- Null when there was no killer. The game reports -1 as its "not
    -- applicable" sentinel, which must not reach a display as "-1 m".
    add column kill_distance    double precision,
    -- The in-world clock, as the game reports it: 1411 is 14:11.
    add column time_of_day      integer,
    -- 0001 stored only the KILLER's admin flag, because only that affected
    -- whether a kill was credited. The feed shows both parties in full.
    add column victim_is_admin  boolean not null default false,
    -- The raw coordinate strings, stored so the feed CAN render them when the
    -- operator turns killfeed.showLocations on. They stay out of every other
    -- surface; see the rule in internal/pot.
    add column victim_location  text,
    add column killer_location  text;

commit;
