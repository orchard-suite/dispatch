# dispatch

The incident desk. Alerts become incidents that a person owns until the
thing is restored, with a timeline, a postmortem, and the numbers an
outcomes board reads. Read `README.md` first.

This repository is public and estate-neutral. Every name in a test, an
example, or a doc comes from the fixture world:
https://github.com/orchard-suite/hedge/blob/main/docs/fixtures.md.

## Shape

- `cmd/dispatch` is the server. One Go module, one static binary, one
  SQLite file (`modernc.org/sqlite`, WAL) under `DATA_DIR`.
- `internal/server` answers on three ports, one per kind of caller:
  - `Session` (8080): the desk. Trusts the front door's `X-Forwarded-Email`
    and `X-Forwarded-Groups` headers, so only Traefik may reach it.
  - `API` (8081): `/v1/alertmanager`, `/v1/azuremonitor`, `/v1/incidents`,
    `/v1/outcomes`, `/v1/events`, `/v1/changes`, `/v1/change-events`,
    `/healthz`. Trusts a bearer token the desk issued or the shared secret
    in `ALERTMANAGER_TOKEN`, and nothing else; `/v1/azuremonitor` also
    trusts an action group's secure webhook token (`internal/azuremonitor`'s
    `Door`). `/api/messages` is the
    Teams bot's door and trusts only a token the bot service signed for
    the bot (`Verify` in `internal/teams`). `openapi.yaml` at the root
    describes every route but `/healthz`; `orrery.yaml` names it as the
    `spec` of both APIs, and a test in `internal/server` fails while it and
    the routes differ. Change it with the route.
  - `Status` (8082, on when `STATUS_ADDR` is set): the public status page.
    Trusts nobody and names nothing the settings did not list.
- `internal/incident` is the model: severities, states and their allowed
  moves, the grouping key, the mapping rules, the paging rule
  (`incident.Pages`), the auto-resolve rule. It has no storage and no
  transport.
- `internal/alertmanager` reads Alertmanager's webhook payload (version 4);
  its `Alert` is the desk's alert from every source.
- `internal/azuremonitor` reads an Azure Monitor action group's webhook in
  the common alert schema into the desk's alert, and its `Door` checks the
  Entra token a secure webhook carries: Azure Monitor's app, for the app in
  `AZURE_MONITOR_APP_ID`, holding the `ActionGroupsSecureWebhook` role.
  `azuremonitortest` stands in for Entra in tests.
- `internal/signed` checks a JWT another service signed with a key from
  its OpenID key set (shape, RS256, time). The Teams bot's and Azure
  Monitor's doors check their claims on top of it.
- `internal/pagerduty` reads PagerDuty's Events API v2 for `POST
  /v2/enqueue`, whose credential is the body's `routing_key`, checked like
  a bearer token.
- `internal/incident/routes.go` is the routes that drop, quiet, raise, or
  rate an alert; `ops.go` is maintenance windows, heartbeats, changes, and
  leaning (which catalog relations say a thing leans on another).
  `internal/desk/ops.go` raises late heartbeats (`Watch`) and quiets events
  in a window (the outbox's hush). `internal/desk/changes.go` keeps
  changes and the suite's change events (platformkit's `change` package:
  `change.Decode`, the kinds, the types; never copy them here), and joins
  incidents to changes by catalog entity. `internal/store/changes.go` is
  their rows; `internal/changetest` builds change events for tests.
- Every change route decodes strictly: a field the contract does not name
  is refused, never dropped. A change event's sender is the token's name.
- `internal/desk` is the logic: a webhook folds into incidents, a person
  moves one through its states, and every change is written to the
  timeline, counted, announced, pushed to the catalog, and posted to the
  annals. Opening and changing state take an `incident.Actor`: a person
  on the desk, or a system (a token's name, `alertmanager`, `dispatch`).
- `internal/store` is the SQLite schema and queries.
- `internal/outcomes` is the maths behind `GET /v1/outcomes` and the
  Components view. Its JSON is a contract: fields are added, never renamed.
- `internal/orrery` resolves a namespace to its Orrery workload, pushes
  each incident as an `it_incident` entity, and walks the catalog's
  relations for what leans on what (`Leaners`, `LeansOn`), through
  platformkit's `orrery` client. The blueprint it offers matches
  `deploy/orrery-catalog/model/incidents.yaml`.
- `orrery.yaml` at the root is what this repository builds, for the estate
  that reads it: Muster reads it from the repository and writes the
  service and its APIs into Orrery. The app never describes itself; its
  Orrery source token pushes incidents only. A test in `internal/server`
  fails while a file it names (`logo.svg`, a `spec`) is missing.
- `internal/notice` is the notification model: an event, a channel, a
  subscription and what it takes, a delivery and its retry waits, and the
  body each channel kind is sent (the Teams Adaptive Card). It has no
  storage and no transport.
- `internal/outbox` takes events from any product, queues one delivery per
  channel a subscription sends it to, and delivers them, keeping each
  channel's answer and trying again on the waits in `notice.Retry`. It is
  the only place a notice is sent. A delivery is a row before anything is
  sent. The desk's incidents reach it as events through `announce`.
- `internal/teams` speaks the Bot Connector protocol for the Teams bot:
  it checks the bot service's token on every activity, signs the bot in
  with the federated workload token, posts cards into channels, lists a
  team's channels, and gets a person's token from the bot service's token
  store. `internal/teams/teamstest` stands in for the bot service in
  tests. `internal/server/bot.go` is the door: the teams the bot is in,
  and a card button, which calls the producer as the person who clicked
  and never as the bot.
- `internal/outbox/people.go` addresses events to people, groups, and
  chains: it queues one message per person per medium, sends them, and
  moves chain pages on when their wait runs out. `AcknowledgeFor` ends the
  pages an incident's events started; the desk calls it on acknowledge.
- `internal/mail` sends one letter: Microsoft Graph signed in with the
  federated workload token (never a client secret), or SMTP from
  `MAIL_SMTP_URL`. Exactly one is set. A sender answers an HTTP-like
  status so `notice.Retry` and `notice.Permanent` hold for both.
- `internal/notice/address.go` is the directory model: targets, people
  and their own addresses (SMS number, Discord account), groups, chains
  and their steps, pages, and messages. `reach.go` is weights, threads and
  their moves, a person's rules and quiet hours, and `Person.Plan`, which
  says when each medium is sent. `mail.go` is an event as a letter.
- `internal/store/directory.go` holds people, groups, chains, and the
  schema upgrades counted by `PRAGMA user_version`; a change to an existing
  table is a new entry at the end of `upgrades`, never an edit of one that
  shipped. `pages.go` holds pages and messages.
- `internal/server/directory.go` and `pages.go` are the People page and a
  chain page's Acknowledge. Every desk page keeps the person who opened it.
- `internal/notice/kinds.go` is the kinds a producer declares and a
  person's choice for one. `internal/store/kinds.go` keeps both.
  `internal/server/kinds.go` is `PUT /v1/kinds` and the grid on Me, and
  `chose` in `internal/outbox/people.go` is the one place a choice is
  applied.
- `internal/sms` texts through Azure Communication Services and reads
  Event Grid's posts back: texts people send (`ACK`, `ESC`) and delivery
  reports, tagged with the message's id. `internal/server/sms.go` is
  `POST /v1/sms`, behind an issued token Event Grid sends as a header.
- `internal/discord` is the Discord bot: direct messages and channel
  posts with page buttons, a channel's plain webhook, and the Ed25519
  check on every interaction. `internal/server/discord.go` is
  `POST /api/discord`; a press acts only as the person whose account it
  is.
- `internal/entra` signs an Entra app in with the federated workload
  token for one scope. Graph mail and SMS use it.
- `internal/notice/schedule.go` is schedules: layers, hours, covers,
  `OnCall`, `Shifts`, and `Gaps`. `internal/desk/schedules.go` tells
  handoffs and gaps, keeps covers, and runs snoozes; `Watch` drives it
  with the heartbeats. `GET /v1/calendar/{secret}` takes a person's link
  secret in the path, checked against its hash, since calendar apps send
  no bearer token.
- `internal/desk/response.go` is running an incident: roles, merges,
  links, status updates and followers, action items' issues
  (`internal/github`), and rooms. `internal/server/rooms.go` opens a
  room as a Teams channel through Graph, signed in as the bot's app.
- `internal/assist` is the assistant, asking Claude through the Ion
  engine's `ion prompt` (the image carries the engine and the Claude Code
  CLI): one-run writing and a bounded, read-only investigation that keeps
  every tool call as numbered evidence. Its tools are an MCP server on the
  loopback interface; each call names its investigation. `ASSIST_BACKEND`
  (`claude-code` or `api`) is the only difference between a subscription
  and an API key. `internal/desk/assist.go` asks it
  on a person's behalf and keeps its answer. It holds no reader of its
  own: an investigation's data tools are the read tools the hub's tool
  door lends (`internal/orrery/tools.go`), listed for each investigation
  and called through the one tool `hub_tool`, because Ion's allow rules
  are exact names written once at start. `internal/server/mcp.go` is
  `POST /mcp` on the official MCP Go SDK: its reads, and `mcp_act.go`
  its changes, which act only as a personal token's person.
- `internal/server/templates/layout.html` is the desk's shell (the rail
  grouped by job, the on-call pill, Open incident, the command menu) and
  `desk.js` its one script: regions marked `data-live` with an id swap in
  place when `GET /live` (`live.go`, server-sent events over
  `store.Mark`) says something changed, posts happen in place, clocks
  count, and keys and the command menu read `data-key` and `data-cmd`.
  `names.go` turns an address into a name and initials; a page shows
  people only that way. Settings are places (`settings_pages.go`), and
  their lists change a row at a time (`settings_rows.go`).
- `internal/config` is the routing document; `internal/desk/config.go`
  checks a whole document before applying any of it, and exports one.
  `cmd/dispatch/cli.go` is `dispatch export` and `dispatch apply`.
- `internal/seal` seals a channel's URL with `SECRET_KEY`.
- `internal/notify` posts the older single JSON webhook on open and on state change.
- `internal/annals` posts each open and state change to Annals
  as one event, through platformkit's `annals` client, when `ANNALS_URL`
  and `ANNALS_TOKEN` are set. Its `event_key` is the timeline entry's id.
  A post runs in its own goroutine; a refusal or failure is logged and
  dropped, never retried, and never fails the change.
- `internal/metrics` is the desk's own metrics. Their names are a contract
  with the deployment; do not rename one without changing the dashboard
  and alerts.
- The shared plumbing comes from platformkit (`github.com/orchard-suite/platformkit/v3`):
  `door` gates the session port on the front door's person and roles and
  checks `Origin` on changes, `telemetry` is the JSON logger, the OTLP
  export, `dispatch.build_info`, the process metrics, and the instrumented
  HTTP on both sides, and `faro` is the browser relay behind
  `POST /api/telemetry`. Do not copy any of it back into this repository.
- `deploy/orrery-catalog` is the incident blueprint an estate's catalog
  imports; nothing in `deploy/` is applied from this repository.

## Rules

- Never let a header set identity on the API port, never accept a bearer
  token on the session port, and never add a route that skips either check.
- Every change on the session port checks `Origin` against `PUBLIC_URL`.
- A bot card's button acts only as the person who clicked, with their own
  token from the token store, on the producer named by `BOT_ACTS_FOR`, and
  only on a path the event itself carried. The bot never calls a product as
  itself.
- Only an alert that pages (`incident.Pages`) opens or reopens an
  incident; any other alert is a notice. Most notices are not incidents.
- A resolved alert never resolves an incident by itself. A person does,
  or one of the two machine rules in `internal/incident/model.go`:
  `ShouldAutoResolve` (an info incident with auto_resolve on, when its
  last alert resolves) and `ShouldResolveQuiet` (any incident alerts
  opened, once its alerts have been quiet for the settings' quiet window).
  Keep machine resolution to those two rules, and have each write why on
  the timeline.
- The status port's only writes are follow, confirm, and unsubscribe;
  following mails a confirm link and nothing more until it is used.
- The status page shows only what the settings and the incident's own
  `public` flags allow: named components, public titles, public notes.
  No owner, no label, no service key, no internal note. The redaction test
  in `internal/server` guards it.
- A token's secret is shown once and only its hash is stored. A channel's
  URL is sealed and never shown or logged again. `MAIL_SMTP_URL` holds a
  password: never log it or put it in an error.
- A number saved on Me needs the person's ticked opt-in, and its time is
  kept on the person (`SMSConsentAt`). A new or cleared number drops it
  (`Person.KeepConsent`); only the person gives it, never an admin.
- Graph mail and SMS sign in with the workload token only. Never add a
  client secret or access key path.
- A Discord press is trusted only with Discord's signature over it, and
  acts only as the person whose Discord account is on their Me page.
- Acknowledging a page is a change on the session port: `Origin` checked,
  and the person is the front door's, never a link's.
- An event's producer is the token's name, never the body's word. The
  same holds for the kinds a producer declares.
- Me (`GET /me`, `POST /me`, `POST /me/kinds`) and what its page loads
  are the only session routes open to someone signed in with no desk
  role. They still need the front door's identity and the `Origin` check.
  Never show such a person anything else of the desk, and never open
  another route to them without a decision to.
- A person's choice for a kind never weakens escalation: it is not read
  for an event of weight `page`, nor for one a maintenance window
  quieted, and only the producer that declared the kind is listened to.
- The assistant reads and writes only. Never give it a tool that changes
  anything, and never let its output change an incident on its own.
  Dispatch holds no key to another system for it: data comes only through
  the hub's tool door, with a token whose role holds only `tools.read`,
  and only tools of tier `read` that the hub listed for that
  investigation are ever called. That token is a secret from the
  environment, never in a row or a log.
  Keep both locks: Ion's permissions that dispatch writes, and the Claude
  Code deny list in the managed settings (README, The assistant).
- A token acts as a person only when it was made for that person on Me;
  the MCP tools that act need such a token.
- dispatch ships no app. A person is reached only through apps they
  already have, by their own rules; a page is the only weight that may
  ignore their quiet hours.
- Every routing decision and every change to how notices are routed is
  posted to Annals (`annals.Decide`); retries are not decisions.
- Logs are JSON lines on stdout. Log both branches of a decision about who
  is let in.
- Metric attributes are severity, service, source, outcome. Never a person,
  an incident id, or free text.
- No continuously repainting CSS animations.
- No em dashes or en dashes in prose. No real names or addresses in
  fixtures or docs.

## Delivery

Agents push to main and land with `make land` (`make land BRANCH=<branch>`
from a worktree): it runs `make check`, then `release-damnit land`. CI runs
the same check on every pull request and before every release, then
versions every `feat` and `fix`, tags it, and builds
`ghcr.io/orchard-suite/dispatch:<version>` for `linux/amd64`, signed with
cosign. Never edit `VERSION`, the manifest, or `CHANGELOG.md` by hand.

## Checks

```bash
make check
```

Run it before every commit; `make land` runs it again, and CI runs it on
every pull request and release.
It needs Go (see `go.mod`). `make run` starts
a local server on 8080, 8081, and 8082 with `local-secret` as the shared
secret and `data/` as the data directory.
