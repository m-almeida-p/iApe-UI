# iApe-UI

[![CI](https://github.com/m-almeida-p/iApe-UI/actions/workflows/ci.yml/badge.svg)](https://github.com/m-almeida-p/iApe-UI/actions/workflows/ci.yml)
[![Go](https://img.shields.io/badge/Go-1.27%2B-00ADD8?logo=go&logoColor=white)](https://go.dev/dl/)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](LICENSE)

**iApe-UI** is the web UI of the Yape agent service. It talks to the service's **AG-UI BFF** (`POST /agent`, answered as
server-sent events), so the page streams the agent's replies and draws its cards: choose a contact, approve or reject a
transfer, give the code the bank asks for.

- **Web UI** (`cmd/web`): Go, server-rendered HTML, HTMX and Server-Sent Events, Bootstrap 5 and PostgreSQL (for the users and
  their sessions). Sign-in, the **Historial** (the user's conversations, read from the conversation history), streamed replies, Stop,
  the P2P cards, a light/dark theme; it works well on keyboard and phone. **The chats are not kept here**: see
  [The conversation history](#the-conversation-history).
- **Scripted BFF** (`cmd/test-bff`): a stand-in for the BFF that answers from a script, so the UI can be run and tested
  with no agent, no model and no Yape account.
- **Explorer** (`cmd/iape-explore`): drives the real UI in Chrome through the paths of the P2P flows, with no model in the
  loop, and says whether each ends where it should. See [Explorer](#explorer).

One Go module, `iape-ui` (Go 1.27), built as hexagonal services: `internal/core` knows nothing of HTTP, SQL or browsers, and the
adapters under `internal/infrastructure` plug into its ports.

## Prerequisites

The scripts are bash: macOS or Linux (Windows through WSL).

| Tool | Version | Needed for |
|------|---------|------------|
| [Go](https://go.dev/dl/) | 1.27+ | everything |
| [Docker](https://www.docker.com/) | any recent | PostgreSQL, the scripted BFF, the integration tests |
| [Node.js](https://nodejs.org/) (for `npx`) | 20.19+, 22.12+ or 23+ | the explorer: it starts Chrome DevTools MCP with `npx` |
| Chrome | any recent | the explorer |
| `yape-token` | — | talking to the real BFF: it prints the Yape account's bearer token |
| `make` | any | the [Makefile](#makefile) targets |

`make install` writes your `.env` and builds this project's own copies of the quality tools, since globally installed ones are often
compiled with an older Go than this module and refuse to analyze it.

## Quick start

With the scripted BFF, which needs no Yape account:

```sh
scripts/init-env.sh                   # writes .env with fresh random passwords (set DB_URL as in Database, below)
docker compose up -d --wait agent     # the scripted BFF on :9093
scripts/run.sh                        # the web server from source
open http://localhost:8300            # sign in with ADMIN_EMAIL / ADMIN_PASSWORD from .env
```

Write `quiero yapear`, or `#pay`, `#otp`, `#contact` for a card, `#slow` for a long reply. `.env.example` points at the scripted
BFF (`AGENT_BASE_URL=http://localhost:9093`, `YAPE_TOKEN_BIN=scripts/test-token.sh`).

With the real BFF (`http://localhost:4000`), set in `.env`:

```sh
AGENT_BASE_URL=http://localhost:4000
YAPE_TOKEN_BIN=yape-token                # prints the token of an account and caches it until it expires
HISTORY_BASE_URL=http://localhost:3100   # the conversation history: without it the page has no Historial
YAPE_ACCOUNT_ID=<the account's UUID>     # with ADMIN_EMAIL and ADMIN_PASSWORD: the user of that account, made at start
```

## The conversation history

The history (`ms-ml-conv-history-bs`) is **the only place the chats are kept**. A chat travels agent → event store → Debezium → Kafka →
consumer → history tables → its API, and this program only reads it: the Historial in the side menu, and a conversation's transcript.

- **Whose chats.** The id of a user **is** the id of their Yape account, so a user reads the conversations of their own account and no
  other. The page never shows the account and never calls the history itself. At start, `ADMIN_EMAIL`, `ADMIN_PASSWORD` and
  `YAPE_ACCOUNT_ID` make the user of that account (id = the account), unless a user with that id exists, which is left as it is. If the
  email is another user's, the program does not start: use another email.
- **The id of a chat.** The UI makes it when a chat starts (`conv_v1_` + a UUID v4) and sends it as `threadId` on every turn; the BFF
  passes it through and it is the `conversation_id` of the events and the `_id` of the conversation in the history. See
  [docs/conversation-id.md](docs/conversation-id.md).
- **Continuing.** Opening a conversation shows its transcript; writing in it is a new turn with the same id. A turn lives **in
  memory** while it is written (the replies, with their cards); what it adds reaches the history by itself, about a second later
  (the Historial refreshes once after a reply ends). Writing into a conversation the server does not know is allowed only if the history
  has it for the account of the user.
- **What is not kept.** The cards (Pay, Reject, the code, the contact) are live with their reply: a reload makes them disappear, and a
  server restart forgets the replies in memory (2 hours at most). There is no Regenerate, rename or delete.
- **The old chats.** `conversations` and `messages` in the schema `web_ui` are no longer read or written. Nothing here touches or
  deletes them.
- **Hours.** The Historial shows the hours of `HISTORY_TIMEZONE` (`America/Lima`).
- **No authentication yet.** The history API trusts the account it is asked for (a proof of concept): do not point `HISTORY_BASE_URL`
  at a deployed service. And the agent does not check that a conversation belongs to the account that writes into it: this program
  does, and another client of the BFF does not.

`scripts/stop.sh` (`make stop`) stops the server; `docker compose down` (`make down`) stops PostgreSQL and the scripted BFF.

## Database

**This program creates no table.** `DB_URL` must reach a PostgreSQL that has the tables `users` and `sessions` in the schema `web_ui`
(it keeps no chat), and it carries `search_path=web_ui`, which is how the program finds them. A database without them stops the
program at start. The tables are not made by this repository.

On the Yape cluster (`yape-postgres`, the network `yape-local`) the database is `agentwebui`, and the cluster trusts local
connections, so there is no password:

```sh
DB_URL="postgres://postgres@localhost:5432/agentwebui?sslmode=disable&search_path=web_ui"
```

Every setting is explained in `.env.example`, and a real environment variable always wins over what is in `.env`. The
`.env` that `scripts/init-env.sh` writes has the web server listening on `127.0.0.1:8300`, this computer only
(`APP_HTTP_ADDR`). The page is plain http: before you put this in front of a network, add a TLS proxy and give `APP_BASE_URL`
its https address, which turns on Secure cookies and HSTS (`APP_ENV=production` will not start without it).

## The cards

When the agent's reply carries a card, the page draws it from the reply, which lives in this server's memory (up to 2 hours); a reload makes the
card disappear, because the history keeps none:

| Card | What it asks | Answered with |
|------|--------------|---------------|
| Contact (`CONTACT_REQUEST`) | Who to pay: the candidates are test contacts, always the same for the same name | A button: the contact is confirmed with the BFF and the agent goes on (the page sends `usuario confirmado`) |
| Transfer (`ACTION_REQUEST`) | Pay or reject S/ N to a destination | **Pay** or **Reject**; when the bank asks for a code, a field for it |

The composer's `@` picker offers the same test contacts and puts `@(Name)` in the message. A card only goes forward (paid and rejected are closed), and
what keeps one transfer from being paid twice is the agent's service, which ignores a repeated answer: see `docs/agent-integration-notes.md`.

## Makefile

`make help` lists every target, grouped like the scripts: setup (`install`, `install/env`, `install/tools`, `install/tools/global`),
quality control (`audit`, `audit/it`, `test`, `test/cover`), development (`build`, `up`, `down`, `run`, `stop`, `tidy`) and operations
(`push`, which runs the gate and refuses to push a dirty tree). Each target calls the matching `scripts/*.sh`.

`make install/tools` builds `golangci-lint`, `staticcheck`, `govulncheck`, `modernize` and `mockery` into this repository's own
`bin/tools`, which `scripts/check.sh` adds to `PATH`. `make install/tools/global` overwrites the ones in your global Go bin instead,
which can replace binaries other projects expect at a different version, so it is left out of `make install`.

## Explorer

```sh
make build                            # bin/agent-web-ui and bin/iape-explore
bin/iape-explore -list                # the paths
bin/iape-explore -path pay            # one path
bin/iape-explore -all                 # every safe path, each in a conversation of its own (guard only with -with-guard)
bin/iape-explore -path pay -amount 35 -contact "Maria Torres"
bin/iape-explore -headless -all       # without a window, for a machine that has no screen
bin/iape-explore "Hola" "expect:Hola" "expect-no-card"     # steps of your own
```

`bin/iape-explore -h` says everything below and more: the flags, the paths, the steps, the settings, the records and the exit status.

| Flag | What it does |
|------|--------------|
| `-list` | List the paths and exit |
| `-path ID` | Run one path |
| `-all` | Run every path, each in a conversation of its own (but `guard`) |
| `-with-guard` | With `-all`: also run `guard`, which can get the account banned |
| `-headless` | Run Chrome without a window (the default is a window, to watch the run) |
| `-amount N`, `-contact NAME` | The amount in soles and the contact of a payment |
| `-out DIR` | Where the records go (default `temp/iape-explore/<time>`) |
| `-wait DURATION` | How long a step waits for the page to settle (default 90s) |
| `-env FILE` | The settings file (default `.env`) |

Exit status: 0 every path ended where it should, 1 the call or the settings are wrong, 2 a path failed, 4 Chrome could not be opened.

The paths are `card`, `pay`, `otp`, `otp-tag`, `reject`, `limit`, `guard` and `contact`. A step is a message to send, or one of
`@`, `@pick[:name]`, `type:`, `send`, `click:`, `click?:`, `wait:`, `otp:`, `expect:`, `expect-card:kind=state` and `expect-no-card`
(see `internal/explore`). A run stops at the first step that fails, after recording what the page showed.

After every step it records, under `temp/iape-explore/<time>/<path>/`: the page's accessibility tree, a screenshot, the network
requests and the lines that are new, then `report.md`, and a `summary.md` for the whole run. Nothing it writes holds the password or a
token. It signs in as `EXPLORE_USER_EMAIL`/`EXPLORE_USER_PASSWORD`, or as the first user (`ADMIN_EMAIL`/`ADMIN_PASSWORD`), and starts
Chrome DevTools MCP with `EXPLORE_MCP_COMMAND` and `EXPLORE_MCP_ARGS`: `npx` on a pinned package, with a window unless `-headless` (a fresh, isolated profile either way), and
always with the flags that keep usage statistics off and the profile temporary.

What was found building this against the agent service (the protocol, the mock bank, the ban and how to lift it, the environment) is in
[`docs/agent-integration-notes.md`](docs/agent-integration-notes.md).

**Mind the guardrail.** The agent service bans an account that trips its prompt-injection guard too often: with the settings of
a development `.env` (`CIRCUIT_BREAKER_MAX_BLOCKS_PER_WINDOW`, `CIRCUIT_BREAKER_BAN_DURATION_SECONDS`) two runs of `guard` in a few
minutes can ban the account for a long time, and every later message then answers "Por seguridad, limitamos tu acceso al
asistente". `-all` therefore leaves `guard` out unless you add `-with-guard`, and running it says what it costs. Run it rarely.

## Configuration

**Every setting lives in `.env`** (the programs, the scripts, Docker Compose and the tests), and **`.env.example` documents every
one**, with its default and what it does. `scripts/init-env.sh` copies the example into `.env` with fresh passwords. Here is who reads it:

- The programs read `.env` from the directory they run in. A variable already set in your environment wins over the file, and a bad
  value stops the program at start, listing every problem it found without showing any secret. `agent-web-ui [--env FILE]
  [healthcheck [URL]]` reads FILE (or `.env`); a file that does not exist, an unknown flag, a flag after `healthcheck` or a second
  address makes it exit with status 2 and a usage line. `scripts/run.sh --env FILE` passes the file through.
- The scripts read `.env` (or the file `--env` names, which has to exist) and fill in what is missing from `.env.example`. They do not
  look at your environment (apart from two hooks the tests use). `run.sh` hands the whole file to the server.
- Docker Compose reads `.env` on its own. An empty value counts as unset.

A test fails if a script, a Go file or the Compose file reads a setting that is not documented in `.env.example`. Its sections:
App, Database, Agent, Chat, Rate limiting, Sessions, Docker Compose, Scripted BFF, Explorer, Scripts and tools (the go command, the
bin and log folders, the coverage floors), Pull requests, and Tests (the opt-in switches).

| Key | Default | What it does |
|-----|---------|--------------|
| `DB_URL` | required | PostgreSQL URL (a secret). Percent-encode `@`, `/`, `?` and `#` in its user and password (`%40`, `%2F`, `%3F`, `%23`). A URL the driver cannot read stops the start with the driver's reason |
| `AGENT_BASE_URL` | required | The BFF, such as `http://localhost:4000` |
| `YAPE_ACCOUNT_ID` | with `ADMIN_*` | The Yape account of the first user (a UUID, kept in lower case): the user is made with it as its id. Each request to the BFF is made as the account of the user who sends it |
| `YAPE_TOKEN_BIN` | `yape-token` | The program that prints the bearer token of an account: run for each request, `--account ID`, and `--refresh` after a 401 |
| `HISTORY_BASE_URL` | none | The history API, such as `http://localhost:3100`. Without it there is no Historial (a chat started here still goes on) |
| `HISTORY_REQUEST_TIMEOUT`, `HISTORY_TIMEZONE` | `10s`, `America/Lima` | How long a request to the history may take; the place whose hours the Historial shows (a name of the time zone database) |
| `AGENT_REQUEST_TIMEOUT` | `120s` | The longest a reply may take; use `600s` with the real BFF. An answer to a card (Pay, Reject, the code) is given at most 60 s whatever this says |
| `APP_HTTP_ADDR` | `:8300` | Where to listen (`.env.example`: `127.0.0.1:8300`) |
| `APP_BASE_URL` | `http://localhost:8300` | The public address; https turns on Secure cookies and HSTS |
| `APP_ENV` | `development` | `production` refuses to start without an https `APP_BASE_URL` |
| `ADMIN_EMAIL`, `ADMIN_PASSWORD` | none | With `YAPE_ACCOUNT_ID`: make the user of that account at start, unless one with that id exists (password: 12+ characters) |
| `CHAT_MAX_MESSAGE_CHARS`, `SESSION_TTL` | `8000`, `24h` | Longest message; how long a sign-in lasts |
| `RATE_LIMIT_RPS`, `RATE_LIMIT_BURST` | `2`, `10` | Per user on sends, per client on sign-in |
| `APP_LOG_LEVEL`, `APP_LOG_FORMAT` | `info`, `json` | Logging |
| `EXPLORE_*` | see `.env.example` | The explorer: the UI's address, who signs in, how Chrome is started |
| `AGENT_LIVE`, `HISTORY_LIVE` | `0` | Switch on the opt-in live tests against the real BFF and the real history |

## Operating it

- **Logs.** JSON lines go to stdout (`scripts/run.sh` keeps a copy in `logs/`). The first line, `starting`, lists every setting under
  `config` by its field name; `DB_URL` and `ADMIN_PASSWORD` show as `[redacted]`, along with the first user's address and account, and URLs appear
  without credentials or query strings. Each request gets one line with `request_id`, `method`, `path` (no query string), `status`,
  `bytes`, `duration` (nanoseconds) and `remote`, and nothing else: no header, cookie or form value, except a well-formed
  `X-Request-ID`, which becomes the request id. A failed reply logs `reply failed` with its `message_id`; a history that cannot be read logs a warning. Message text, accounts,
  passwords and tokens are never logged.
- **Health.** `GET /healthz` confirms the process is up. `GET /readyz` checks the database and the BFF (not the history: the chat does not depend on it) (`GET /health`, which needs no
  token) and returns 503 naming whatever failed. The container's health check runs `agent-web-ui healthcheck`.
- **Stopping.** SIGTERM (`scripts/stop.sh` or `docker stop`) lets replies in progress finish writing, ends each as stopped, and exits
  within `APP_SHUTDOWN_GRACE`. `scripts/stop.sh` waits that long plus 2 seconds before SIGKILL.
- **The page.** A page request with no answer in 30 seconds gives up and says "The server took too long to answer. Try again." A reply's
  event stream has no such limit.
- **The image.** `Dockerfile` builds the web server as a distroless, non-root image. It needs a program for `YAPE_TOKEN_BIN` that
  belongs to wherever it runs (yape-token is not in the image), which is why Compose runs only PostgreSQL and the scripted BFF.
- **Back up and restore.** The dump holds the users' password hashes: keep it private. The chats are in the history, not here.

  ```sh
  docker compose exec -T postgres sh -c 'pg_dump -U "$POSTGRES_USER" -d "$POSTGRES_DB"' > backup.sql   # back up
  docker compose exec -T postgres sh -c 'psql -U "$POSTGRES_USER" -d "$POSTGRES_DB"' < backup.sql      # restore, into an empty database
  ```

- **Upgrade.** The program creates and changes no table, so a release that needs another schema needs it in place first: take a
  backup. An older program works against a newer schema only if the change is backward compatible.
- **Limits to know.** One instance at a time (the turns in memory, the Stop registry and the rate limits live in memory). A conversation opens with its
  latest 100 messages, and "Earlier messages" loads the rest. No TLS, no metrics endpoint, no password change. A reply's token counts are unknown: the BFF's stream carries none.

## Tests

```sh
scripts/check.sh                  # the quality gate: format, vet, linters, vulnerabilities, race tests, coverage, the tests under it/ that need no Docker
scripts/check.sh --it             # the same, with all the integration tests
scripts/pr-check.sh --title "fix: stop dropping the partial reply"   # before a pull request: its title and size
go clean -testcache && go test -race ./internal/... ./cmd/...   # unit tests, no Docker
go test ./it/...                  # integration: needs Docker (add -short to skip the image builds)
go test ./it/live/                # with AGENT_LIVE=1 in .env: the real BFF it names; never part of the default run
```

Or `make audit`, `make audit/it` and `make test`. The gate expects 85% coverage on the use cases (`internal/core`) and 70% everywhere
else under `internal/` and `cmd/`, counted from unit tests alone and excluding generated mocks (`COVER_CORE_MIN`, `COVER_ALL_MIN`,
`COVER_PKG_MIN` and `COVER_EXEMPT` in `.env`). The shares are truncated, not rounded, to one decimal: 84.96% prints as 84.9% and counts
as under 85. The linters come from `bin/tools` (`scripts/build-tools.sh`).

After the coverage, `scripts/check.sh` runs the tests under `it/` that need no Docker: `./it/scripts/` and the `TestRoutes_` tests of
`./it/security/`. `--it` runs everything under `./it/...`; `--quick` skips the race detector and the vulnerability scan.

On GitHub the same checks run on every pull request (`.github/workflows/ci.yml`: the gate, a secret scan, the title and size check),
plus a build, vet and test of the Windows-specific process handling on a `windows-latest` runner.

The integration suites start PostgreSQL with Testcontainers and run the real application against the scripted BFF
(`internal/testbff`). The chat and security suites start a fresh stack per case (chat) or per test (security), and fail if any of the
application's goroutines are still running afterward.

| Suite | What it proves |
|-------|----------------|
| `it/chat` | A reply arrives in pieces and ends as shown; Stop, closing the page and shutdown cancel the agent's run and keep the text; a dead agent gives an error and the chat goes on once it is back, and an answer the BFF cuts off is kept as far as it got, with the text it got to; the flush interval paces the updates; replays never call the agent; the Web UI adds under 100 ms (95th percentile) |
| `it/security` | Security headers on every route (the table follows every route pattern the web package names); anonymous visitors, other origins and other users get nowhere; hostile replies and user text stay inert; passwords and sessions are stored safely; cookies; size and rate limits; https; the log never holds what people write or the passwords they type |
| `it/container` | The images build from their Dockerfiles, run as an ordinary user, answer their health checks, carry a chat between them, and stop at once on `docker stop` |
| `it/app` | The assembled application: the schema, the user of an account (and an email that is another's stops the start), a restart that touches nothing of the old chats, and `/readyz` following the BFF down and up |
| `it/scripts` | The shell scripts, run for real, with no Docker: the pid-file helpers, `.env` loading, how settings are read, that `run.sh` passes `--env` on, how long `stop.sh` waits, the pull-request check, the gate against stand-in tools, what `.gitignore` and `.dockerignore` keep out, and that the CI workflow runs the scripts |
| `it/live` | The real BFF answers and can be stopped (`AGENT_LIVE=1`); the history adapter against the real history (`HISTORY_LIVE=1`); and the whole flow: a chat's id in the history, the Historial, a conversation continued, another user, a payment from a card |

## Layout

```
cmd/web                   the Web UI binary
cmd/test-bff              the scripted BFF
cmd/iape-explore          the explorer
internal/core             chat (live turns), history (reads it), auth, interaction (the cards), domain (ports in core/port)
internal/infrastructure   postgres (users, sessions), memory (the live turns), historyapi (the history client), agent/aguibff (the BFF client), web (HTTP, templates), auth, config
internal/platform         what the programs share and neither owns (reading .env, secrets, a view of a struct for a log)
internal/testbff          the scripted BFF
internal/explore          the explorer's logic: steps, the catalog of paths, the run
internal/chrome           the explorer's browser: Chrome DevTools MCP
assets                    templates, static files (HTMX, Bootstrap vendored)
it                        integration tests
scripts                   init-env, run, stop, check, build-tools, pr-check, test-token
sketches                  small, standard-library-only reference implementations (see sketches/README.md)
.github                   CI workflow and pull request form
```

Which package can import which is tested too (`internal/architecture`): a Web UI use case can import its own domain and ports and no
other use case; an adapter can import the domain and the ports but no use case; neither side of core imports `net/http`,
`database/sql`, `os` or `io/fs`; the explorer's logic knows only itself, its browser only the logic, and neither knows the Web UI. The
same package enforces two rules about secrets: a `Secret` lives in an exported struct field, and no string in a production file holds
the `%p` verb.

## License

[MIT](LICENSE): use, modify and sell it freely.
