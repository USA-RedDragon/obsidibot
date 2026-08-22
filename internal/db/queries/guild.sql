-- name: GetGuildConfig :one
select * from guild_config where guild_id = $1;

-- name: SetKillFeedChannel :exec
insert into guild_config (guild_id, kill_feed_channel_id, updated_at)
values ($1, $2, now())
on conflict (guild_id) do update
    set kill_feed_channel_id = excluded.kill_feed_channel_id,
        updated_at           = now();

-- SetLeaderboardChannel clears the message id in the same statement that moves
-- the channel. The stored id names a message in the OLD channel, and an edit
-- against it would keep updating a board nobody is reading.
-- name: SetLeaderboardChannel :exec
insert into guild_config (guild_id, leaderboard_channel_id, leaderboard_message_id, updated_at)
values ($1, $2, null, now())
on conflict (guild_id) do update
    set leaderboard_channel_id = excluded.leaderboard_channel_id,
        leaderboard_message_id = null,
        updated_at             = now();

-- name: SetLeaderboardMessage :exec
update guild_config
   set leaderboard_message_id = $2,
       updated_at             = now()
 where guild_id = $1;

-- name: SetModRole :exec
insert into guild_config (guild_id, mod_role_id, updated_at)
values ($1, $2, now())
on conflict (guild_id) do update
    set mod_role_id = excluded.mod_role_id,
        updated_at  = now();

-- name: SetBanFeedChannel :exec
insert into guild_config (guild_id, ban_feed_channel_id, updated_at)
values ($1, $2, now())
on conflict (guild_id) do update
    set ban_feed_channel_id = excluded.ban_feed_channel_id,
        updated_at          = now();

-- name: SetWarnFeedChannel :exec
insert into guild_config (guild_id, warn_feed_channel_id, updated_at)
values ($1, $2, now())
on conflict (guild_id) do update
    set warn_feed_channel_id = excluded.warn_feed_channel_id,
        updated_at           = now();
