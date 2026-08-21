# obsidibot

[![Release](https://github.com/USA-RedDragon/obsidibot/actions/workflows/release.yaml/badge.svg)](https://github.com/USA-RedDragon/obsidibot/actions/workflows/release.yaml) [![License](https://badgen.net/github/license/USA-RedDragon/obsidibot)](https://github.com/USA-RedDragon/obsidibot/blob/main/LICENSE) [![Version](https://img.shields.io/github/release/USA-RedDragon/obsidibot.svg)](https://github.com/USA-RedDragon/obsidibot/releases/) [![Coverage](.github/badges/coverage.svg)](https://github.com/USA-RedDragon/obsidibot/actions/workflows/test.yaml)

Discord bot for the **Obsidian Wilds** Path of Titans server.

It links Discord accounts to in-game identities, tracks kills into an Elo rating
and a live leaderboard, and banks marks on players' behalf over RCON.

## What it does

- **`/link`** — binds a Discord account to an Alderon ID by whispering a
  one-time code **into the game**. The code never appears in Discord, and only
  its SHA-256 is stored, so neither database access nor a leaked reply lets
  anyone claim somebody else's identity.
- **Kill tracking** — ingests the game's `PlayerKilled` webhook, keeps per-player
  kills, deaths and an Elo rating, posts a kill feed, and maintains a persistent
  top-20 leaderboard message that is edited in place.
- **Banking** — `/deposit` and `/withdraw` move marks between the dinosaur a
  player is currently controlling and a Discord-side balance.

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
| `ingest` | 8081 | `POST /webhooks/pot/<secret>/killed` | **Cluster-internal only.** The game server posts webhooks here |
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
/config show
```

These live in the database, not in obsidibot's config file, so a moderator can
move the feed without a redeploy. Changing the leaderboard channel makes the bot
post a fresh message there within one refresh interval.

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
```

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

**Required** — the bot refuses to start without these:

| Key | Env | Description |
| --- | --- | --- |
| `ingest.secret` | `INGEST_SECRET` | Shared secret in the webhook path; ≥32 chars, no `/?#%` |
| `database.url` | `DATABASE_URL` | PostgreSQL URL; `psql://` and `postgresql://` are accepted too |
| `discord.token` | `DISCORD_TOKEN` | Bot token |
| `discord.applicationId` | `DISCORD_APPLICATIONID` | Application ID |
| `discord.publicKey` | `DISCORD_PUBLICKEY` | Ed25519 public key, 64 hex characters |
| `rcon.password` | `RCON_PASSWORD` | RCON password |

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

**Listeners:**

| Key | Env | Default | Description |
| --- | --- | --- | --- |
| `logLevel` | `LOGLEVEL` | `info` | `debug`, `info`, `warn` or `error` |
| `interactions.bind` | `INTERACTIONS_BIND` | *(all)* | Empty binds every interface, IPv4 and IPv6 |
| `interactions.port` | `INTERACTIONS_PORT` | `8080` | Public; Discord posts here |
| `ingest.bind` | `INGEST_BIND` | *(all)* | |
| `ingest.port` | `INGEST_PORT` | `8081` | **Do not publish this port** |
| `metrics.enabled` | `METRICS_ENABLED` | `true` | Serves `/metrics`. Health probes are on 8080 and are unaffected by this |
| `metrics.port` | `METRICS_PORT` | `9090` | |
| `pprof.enabled` | `PPROF_ENABLED` | `false` | `/debug/pprof/cmdline` prints process arguments — keep it internal |
| `pprof.port` | `PPROF_PORT` | `6060` | |

**Game connection:**

| Key | Env | Default | Description |
| --- | --- | --- | --- |
| `rcon.host` | `RCON_HOST` | `127.0.0.1` | |
| `rcon.port` | `RCON_PORT` | `7779` | |
| `rcon.timeoutSeconds` | `RCON_TIMEOUTSECONDS` | `10` | Covers a whole exchange: connect, authenticate, command, response |
| `rcon.maxConcurrent` | `RCON_MAXCONCURRENT` | `4` | The game handles RCON on its game thread; keep this small |
| `database.migrateOnStart` | `DATABASE_MIGRATEONSTART` | `true` | Migrations are serialised by an advisory lock, so every replica may run them |
| `database.maxConns` | `DATABASE_MAXCONNS` | `16` | Pool size per replica. Set explicitly because pgx's own default is `max(4, NumCPU)` — the same image would then run a comfortable pool on a large node and a four-connection pool on a small one. **Startup refuses** if this leaves no room for the background jobs plus request traffic |

**Rating** — the leaderboard is ordered by Elo. Beating a stronger player is
worth more; farming a weaker one is worth almost nothing, and two players
trading kills net out near zero.

| Key | Env | Default | Description |
| --- | --- | --- | --- |
| `rating.initial` | `RATING_INITIAL` | `1200` | Starting rating |
| `rating.provisionalK` | `RATING_PROVISIONALK` | `40` | K while under `provisionalGames` |
| `rating.settlingK` | `RATING_SETTLINGK` | `20` | K between the two thresholds |
| `rating.stableK` | `RATING_STABLEK` | `16` | K once past `settlingGames` |
| `rating.provisionalGames` | `RATING_PROVISIONALGAMES` | `20` | |
| `rating.settlingGames` | `RATING_SETTLINGGAMES` | `50` | |
| `rating.decayGraceDays` | `RATING_DECAYGRACEDAYS` | `30` | Idle days before decay begins |
| `rating.decayPermillePerDay` | `RATING_DECAYPERMILLEPERDAY` | `5` | Thousandths of the gap to `initial` per idle day. Only ever pulls a rating *down* toward the baseline |

**Feed, board and banking:**

| Key | Env | Default | Description |
| --- | --- | --- | --- |
| `killfeed.retentionDays` | `KILLFEED_RETENTIONDAYS` | `30` | Processed events are pruned after this. Player totals are unaffected |
| `leaderboard.intervalSeconds` | `LEADERBOARD_INTERVALSECONDS` | `60` | The board and the feed share a channel rate limit; a shorter tick starves the feed |
| `leaderboard.size` | `LEADERBOARD_SIZE` | `20` | |
| `bank.cooldownSeconds` | `BANK_COOLDOWNSECONDS` | `10` | Between transfers, per player |
| `bank.verifyAttempts` | `BANK_VERIFYATTEMPTS` | `5` | Observation attempts before a transfer is parked for review. The wait between them is the reconciler's own tick |
| `link.codeTTLSeconds` | `LINK_CODETTLSECONDS` | `300` | |
| `link.maxAttempts` | `LINK_MAXATTEMPTS` | `5` | Wrong codes before a challenge is burned |
| `link.reissueCooldownSeconds` | `LINK_REISSUECOOLDOWNSECONDS` | `30` | `/link start` whispers somebody in game; this stops it being a spam button |

## Commands

| Command | Who | What |
| --- | --- | --- |
| `/link start player:<AGID or name>` | anyone | Whispers a code to that character in game. You must be logged in |
| `/link confirm code:<code>` | anyone | Completes the link |
| `/link status` | anyone | Shows your current link |
| `/link remove` | anyone | Unlinks. Stats and banked marks are kept and reattach if you link again |
| `/stats [user]` | anyone | Rating, kills, deaths, K/D and last seen. Public |
| `/deposit [amount]` | linked | Marks from your dinosaur into the bank. Omit the amount for all of it |
| `/withdraw [amount]` | linked | Marks from the bank onto your dinosaur |
| `/balance` | linked | What you have banked |
| `/config kill-channel <channel>` | Manage Server | |
| `/config leaderboard-channel <channel>` | Manage Server | |
| `/config show` | Manage Server | |

Banking requires being **in game**: marks live on the character you are
controlling, so there is nothing to read or move while you are logged out.

### What counts

One kill event answers three separate questions:

| | shown in the feed | moves Elo | counts toward K/D |
| --- | --- | --- | --- |
| Player kill (`DT_ATTACK`) | yes | yes | yes |
| Environmental death (thirst, hunger, drowning, falls) | yes | no | yes |
| Self-kill | yes | no | no |
| Killed by an admin | yes | no | no |

Dying of thirst counts against you because surviving is part of playing, but it
moves no rating: there is no counterparty to take the points, and inventing one
would drain the pool and deflate every rating over time. An admin moderating a
fight should not dent the record of whoever they stop.

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

`KillDistance` arrives in Unreal units and is rendered in metres.

**The bot needs View Channel, Send Messages and Embed Links in the feed
channel.** Channel permission overrides beat server-wide grants, so a read-only
announcement channel needs an override for obsidibot's role specifically —
leaving `@everyone` denied, which keeps the channel read-only for members. If it
cannot post, it says so once every five minutes and keeps the backlog rather
than retrying every second; kills are never dropped and appear as soon as the
permission is granted.

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

Also useful: `obsidibot_leaderboard_last_success_timestamp_seconds` for
staleness, and `obsidibot_rcon_commands_total{command,result}`.

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
