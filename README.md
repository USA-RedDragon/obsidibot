# obsidibot

[![Release](https://github.com/USA-RedDragon/obsidibot/actions/workflows/release.yaml/badge.svg)](https://github.com/USA-RedDragon/obsidibot/actions/workflows/release.yaml) [![License](https://badgen.net/github/license/USA-RedDragon/obsidibot)](https://github.com/USA-RedDragon/obsidibot/blob/main/LICENSE) [![Version](https://img.shields.io/github/release/USA-RedDragon/obsidibot.svg)](https://github.com/USA-RedDragon/obsidibot/releases/) [![coverage](https://raw.githubusercontent.com/USA-RedDragon/obsidibot/main/.github/badges/coverage.svg)](https://github.com/USA-RedDragon/obsidibot/actions)

Discord bot for the **Obsidian Wilds** Path of Titans server.

It links Discord accounts to in-game identities, tracks kills into an Elo rating
and a live leaderboard, banks marks on players' behalf over RCON, answers `!`
commands typed in game chat, and runs warnings and time-limited game bans for
moderators.

## What it does

- **`/link`** — binds a Discord account to an Alderon ID by whispering a
  one-time code **into the game**. The code never appears in Discord, and only
  its SHA-256 is stored, so neither database access nor a leaked reply lets
  anyone claim somebody else's identity. Works from either end: `/link start`
  in Discord, or `!link` typed in game chat.
- **Kill tracking** — ingests the game's `PlayerKilled` webhook, keeps per-player
  kills, deaths and an Elo rating, posts a kill feed, and maintains a persistent
  top-20 leaderboard message that is edited in place.
- **Banking** — `/deposit` and `/withdraw` move marks between the dinosaur a
  player is currently controlling and a Discord-side balance. The same
  operations work as `!deposit`/`!withdraw`/`!balance` typed in game chat, with
  replies whispered back — no link required, because the game itself vouches
  for who is typing.
- **Moderation** — role-gated `/warn` and `/ban` (with `1d3h43m`-style
  durations) recorded against a player's identity, enforced in game over RCON,
  posted to warn/ban feed channels, and lifted automatically when they expire.

## How it is put together

Every replica is identical and stateless. Discord delivers slash commands over
**HTTP** rather than a gateway, so scaling is `replicas: N` with no shard
assignment and no session state. The jobs that must have a single writer — the
Elo applier above all, because Elo is order-dependent — coordinate through
Postgres advisory locks, so exactly one replica runs each at a time and failover
is automatic.

### Four listeners, and why they are separate

| Listener | Default port | Routes | Exposure |
| --- | --- | --- | --- |
| `interactions` | 8080 | `POST /`, `GET /healthz`, `GET /readyz` | **Public.** Discord posts signed interactions here |
| `ingest` | 8081 | `POST /webhooks/pot/<secret>/killed`, `POST /webhooks/pot/<secret>/command` | **Cluster-internal only.** The game server posts webhooks here |
| `metrics` | 9090 | `GET /metrics` | Internal |
| `pprof` | 6060 | `/debug/pprof/...` | Internal, disabled by default |

Interactions and ingest are deliberately **separate ports rather than two paths
on one server**. Path of Titans signs nothing and sends no configurable headers,
so the ingest endpoint's only credential is a secret in its URL. Splitting the
ports lets an ingress publish the interactions port alone, so a forged kill event
has to originate inside the cluster before the secret is even the question.

**Do not publish the ingest port.** The bot refuses to start if any two enabled
listeners share a port, but it cannot tell whether your ingress is pointed at the
right one.

## Discord setup

### 1. Create the application

At <https://discord.com/developers/applications>, hit **New Application**.

From **General Information**, copy the **Application ID** and the **Public Key**
— those become `discord.applicationId` and `discord.publicKey`.

### 2. Create the bot user

Under **Bot**, hit **Reset Token** and copy it. That is `discord.token`, and it
is shown once.

No privileged gateway intents are needed. obsidibot never opens a gateway
connection; it only makes REST calls to post the feed and the leaderboard, and
answers interactions over HTTP. Leave **Server Members Intent** and **Message
Content Intent** off.

### 3. Invite it to the server

Because the bot serves one guild and works out which from its own membership,
**leave the application non-public** (Bot → uncheck *Public Bot*). Nobody else
can then invite it, and there is never a second guild for startup to be
ambiguous about.

Under **OAuth2 → URL Generator**, select:

- **Scopes**: `bot` and `applications.commands`
- **Bot Permissions**: **View Channel**, **Send Messages**, **Embed Links**

That is permission bitfield **19456**, and the invite URL is:

```
https://discord.com/api/oauth2/authorize?client_id=<APPLICATION_ID>&scope=bot%20applications.commands&permissions=19456
```

The bot needs those three permissions **in the kill feed and leaderboard
channels specifically** — channel overrides win over server-wide grants, so a
private channel needs the bot added to it.

Nothing else is required. It never deletes messages, never reads history, and
never needs Manage Server for itself — that permission is checked on the
*caller* of `/config`, not held by the bot.

### 4. Point Discord at the interactions endpoint

This is the step that is easy to miss, and the bot does nothing until it is done.

Back in **General Information**, set **Interactions Endpoint URL** to the public
HTTPS address of the `interactions` listener, at the **root path**:

```
https://obsidibot.example.com/
```

Discord will immediately send a signed PING and **refuse to save the URL** if
verification fails. That refusal is a useful test in itself: if it saves, your
`discord.publicKey` is correct and the request is reaching the right port.

obsidibot must already be running and reachable when you save this.

### 5. Commands register themselves

On startup one replica registers the command set into the guild it discovered,
as a bulk overwrite. Guild-scoped registration applies immediately, where global
registration takes about an hour. Commands removed from a release disappear from
Discord on the next start rather than lingering as registrations that route
nowhere.

You do not need to register anything by hand.

### 6. Choose the channels, in Discord

Once the bot is up, someone with **Manage Server** runs:

```
/config kill-channel        #kill-feed
/config leaderboard-channel #leaderboard
/config ban-channel         #ban-feed
/config warn-channel        #warn-feed
/config mod-role            @Moderators
/config show
```

These live in the database, not in obsidibot's config file, so a moderator can
move the feed without a redeploy. Changing the leaderboard channel makes the bot
post a fresh message there within one refresh interval.

`mod-role` is who may run `/warn`, `/ban`, `/unban` and `/modstats`. Anyone
with **Manage Server** always may — that is the bootstrap, or nobody could set
the role in the first place — but `/config` itself stays Manage Server only, so
holding the mod role does not let someone move the gate. The ban and warn feeds
are optional: unset, the actions still happen and are still recorded, they are
just not announced.

## Game server setup

Two sections of `Game.ini`, at
`PathOfTitans/Saved/Config/LinuxServer/Game.ini`. **Stop the server before
editing it.**

### RCON

```ini
[SourceRCON]
bEnabled=true
Password=<a long random password>
Port=7779
```

RCON is how obsidibot reads marks, moves marks, and delivers link codes. Without
it, `/link`, `/deposit` and `/withdraw` do not work; kill tracking still does.

### The kill webhook

```ini
[ServerWebhooks]
bEnabled=True
Format="General"
PlayerKilled="http://obsidibot.example.internal:8081/webhooks/pot/<INGEST_SECRET>/killed"
PlayerCommand="http://obsidibot.example.internal:8081/webhooks/pot/<INGEST_SECRET>/command"
```

`PlayerCommand` is what makes the in-game `!` commands work: the game POSTs
every chat line starting with `!` to that URL (invisibly to other players), and
obsidibot whispers the reply back over RCON. Without it, `!link`, `!deposit`,
`!withdraw` and `!balance` silently do nothing; everything else is unaffected.

**Upgrading from a version without in-game commands? Roll out the new bot to
every replica BEFORE adding the `PlayerCommand` line.** The first `!link`
creates a challenge row no Discord user owns yet, and old replicas cannot read
those rows — their `/link start` breaks on them. No such row can exist until
the game starts sending `PlayerCommand`, so the webhook going in last makes the
ordering safe.

Three things to know:

- **`Format="General"` sends raw JSON.** The default, `"Discord"`, sends a
  channel-ready embed that obsidibot cannot parse.
- **`Format` is a single global setting.** It applies to *every* webhook type at
  once. If you already have another webhook pointed straight at a Discord webhook
  URL — a `Leaderboard` hook, say — switching to `"General"` will start POSTing
  raw JSON at Discord and it will fail. Move or remove those first.
- **`<INGEST_SECRET>` is the value of `ingest.secret`**, and it is the endpoint's
  only credential. Generate it with `openssl rand -hex 32`. It must not contain
  `/`, `?`, `#` or `%`, because it is a URL path segment; the bot refuses to
  start otherwise.

Restart the server after editing.

## Configuration

Settings come from a **config file**, then **environment variables**, then
**flags**, each overriding the last. The default file is `config.yaml`; point
elsewhere with `--config`. A `--config` naming a file that does not exist is a
startup error rather than a silent fall back to defaults.

- Flags are dotted and keep their case: `--discord.token`, `--link.maxAttempts`.
- Environment variables are the section and field, upper-cased, joined with `_`:
  `discord.token` → `DISCORD_TOKEN`, `link.maxAttempts` → `LINK_MAXATTEMPTS`,
  `database.migrateOnStart` → `DATABASE_MIGRATEONSTART`. Top-level keys have no
  prefix: `logLevel` → `LOGLEVEL`.

Everything is validated at startup and **every problem is reported at once**, so
a bad deployment does not have to be fixed one restart at a time.

### Minimal config

```yaml
ingest:
  secret: "<openssl rand -hex 32>"

database:
  url: postgres://obsidibot:password@postgres:5432/obsidibot

discord:
  token: "<bot token>"
  applicationId: "<application id>"
  publicKey: "<public key>"

rcon:
  host: path-of-titans-rcon.path-of-titans.svc.cluster.local
  port: 7779
  password: "<rcon password>"
```

That is the whole file. The guild and the game server's GUID are discovered at
startup — see below — and everything else has a working default.

On boot you should see both resolved:

```
INF discovered the guild to serve guild="Obsidian Wilds" guildId=1234...
INF discovered the game server server="Obsidian Wilds" serverGuid=09466acf-...
```

The server GUID is checked against every inbound webhook, so a second game
server pointed at this URL cannot silently merge its kills into this server's
ratings.

Put `discord.token`, `rcon.password`, `ingest.secret` and `database.url` in a
Secret, not in the file.

### Reference

**Required**: the bot refuses to start without `ingest.secret`, `database.url`,
`discord.token`, `discord.applicationId`, `discord.publicKey` and
`rcon.password`.

The leaderboard is ordered by Elo. Beating a stronger player is worth more;
farming a weaker one is worth almost nothing, and two players trading kills net
out near zero.

<!-- configulator:begin -->

| Key                           | Type    | Default     | Environment                   | Flag                            | Description                                                                                                                              |
|-------------------------------|---------|-------------|-------------------------------|---------------------------------|------------------------------------------------------------------------------------------------------------------------------------------|
| `logLevel`                    | string  | `info`      | `LOGLEVEL`                    | `--logLevel`                    | log verbosity: debug, info, warn, or error                                                                                               |
| `interactions.bind`           | string  |             | `INTERACTIONS_BIND`           | `--interactions.bind`           | address to listen on; empty listens on all interfaces over both IPv4 and IPv6                                                            |
| `interactions.port`           | integer | `8080`      | `INTERACTIONS_PORT`           | `--interactions.port`           | port the Discord interactions endpoint listens on                                                                                        |
| `ingest.bind`                 | string  |             | `INGEST_BIND`                 | `--ingest.bind`                 | address to listen on; empty listens on all interfaces over both IPv4 and IPv6                                                            |
| `ingest.port`                 | integer | `8081`      | `INGEST_PORT`                 | `--ingest.port`                 | port the game webhook endpoint listens on; must NOT be published to the internet                                                         |
| `ingest.secret`               | string  |             | `INGEST_SECRET`               | `--ingest.secret`               | shared secret embedded in the webhook path (required); at least 32 characters, no / ? # or %; generate with openssl rand -hex 32         |
| `metrics.enabled`             | boolean | `true`      | `METRICS_ENABLED`             | `--metrics.enabled`             | serve Prometheus metrics; the health probes are on the interactions listener and unaffected by this                                      |
| `metrics.port`                | integer | `9090`      | `METRICS_PORT`                | `--metrics.port`                | TCP port for the metrics listener                                                                                                        |
| `pprof.enabled`               | boolean | `false`     | `PPROF_ENABLED`               | `--pprof.enabled`               | serve pprof profiling endpoints; /debug/pprof/cmdline prints process arguments, so keep it internal                                      |
| `pprof.port`                  | integer | `6060`      | `PPROF_PORT`                  | `--pprof.port`                  | TCP port for the pprof listener                                                                                                          |
| `database.url`                | string  |             | `DATABASE_URL`                | `--database.url`                | connection URL, e.g. postgres://user:pass@host:5432/obsidibot; psql:// and postgresql:// are accepted too                                |
| `database.migrateOnStart`     | boolean | `true`      | `DATABASE_MIGRATEONSTART`     | `--database.migrateOnStart`     | apply pending schema migrations on startup                                                                                               |
| `database.maxConns`           | integer | `16`        | `DATABASE_MAXCONNS`           | `--database.maxConns`           | maximum PostgreSQL connections this replica's pool may open; must leave room for the background jobs and request traffic at once         |
| `discord.token`               | string  |             | `DISCORD_TOKEN`               | `--discord.token`               | bot token (required), used for the REST calls that post the feed and the board                                                           |
| `discord.applicationId`       | string  |             | `DISCORD_APPLICATIONID`       | `--discord.applicationId`       | Discord application ID (required), used to register commands and edit deferred replies                                                   |
| `discord.publicKey`           | string  |             | `DISCORD_PUBLICKEY`           | `--discord.publicKey`           | Ed25519 public key of the application as hex (required); every interaction is verified against it                                        |
| `rcon.host`                   | string  | `127.0.0.1` | `RCON_HOST`                   | `--rcon.host`                   | hostname or IP of the Source RCON server                                                                                                 |
| `rcon.port`                   | integer | `7779`      | `RCON_PORT`                   | `--rcon.port`                   | TCP port of the Source RCON server                                                                                                       |
| `rcon.password`               | string  |             | `RCON_PASSWORD`               | `--rcon.password`               | RCON password (required)                                                                                                                 |
| `rcon.timeoutSeconds`         | integer | `10`        | `RCON_TIMEOUTSECONDS`         | `--rcon.timeoutSeconds`         | deadline in seconds covering a whole RCON exchange: connect, authenticate, command, response                                             |
| `rcon.maxConcurrent`          | integer | `4`         | `RCON_MAXCONCURRENT`          | `--rcon.maxConcurrent`          | maximum RCON commands in flight at once; further callers fail fast rather than queue                                                     |
| `rating.initial`              | integer | `1200`      | `RATING_INITIAL`              | `--rating.initial`              | rating every player starts at                                                                                                            |
| `rating.provisionalK`         | integer | `40`        | `RATING_PROVISIONALK`         | `--rating.provisionalK`         | K factor while a player has fewer than provisionalGames rated kills                                                                      |
| `rating.settlingK`            | integer | `20`        | `RATING_SETTLINGK`            | `--rating.settlingK`            | K factor between provisionalGames and settlingGames                                                                                      |
| `rating.stableK`              | integer | `16`        | `RATING_STABLEK`              | `--rating.stableK`              | K factor once a player passes settlingGames                                                                                              |
| `rating.provisionalGames`     | integer | `20`        | `RATING_PROVISIONALGAMES`     | `--rating.provisionalGames`     | rated games before K drops from provisionalK to settlingK                                                                                |
| `rating.settlingGames`        | integer | `50`        | `RATING_SETTLINGGAMES`        | `--rating.settlingGames`        | rated games before K drops from settlingK to stableK                                                                                     |
| `rating.decayGraceDays`       | integer | `30`        | `RATING_DECAYGRACEDAYS`       | `--rating.decayGraceDays`       | days a player may be idle before decay begins                                                                                            |
| `rating.decayPermillePerDay`  | integer | `5`         | `RATING_DECAYPERMILLEPERDAY`  | `--rating.decayPermillePerDay`  | thousandths of the gap to initial that an idle rating decays per day past the grace period; only ever pulls a rating down toward initial |
| `bank.cooldownSeconds`        | integer | `10`        | `BANK_COOLDOWNSECONDS`        | `--bank.cooldownSeconds`        | seconds a player must wait between banking operations                                                                                    |
| `bank.verifyAttempts`         | integer | `5`         | `BANK_VERIFYATTEMPTS`         | `--bank.verifyAttempts`         | times to re-read a player's marks trying to confirm an unverified transfer before parking it for review                                  |
| `leaderboard.intervalSeconds` | integer | `60`        | `LEADERBOARD_INTERVALSECONDS` | `--leaderboard.intervalSeconds` | seconds between leaderboard message refreshes; the board and the feed share a channel rate limit, so a shorter tick starves the feed     |
| `leaderboard.size`            | integer | `20`        | `LEADERBOARD_SIZE`            | `--leaderboard.size`            | players listed on the leaderboard                                                                                                        |
| `killfeed.retentionDays`      | integer | `30`        | `KILLFEED_RETENTIONDAYS`      | `--killfeed.retentionDays`      | days to keep the raw webhook payload of a processed kill event; the event itself is kept forever                                         |
| `link.codeTTLSeconds`         | integer | `300`       | `LINK_CODETTLSECONDS`         | `--link.codeTTLSeconds`         | seconds a link code stays valid                                                                                                          |
| `link.maxAttempts`            | integer | `5`         | `LINK_MAXATTEMPTS`            | `--link.maxAttempts`            | wrong codes accepted before a challenge is burned                                                                                        |
| `link.reissueCooldownSeconds` | integer | `30`        | `LINK_REISSUECOOLDOWNSECONDS` | `--link.reissueCooldownSeconds` | seconds before a user may request another link code                                                                                      |

<!-- configulator:end -->

### Two things there is deliberately no setting for

**Which Discord guild to serve**, and **the game server's GUID**. Both are read
at startup from the systems that own them:

| | Read from |
| --- | --- |
| Guild | The guild the bot is in. obsidibot serves one, so there is normally no choice to make |
| Server GUID | The `ServerInfo` RCON command, over the connection that already points at the right server |

They are not configurable because both are identifiers that fail *silently* when
copied wrong, in ways that look like nothing rather than like an error: a
mistyped guild registers commands into a server nobody is watching, and a
mistyped server GUID rejects every kill the game sends, which looks exactly like
a server nobody is playing on. The guild the bot is in and the server RCON
points at are the right answers by construction, so there is nothing to get
wrong.

Both failures are fatal at startup, deliberately. Serving Discord without
knowing the guild would post nowhere, and accepting webhooks without the GUID
would mean rejecting real kills and losing them.

**Being in two or more guilds is a startup error**, naming them, rather than a
guess — picking one arbitrarily would register commands into a server at random.
Keep the application non-public and this cannot arise.

## Commands

| Command | Who | What |
| --- | --- | --- |
| `/link start player:<AGID or name>` | anyone | Whispers a code to that character in game. You must be logged in |
| `/link confirm code:<code>` | anyone | Completes the link |
| `/link status` | anyone | Shows your current link |
| `/link remove` | anyone | Unlinks. Stats and banked marks are kept and reattach if you link again |
| `/stats [user]` | anyone | Rating, kills, deaths, K/D, last seen, and the last five events that moved the rating. Private to you, with a button for the full history |
| `/deposit [amount]` | linked | Marks from your dinosaur into the bank. Omit the amount for all of it |
| `/withdraw [amount]` | linked | Marks from the bank onto your dinosaur |
| `/balance` | linked | What you have banked |
| `/warn reason:<text> user:@x \| player:<AGID or name>` | mod role / Manage Server | Records a warning, whispers the target if online, posts to the warn feed |
| `/ban reason:<text> user:@x \| player:<AGID or name> [duration:1d3h43m]` | mod role / Manage Server | Records a ban, kicks + bans in game, posts to the ban feed. No duration = permanent |
| `/unban user:@x \| player:<AGID or name> [reason]` | mod role / Manage Server | Lifts the game ban, then closes the record |
| `/modstats user:@x \| player:<AGID or name>` | mod role / Manage Server | Counts, active ban and recent history for one player |
| `/config kill-channel <channel>` | Manage Server | |
| `/config leaderboard-channel <channel>` | Manage Server | |
| `/config ban-channel <channel>` | Manage Server | |
| `/config warn-channel <channel>` | Manage Server | |
| `/config mod-role <role>` | Manage Server | |
| `/config show` | Manage Server | |

`/warn` and `/ban` target **either** a Discord user **or** an in-game identity
— exactly one. A player warned by Alderon ID before linking and by @mention
after is one person with one record; the two halves merge through the link.
Banning an unlinked @user is recorded but cannot reach the game until they
link; the reply says so, and enforcement happens automatically the moment a
link appears.

Banking requires being **in game**: marks live on the character you are
controlling, so there is nothing to read or move while you are logged out.

### In game

Typed in game chat; replies arrive as whispers only you can see. Other players
never see the command either — the game does not broadcast `!` lines.

| Command | What |
| --- | --- |
| `!link` | Whispers a link code for this character. Bring it to `/link confirm` in Discord. Do not share it — whoever enters it gets the link |
| `!deposit [amount]` | Marks from this dinosaur into the bank. Omit the amount, or say `all`, for everything |
| `!withdraw [amount]` | Marks from the bank onto this dinosaur |
| `!balance` | Banked balance, and this dinosaur's marks |
| `!help` | The list above, whispered |

No link is needed for in-game banking: the game itself vouches for who is
typing, and the balance lives against the Alderon ID either way. Whatever you
bank before linking is already yours when you later link.

### What counts

One kill event answers three separate questions:

| | shown in the feed | moves Elo | counts toward K/D |
| --- | --- | --- | --- |
| Player kill (`DT_ATTACK`) | yes | yes | yes |
| Killed by an admin | yes | **yes** | **yes** |
| Environmental death (thirst, hunger, drowning, falls) | yes | no | yes |
| Killed by a player, non-attack damage (a trample, say) | yes | no | yes |

**Every event is a death**, because the webhook fires when a player dies. Dying
of thirst counts against you because surviving is part of playing, but it moves
no rating: there is no counterparty to take the points, and inventing one would
drain the pool and deflate every rating over time.

**An admin's kill counts like anybody else's.** The game has a *separate* admin
kill function that names no killer at all, so an event that does name an admin
is an admin playing the game — not moderating it. Discarding those was
discarding half the recorded history.

Two things the game does that are worth knowing, because they are not what the
names suggest:

- **An environmental death names the victim as their own killer**, with a kill
  distance of zero. It is not a suicide, and the feed reads it as "died of
  thirst" rather than "X killed X".
- **Fall damage arrives as `DT_IMPACT` or `DT_BREAKLEGS`**; `DT_GENERIC` with
  no killer at all is something else again and is still being identified.

The leaderboard lists **everyone**, linked or not — unlinked players appear under
their in-game name — so the board ranks the server rather than the subset of it
that uses the bot.

## Operations

### The kill feed

Every field the game's `PlayerKilled` webhook reports is rendered, laid out as
three inline columns (killer, victim, circumstances) rather than the twenty
stacked lines the game's own Discord webhook posts. That includes both parties'
coordinates and the point of interest — there is no flag, because the feed
describes a fight that has already happened.

`KillDistance` arrives in Unreal units and is rendered in metres. `TimeOfDay` is
hundredths of an hour rather than HHMM, so 1489 is 14:53 — read the other way it
produces times like "17:79".

Each side of a rated kill shows **what it did to their rating** — `1200.0 →
1212.4 (+12.4)` — so the number on the leaderboard can be taken apart by anyone
who wants to. An event that moved nothing shows nothing rather than "+0.0".

The feed **waits for the rating to be computed** before posting, since it
reports that rating. A stalled rating applier therefore also stalls the feed;
nothing is dropped, because the queue is durable.

**The bot needs View Channel, Send Messages and Embed Links in the feed
channel.** Channel permission overrides beat server-wide grants, so a read-only
announcement channel needs an override for obsidibot's role specifically —
leaving `@everyone` denied, which keeps the channel read-only for members. If it
cannot post, it says so once every five minutes and keeps the backlog rather
than retrying every second; kills are never dropped and appear as soon as the
permission is granted.

### Kill history and retention

Kill events are kept **for the life of the server**. Only the raw webhook
payload ages out, after `killfeed.retentionDays` (30 by default) — a slim row is
a few hundred bytes, so a busy server costs a megabyte or two a year.

That is deliberate, and it buys two things:

- **`/stats` can show a player their whole history**, which is what makes a
  rating arguable rather than merely asserted.
- **A rule change can be replayed against all of it.** The rules have been wrong
  twice now; both times the fix was to recompute every rating from the events in
  order, which is only possible while the events exist.

A replay is requested by a migration and performed by the rating applier, in one
transaction: every aggregate is reset, every event re-rated in id order, and the
whole thing commits at once — so nobody ever sees a half-rebuilt leaderboard.
It **refuses to run** if any event it needs has been deleted, because a replay
over a partial history produces a wrong leaderboard that looks completely
ordinary.

### Moderation and bans

The database row is the ban; the game is brought into line with it. A
scheduler on one replica enforces recorded bans (kick, then ban — the ban
alone blocks rejoining, the kick is what shows the player the reason), lifts
them when they expire, and re-asserts every active ban hourly so a wiped or
restored `Bans.txt` heals itself.

Four things worth knowing:

- **The game's own timed bans are never used.** `Ban <id> <time>` writes a
  corrupt `Bans.txt` row that binds nobody and cannot be lifted — verified
  against the live server. obsidibot always issues permanent game bans and
  owns the expiry itself, which is also why a ban survives the server's
  periodic restarts.
- **The game refuses to ban a server admin** (`ServerAdmins` in `Game.ini`),
  and no RCON command changes that. Such a ban is recorded, marked
  unenforceable, shown as such in `/modstats`, and never uselessly retried.
  To enforce it: remove them from `ServerAdmins`, restart, and `/unban` +
  re-`/ban` (or wait for the hourly re-assertion to pick the row up after
  clearing the flag).
- **The one unliftable edge:** a `Bans.txt` row for someone currently in
  `ServerAdmins` (hand-edited file, or banned then promoted) locks them out,
  but RCON can neither place nor lift it. If an expiry closes with lift reason
  `expired; game reported no liftable ban` and the player is still locked out,
  edit `Bans.txt` by hand — remove their line — and run `ReloadBans`.
- **A banned player cannot be in game**, so their `!` commands and Discord
  `/deposit`/`/withdraw` all fail with "you need to be logged in", while
  `/balance` still answers. That is coherent, not a bug: their marks are
  safe and waiting.

### Probes

`/healthz` and `/readyz` are on the **interactions listener (8080)**, and
nowhere else. Point Kubernetes probes there.

Two consequences of that placement, both deliberate:

- The interactions listener is the one that always exists and the one Discord
  actually talks to. Probing a port that `metrics.enabled: false` can switch
  off, to learn whether a *different* port is serving, would be misleading in
  both directions.
- `/readyz` returns **the reason in the body**, so a failing probe says what is
  wrong without a log dive.

**Route only `/` to this listener.** With an exact-path match, the sole path
reaching it from outside is Discord's `POST /`, and the two health paths are
never exposed — the kubelet reaches them on the pod IP, which does not traverse
the ingress. For example:

```yaml
matches:
  - path: {type: Exact, value: /}
```

This matters because `/readyz` echoes the underlying error, which on a database
outage reads like ``database: failed to connect to `user=obsidibot
database=obsidibot`: 10.0.0.5:5432 … connection refused``. Behind an exact match
that is a useful probe; behind a prefix match it is internal hostnames served to
the internet. If you ever widen the route, drop the body and keep the status
code — it is one line in `internal/interactions`.

`/healthz` claims only that the process is up and accepting; it looks nothing
up, because a liveness probe that fails on a dependency gets the container
restarted for an outage restarting cannot fix. `/readyz` pings the database,
without which this replica cannot answer a single command.

### Metrics

Metrics are on `/metrics` (9090) with an `obsidibot_` prefix. Two are worth
alerting on:

- **`obsidibot_bank_needs_review` — alert on nonzero.** RCON has no transactions
  and `AddMarks` is not idempotent, so a transfer whose outcome cannot be
  established is parked rather than guessed at or retried. This is the only path
  by which a balance can be wrong, and nothing else will surface it. Inspect with
  `select * from bank_ledger where state = 'needs_review'`.
- **`obsidibot_kill_feed_backlog`** — the feed is lossless, so a Discord outage
  makes this grow rather than dropping kills. Sustained growth means it is not
  draining.
- **`obsidibot_moderation_unenforced_bans` — alert on sustained nonzero.** An
  active ban the game is not yet enforcing: the target has never linked, RCON
  is failing, or the scheduler is behind. A banned player who can still join
  is invisible otherwise. (Bans already flagged unenforceable — admin targets
  — are excluded; those are surfaced in `/modstats` instead, because a gauge
  that is permanently red trains people to ignore it.)

Also useful: `obsidibot_leaderboard_last_success_timestamp_seconds` for
staleness, `obsidibot_rcon_commands_total{command,result}`,
`obsidibot_game_commands_total{command,result}` for the in-game `!` commands,
and `obsidibot_moderation_actions_total{kind,result}`.

No metric is ever labelled with a Discord user id, an Alderon ID or a player
name. Player churn would otherwise become an unbounded set of time series.

## Development

```bash
go build ./... && go test ./...

# Integration tests need a database and SKIP without one, so run them with it:
docker run -d --rm --name obsidibot-pg \
  -e POSTGRES_PASSWORD=test -e POSTGRES_DB=obsidibot \
  -p 5432:5432 postgres:18-alpine
TEST_DATABASE_URL=postgres://postgres:test@127.0.0.1:5432/obsidibot go test ./... -race

# After changing internal/db/queries or schema/migrations. Generated code is
# committed and CI checks that regenerating is a no-op:
go run github.com/sqlc-dev/sqlc/cmd/sqlc@v1.31.1 generate
```

Each test package gets its own Postgres schema, so `go test ./...` can run them
concurrently against one database.

The schema lives in `schema/migrations`, is embedded into the binary, and is
applied on startup under an advisory lock — so a rolling deploy is safe and
there is no separate migration step.
