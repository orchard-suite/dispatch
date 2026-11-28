# dispatch

The incident desk. Alertmanager fires, someone silences or fixes, and
nothing holds the incident: no owner, no timeline, no postmortem, no time
to restore. dispatch holds it. Alerts become incidents, a person owns each
one until the thing is restored, and the desk exports the numbers an
outcomes board needs. It is fed by Alertmanager's webhook and by anything
that can POST JSON, and it serves any organization. It is not an
inventory: the catalog of what exists stays where it is.

It is also the one place every product's notices go out from: to Teams
channels and webhooks, and to people through the apps they already have
(Teams, Discord, SMS, email), one at a time, as a group, or up an
escalation chain until someone acknowledges. dispatch has no app of its
own. Most notices are not incidents.

## The model

An **incident** has:

- a **severity**: `critical`, `warning`, or `info`, from the alert's
  `severity` label; a person can change it;
- a **title**, from the alert's `summary` annotation or its name;
- a **service**, the component it is on, from the alert's labels through
  a small rule set (Routes) and, with no rule, the first of `service`,
  `app`, `namespace`, `job` that is set. When `ORRERY_URL` and
  `ORRERY_TOKEN` are set, the `namespace` label is resolved to the Orrery
  **workload** the namespace links to;
- a **state**: `open`, then `acknowledged`, then `mitigated`, then
  `resolved`. Forward moves may skip a step. The only move back is
  reopening a resolved incident;
- an **owner**: the person who acknowledged it, or whoever it was assigned
  to;
- a **timeline**: every alert firing and resolving, every state change,
  every note, each with who and when;
- after resolution, a **postmortem** in Markdown from a template (what
  happened, impact, timeline, cause, what we change) and **action items**.

Only an alert that **pages** becomes an incident: a critical alert, or
one labelled `page=true` or `notify=sms`. Any other alert joins an
unresolved incident of its group if there is one, and otherwise goes to
the channels as an `alert.firing` or `alert.resolved` notice, threaded by
its fingerprint; it never opens or reopens an incident.

**Routes** (the desk's Routes page) come first: an alert that fits one is dropped, made
a notice, made to page, or given a severity. A **maintenance window**
quiets a service for a while: its alerts page nobody and its events tell
nobody, though channels still get them, marked `maintenance=true`.

Alerts come from Alertmanager's webhook, from Azure Monitor's action
groups (`POST /v1/azuremonitor`), from anything that speaks
**PagerDuty's Events API v2** (`POST /v2/enqueue`, the dedup key as the
alert's fingerprint, `error` and `critical` paging), and from
**heartbeats**: a job promises to check in every interval, and past it
and its grace the desk raises a `HeartbeatMissed` alert, which the next
check-in resolves. **Changes**, such as deploys, are kept by their
service and the catalog entities they touched, and shown on the page of
every incident on them, or on a thing that leans on them, that opened up
to two hours after (Changes below).

Alerts group into one incident by a **grouping key**: the alert's name
plus its service. An unresolved incident always takes its alerts. A
resolved one takes them for the **grouping window** after it resolved
(default fifteen minutes) and reopens; after that a new incident opens.

A resolved alert does not resolve an incident on its own. A person does,
or one of two rules. An `info` incident with `auto_resolve` on resolves
when its last alert resolves; new info incidents get `auto_resolve` from a
setting (on by default). And an incident of any severity that alerts
opened resolves once none of its alerts has fired for the **quiet window**
(General settings, 24 hours by default, 0 turns it off), with a line on its
timeline saying so. If the trouble comes back after the grouping window,
a new incident opens. An incident a person opened holds no alerts and
never resolves this way.

While an incident waits out the quiet window it is **settling**: it
carries `closes_at`, the desk lists it apart under "Cleared, closing on
their own" with a Resolve now button, the board's `settling` list holds
it instead of mine, unowned, or others, and Orrery's entity shows phase
`settling`. When its last alert stops the desk sends `incident.cleared`
once, saying when it closes.

An incident's events carry the `notify` and `page` labels of its alerts,
so a subscription can match them: `match: {notify: sms}` reaches only the
incidents an alert asked to text about.

## The desk

Behind the estate's front door, on the session port. A rail on the left
groups the desk by job: Respond (Home, Incidents), On call (Schedules,
People and chains), Signals (Services and noise, Routes, Notifications),
Publish (Status page, Status settings), and Desk (General, Machines). Its
foot holds the on-call pill (whether you are on call, and for how long),
**Open incident**, and you. On a phone the rail becomes a tab bar and a
More sheet.

- **Home** (`/`): a triage desk. One sentence says what is open, how bad,
  and whether anyone is on it. The queue sorts by who must act next:
  Needs you (nobody owns it, or it fires and nobody acknowledged it),
  Yours, Settling (closing on its own, with a countdown and Resolve now),
  and Others, every row's age on one shared clock. Views (All open, Mine,
  Unowned, Critical, Settling) and a List or Board layout sit above it.
  Beside the queue, the selected incident: its summary, phase bar, **Next
  move** with one primary button, and its alert strip. With nothing
  selected, or nothing open, the estate instead: every service as a lane
  on a seven-day clock, worst first (a cell is the worst severity that
  fired in four hours, hatched where an incident sat open and quiet; an
  open incident is a bar still growing at the right edge), and the
  **Noise** panel, the alerts that fired most, each with **Quiet it**,
  which opens Routes with that route ready to save.
- **Incident** (`/incidents/{id}`): Overview and Postmortem tabs. Overview
  leads with a written summary (Edit keeps it as a note), then the phase
  bar with when each state was reached, the **Next move**, the alert strip
  of firings, moves, and changes on one axis, and the activity timeline
  grouped by day, with quiet gaps marked, alert flaps folded into runs,
  and a Highlights or All filter. A pinned properties rail holds state,
  severity, owner, roles (commander, comms, scribe), service and workload,
  durations that count up, alerts, links and merges (a merge moves the
  alerts and resolves the merged one), status page visibility, the
  catalog's runbook, docs, owner, and repository, and, with a Teams room,
  its channel. Every action (acknowledge, mitigate, resolve, reopen, take
  or assign, note, severity, status update from the severity's template,
  and the rest) is in the command menu. Postmortem holds the Markdown
  postmortem and its action items.
- **Incidents** (`/history`): every incident as a sortable table, with
  repeats of a title on a service in a day folded into one row, search
  over title, service, owner, and id, views (All, Open, Mine, Unowned,
  Critical, This week, Needs postmortem) and views you save, and a Board
  by state.
- **Services and noise** (`/components`): over 7, 30, or 90 days, each
  service as a lane on one clock with its incidents, MTTA, MTTR, and pages
  (and how many at night) beside it; the **noise**: every alert that
  fired, how often it paged, and how often a person acted on it; and each
  person's **on-call load** (pages, pages at night, acknowledgements,
  hours on call).
- **Schedules** (`/schedules`): who is on call on each schedule now and
  until when, a two-week ribbon with one color per person and its gaps
  hatched, and its covers. Anyone on the desk adds a cover; admins edit
  a schedule's layers as rows.
- **Status page** (`/status`): the status page for the people behind the
  front door, with every open incident and every note.
- **Me** (`/me`): your shifts and a calendar link to them, everything that
  reached you, whatever its weight, and how you are reached: your SMS
  number, Discord account, a rule for each weight, and quiet hours, with
  how to let a page through your phone's Do Not Disturb. **What you are
  told about** is a grid with one row for each kind of event a product
  declared, grouped by product, where you tick how you want to hear of
  that kind (Notifications below). Only you change yours; the address is
  the front door's. Me is open to everyone the front door signs in, with
  or without a desk role.
- **People and chains** (admins, `/people`): the people the desk reaches
  and how, the groups, and the escalation chains as numbered steps.
- **Notifications** (admins): the channels, the subscriptions that send
  them events, the active and recent pages, and the newest deliveries and
  messages to people with each one's answer.
- **Page** (`/pages/{id}`, anyone on the desk): one chain's page, who it
  told, and the Acknowledge button every chain message links to.
- **Settings** (admins), each its own place, each list edited a row at a
  time: **General** (`/settings`: the grouping and quiet windows, the
  auto-resolve default, the desk's time zone, interruption budgets),
  **Routes** (`/settings/routing`: the routes, each with how many alerts
  it caught in seven days, and the mapping rules), **Status settings**
  (`/settings/status`: the status page's title, components, and update
  templates), and **Machines** (`/settings/machines`: API tokens,
  maintenance windows, heartbeats, and issue repositories).

Pages update themselves: the desk holds a server-sent event stream
(`GET /live`) that says when anything a page shows changed, and the page
fetches itself and swaps the parts that moved. Changes post in place,
durations count up, and people show by name and initials, never by
address. The keyboard works everywhere: Cmd or Ctrl K opens the command
menu (every action on the page, every place, a jump to `#number`, and a
search), `/` searches, `j` and `k` move through rows, and each page's
main moves have a key, such as `A` to acknowledge.

Phone-first, light and dark, no continuously repainting animations. The
page runs the Grafana Faro SDK as `dispatch-web` and posts its telemetry to
`POST /api/telemetry` on the desk, which relays it with the estate's key
through platformkit's `faro` relay.

Roles come from the front door through platformkit's `door` gate:
`X-Forwarded-Groups` must carry `DESK_ROLE` (default `User`) to open the
desk and `ADMIN_ROLE` (default `Admin`) to change settings and tokens. Both
are bare roles, such as `Admin` for the app role `dispatch.Admin`; the
server refuses to start with a role that carries the app's part.

**Me** is the one page that needs no role. Anyone the front door signs in
can open it, save how they are reached, and set their grid, because a
product may address an event to someone who never works an incident. With
no desk role they see their own list and settings and nothing else: no
rail, no counts, no shifts, no live stream, and no agent token or calendar
link. Home sends them to Me, and every other page and change answers 403.
The directory keeps them as a person the first time they save, with no
role, since roles are only ever the front door's word.

## The API

On the API port, every route but `/healthz` needs
`Authorization: Bearer <token>`: a token the desk issued (Machines) or the
shared secret in `ALERTMANAGER_TOKEN`. `POST /v1/azuremonitor` also takes
the Entra token of an Azure Monitor action group's secure webhook.

| Route | Does |
| --- | --- |
| `POST /v1/alertmanager` | Alertmanager's webhook receiver. Body: the version 4 payload. Answers which incidents it opened, updated, reopened, or resolved. |
| `POST /v1/azuremonitor` | An Azure Monitor action group's webhook, in the common alert schema. A Prometheus rule group's alert keeps its labels and annotations; any other alert is named by its rule, takes its severity from Sev0 and Sev1 (critical), Sev2 (warning), or lower (info), and its service from the `service` custom property, else the resource it fired on. Custom properties become labels, so an action rule can add `notify=sms`. It then folds as an Alertmanager alert does. Point a secure webhook (Entra auth) at it with `AZURE_MONITOR_APP_ID` set: the token must be Azure Monitor's app's, for that app, holding its `ActionGroupsSecureWebhook` role. |
| `POST /v1/incidents` | Open an incident. Body: `{"title","severity","service","description","public"}`. |
| `GET /v1/incidents` | List. Query: `state=open,acknowledged`, `service=`, `severity=`, `q=` (title, service, owner, or id), `limit=`. |
| `GET /v1/incidents/{id}` | One incident with its alerts, timeline, postmortem, and action items. |
| `POST /v1/incidents/{id}/acknowledge`, `/mitigate`, `/resolve`, `/reopen` | Move it. Optional body `{"actor":"who"}`; the token's name otherwise. |
| `POST /v1/incidents/{id}/notes` | Add a note. Body: `{"text","public","actor"}`. |
| `POST /v1/incidents/{id}/roles` | Give a person a role. Body: `{"role","person"}`. |
| `POST /v1/incidents/{id}/merge` | Merge it into another. Body: `{"into","actor"}`. |
| `GET /v1/outcomes?window=30d` | The outcomes report below. |
| `POST /v1/events` | Send an event to the channels that subscribe to it and to the people, groups, and chains it reaches (Notifications). Body: `{"kind","key","title","text","page","severity","weight","action","dedup_key","entity":{"blueprint","key","title"},"facts":[{"name","value"}],"links":[{"title","url"}],"labels":{},"to":[{"person":"a@example.com"},{"group":"platform"},{"chain":"platform-oncall"}],"at"}`. Answers `{"event","queued","messages","pages","repeat","deduplicated","dropped"}`. An unknown group or chain is a 400 naming it. |
| `PUT /v1/kinds` | A product declares the kinds of events it sends, so people can choose on **Me** how to hear of each (Notifications below). Body: `{"kinds":[{"kind","title","description","weight"}]}`, which replaces that producer's whole set; an empty list clears it. `kind` is dotted lowercase words as in an event, `title` is 1 to 120 characters, `description` at most 300, `weight` is `notice`, `notify`, or `page`, and a producer declares at most 100. Answers `{"kinds": n}`. A kind another producer declared is a 400 naming it. A personal token is a 403. |
| `POST /v2/enqueue` | PagerDuty's Events API v2: point anything that pages PagerDuty here. The `routing_key` in the body is an issued token or the shared secret. A trigger is an alert; acknowledge notes it on the incident; resolve resolves the alert, never the incident. Answers as PagerDuty does. |
| `POST /v1/heartbeats/{name}` | A heartbeat's check-in. `?interval=24h` makes one the desk does not know. |
| `POST /v1/changes` | A deploy or another change. Body: `{"service","title","kind","url","by","at","things":[{"blueprint","key"}],"run","record":{"sender","key"}}`, with its service, its things, or both. A field the contract does not name is a 400, never dropped. Answers the change, 201; one whose `record` is kept already answers the kept change, 200. Sent on as a `change.<kind>` notice. |
| `GET /v1/changes` | Changes back, newest first. Query: `entity=blueprint:key` (repeat it for several), `service=`, `since=`, `until=` (RFC 3339), `limit=` (default 50, at most 500). A change matches on its service or any entity it touched. Answers `{"changes":[...]}`. |
| `POST /v1/change-events` | The suite's change events, all or none (Changes below). Body: `{"events":[...]}`. Answers `{"kept","repeats"}`, or a 400 `{"error","index","reason"}` naming the first refused event. A producer's token only. |
| `GET /v1/schedules` | Who each schedule has on call now, and until when. |
| `POST /v1/schedules/{slug}/overrides` | A cover. Body: `{"person","from","to","reason"}`. |
| `GET /v1/calendar/{secret}.ics` | One person's shifts as an iCalendar feed. The link's secret, made on **Me** and kept only as a hash, is the credential, since a calendar app sends no bearer token. |
| `POST /mcp` | The desk as an MCP server for agents (Agents below). |
| `GET /v1/config`, `PUT /v1/config` | The desk's routing as one YAML document (Routing as a file below). |
| `POST /v1/sms` | Event Grid's door for the SMS number: its validation, texts people send (`ACK` takes the page that last texted them, `ESC` passes it on), and delivery reports. Event Grid sends an issued token as a static `Authorization` header. |
| `POST /api/discord` | The Discord bot's door: Discord's ping and a press of a card's button. Takes only a body Discord signed with the application's key; a press acts as the person whose Discord account is on their **Me**. |
| `GET /healthz` | `ok`. |

### Outcomes: the contract

`GET /v1/outcomes?window=30d` (also `24h`, `7d`, `90d`, up to `365d`) is
read by an outcomes board. Its fields are never renamed or removed; new
ones may be added.

```json
{
  "window": "30d",
  "since": "2026-10-01T00:00:00Z",
  "until": "2026-10-31T00:00:00Z",
  "total": {
    "incidents": 12,
    "open": 1,
    "by_severity": {"critical": 2, "warning": 8, "info": 2},
    "acknowledged": 10,
    "resolved": 11,
    "mtta_seconds": 412.5,
    "mttr_seconds": 5310.0,
    "ttr_trend": "improving",
    "pages": 6,
    "night_pages": 2,
    "unanswered_pages": 0
  },
  "services": [
    {
      "service": "shop",
      "incidents": 5,
      "open": 0,
      "by_severity": {"critical": 1, "warning": 4},
      "acknowledged": 5,
      "resolved": 5,
      "mtta_seconds": 180.0,
      "mttr_seconds": 2400.0,
      "ttr_trend": "steady",
      "pages": 3,
      "night_pages": 1,
      "unanswered_pages": 0
    }
  ],
  "people": [
    {"person": "oncall@example.com", "pages": 4, "night_pages": 2, "acknowledged": 4, "hours_on_call": 168}
  ]
}
```

- `incidents` counts incidents opened in `[since, until)`; `open` is how
  many of them are not yet resolved.
- `mtta_seconds` is the mean of acknowledged minus opened over the
  `acknowledged` incidents; `mttr_seconds` the mean of resolved minus
  opened over the `resolved` ones. Either is `null` with none.
- `ttr_trend` compares the mean restore time of the window's second half
  with its first: `improving` (down by a fifth or more), `worsening` (up
  by a fifth or more), `steady`, or absent with fewer than two restored
  incidents on either side.
- `services` is sorted by incidents, most first, then by name. A service
  with pages but no incident in the window is listed too.
- `pages` counts the chains' pages started in the window, `night_pages`
  those started between 22:00 and 07:00 in the desk's zone (General settings),
  and `unanswered_pages` those nobody acknowledged; on `total` and each
  service.
- `people` is each person's load: `pages` that told them, `night_pages`,
  `acknowledged`, and `hours_on_call` on every schedule, most pages
  first.

## The status page

With `STATUS_ADDR` set, a third port serves a public page and
`GET /status.json`. It shows the components the settings list, by the
names given there, each `operational`, `degraded` (a warning open on it),
or `down` (a critical open on it); open incidents marked public, by title
and state; their notes marked public; and public incidents resolved in the
last week. It never shows an owner, a label, a service key, or an internal
note.

Anyone may **follow** it by email: the address is mailed a link to
confirm, and nothing else comes until it does. A confirmed follower is
mailed when a public incident opens or changes state, and each public
note, each mail with a link to stop. The page answers the same whatever
address is given. Its links point at `STATUS_URL`.

## Notifications

dispatch delivers notices for every product, not only its own incidents.
A product sends what happened as an **event** (`POST /v1/events`); a
**subscription** sends the events it takes to a **channel**; every
**delivery** is a row before anything is sent, so a restart loses nothing.

- A **channel** is a place to post: `teams`, a Teams channel's Workflows
  webhook, which gets an Adaptive Card with the event's title, facts, and
  link buttons; `discord`, a Discord channel's webhook, which gets the same
  card as an embed; `discord-bot`, a channel of a server the Discord bot
  is in; or `webhook`, which gets `{"event": {...}}`. Its URL is a
  secret, sealed with `SECRET_KEY` and never shown again. Without
  `SECRET_KEY` no channel can be added. **Send a test** posts one now and
  shows the channel's answer.
- A `webhook` channel **signs** every post. It gets a signing secret when
  added, shown once and sealed after. Each post carries
  `X-Dispatch-Signature: t=<unix seconds>,v1=<hex>`, where `v1` is
  HMAC-SHA256 of `<t>.<body>` under the secret, and `X-Dispatch-Delivery`,
  the delivery's id. A receiver checks a `v1` against its secret and
  refuses a `t` more than five minutes off. **New secret** adds a second
  signature beside the first, so the receiver changes over with no gap,
  until **Drop the old secret**. A channel added before signing signs
  once given a secret.
- A **subscription** takes an event when every part it gives holds:
  **topics**, globs on the event's kind (`action.*`, `incident.opened`,
  `*`); **producers**, the token names it takes events from; **about**, a
  catalog id such as `program:4242`, which takes events about that entity
  and about anything that reaches it through its relations, up to two hops
  (an app service whose `program` is it); and **labels**, `label=glob`
  pairs the event's labels must match. An event goes to a channel once,
  however many of its subscriptions take it. A subscription can be paused.
- The **producer** is the name of the token that sent the event, never
  the body's word, so one product cannot speak as another. A second event
  with the same producer and `key` is a repeat and goes nowhere.
- An event's **weight** says how hard it pushes on people: `page` works up
  each chain it reaches until someone acknowledges, `notify` reaches
  people by their notify rule once their quiet hours end, and `notice`
  only lands in their list on **Me**. Left out, it is `page` when the event
  reaches a chain and `notify` otherwise. Channels take every weight.
- A product **declares its kinds** with `PUT /v1/kinds`: each kind it
  sends, with a title and a sentence people read, and the weight it
  usually carries. The producer is the token's name, as for an event, and
  a kind belongs to the first producer that declared it. Each declared
  kind is a row on everyone's **Me**. Left alone, a row follows the
  person's rule for the event's weight. A row the person set wins over
  that rule for events of that kind from that producer, whether the event
  names them, a group they are in, or a chain: they are told by the media
  they ticked, all at once, or by none with **List only**, which keeps the
  event on their list. A choice never touches a page. An event sent with
  weight `page` goes by the person's page rule whatever the row says, and
  a kind declared as `page` shows as fixed. A choice also never sends what
  a maintenance window quieted, and what it sends still waits out quiet
  hours and the notify budget. Channel subscriptions are not changed by
  any of this.
- A **dedup key** makes events a **thread**, moved by the event's
  `action`: the first `trigger` opens it and reaches everyone, a later
  trigger reaches the channels and tells no person again, `acknowledge`
  ends its pages, and `resolve` closes it, so the next trigger opens a new
  one. An acknowledge or resolve with no open thread moves nothing.
- A **delivery** keeps the channel's answer as proof it took the message.
  A refusal or a network failure is tried again after 30 seconds, 2, 10,
  and 30 minutes, and an hour, then fails for good; a 4xx other than 408 or
  429 fails at once. The URL never appears in an error.
- A `bot` channel is a channel of a Teams team that dispatch's Teams bot
  is in, picked from a list on **Notifications**: no webhook to make. The
  bot posts the same card, and an event's `actions` become buttons. Whoever
  clicks one signs in as themselves (silently, through Teams, after the
  first time) and the bot calls the producer's API with that person's own
  token, so the producer decides whether they may. The card then says who
  did what; a refusal shows only to the person who clicked. Only the
  producer named by `BOT_ACTS_FOR` gets buttons; a `teams` webhook card
  never has them.
- The desk's own incidents are events too: `incident.opened` and
  `incident.state_changed`, about the Orrery workload when the namespace
  resolved to one, labelled `service`, `severity`, and `state`.

Admins manage channels and subscriptions, and read the newest deliveries,
on **Notifications**. One replica sends; the store is one SQLite file.

### People, groups, and chains

Events also reach people, by email and in Teams.

- A **person** is an address: someone, or a distribution list. Everyone
  who signs in with a desk role is kept as one, and sets how they are
  reached on **Me**; someone with no desk role is kept when they first
  save there; admins add others on **People**. A person has an SMS number,
  a Discord account, and Teams and email by their address. Their **rules**
  give each weight its media in order, each after a wait, such as
  `teams, sms+5` (a later step of a page goes only while the page waits).
  Without a rule, a page takes every medium they have, notify every one
  but SMS, and a notice none. **Quiet hours** hold everything but a page back
  until they end; a page never waits. An address not on People is sent email.
- Saving an **SMS number** on Me needs the person's opt-in: a box they tick
  beside wording that names the sender (`SMS_SENDER_NAME`), says how often
  texts come, that rates may apply, and that STOP opts out and HELP helps,
  with a link to `SMS_POLICY_URL` when it is set. The desk keeps when they
  opted in and shows it back on Me. Clearing the number needs no box; a new
  number, or one an admin sets on People, has no opt-in until the person
  gives it there. People asks admins to add a number only at the person's
  request.
- A **group** is a slug, a name, and its members' addresses.
- A **chain** is a slug, a name, ordered steps, and how many times it
  starts over. Each step names people, groups, and schedules, and a wait
  in minutes.
- A **schedule** says who is on call: layers whose members take turns of
  hours, days, or weeks, a later layer winning where it has someone, and
  each layer optionally kept to hours of the day in the schedule's zone,
  so layers follow the sun. A **cover** puts one person on call above
  every layer for a while; two covers are a swap. `schedule:slug` in a
  chain's step, an event's `to`, or a subscription reaches whoever has it
  at that moment. Whoever comes on call is told (`schedule.handoff`), and
  a gap in the next week is told once a day (`schedule.gap`). Each person
  can make a calendar link to their shifts on **Me**.
- An event reaches them through its `to` (`[{"person": "a@example.com"},
  {"group": "platform"}, {"chain": "platform-oncall"}]`, at most 20), or
  through a subscription whose target is a person, group, or chain
  instead of a channel. A person is sent an event once per medium,
  however many routes reach them.
- A chain starts a **page**: its first step is told at once, and when a
  step's wait runs out with nobody acknowledging, the next step is told.
  After the last step's wait the chain starts over as many times as it
  repeats, and then the page ends unanswered. Every chain
  message carries a link to `/pages/{id}` on the desk, behind the front
  door, where anyone on the desk acknowledges it or tells the next step
  now; nobody further is told after an acknowledge. Acknowledging an
  incident on the desk or the API ends the pages its events started.
- On an incident, **Tell the next step now** escalates every page about
  it, and an acknowledged incident can be **snoozed**: the person is
  paged again after the time they pick unless it is mitigated or resolved
  by then.
- A **room**: with the Teams bot and `INCIDENT_TEAM_ID` set, an incident
  at least as bad as `INCIDENT_ROOMS_FROM` (critical by default) gets its
  own channel in that team, made through Microsoft Graph as the bot's
  Entra app (which needs Graph's `Channel.Create` application
  permission), and its state changes and notes post there as the bot. Any
  incident gets one from its page.
- An **action item** opens an issue, with `GITHUB_TOKEN` set, in the
  repository Machines gives its incident's service, and is ticked off
  when the issue closes.
- **Interruption budgets** (General settings): a person past their pages in
  seven days has notify-weight messages wait for 07:00 in the desk's
  zone, and a service past its pages in seven days is told once a week
  as a `budget.exceeded` notice.
- An acknowledge **flows back** to the source. A page about an incident
  acknowledges the incident as the person. Any other page acknowledges
  its event's thread, and dispatch sends a `page.acknowledged` notice,
  labelled with the `producer`, `kind`, `event_key`, and `dedup_key`, for
  the producer to subscribe a webhook to.
- **Email** goes through Microsoft Graph or an SMTP server, whichever is
  set (Settings below), as `MAIL_FROM`, with the event's title, text,
  facts, links, and the acknowledge link. Graph signs in as an Entra app
  with the pod's federated workload token, never a secret; the app holds
  Exchange's `Application Mail.Send` role scoped to the one mailbox.
- **Teams** is a chat from the bot. The person's Teams account is found
  once in the rosters of the teams the bot is in and kept; someone in
  none of them is skipped in Teams, not tried again. Teams has no
  supported way for a bot to send an urgent message, so a Teams page
  rings like any chat; let Teams through your phone's Do Not Disturb.
- **SMS** goes from the desk's Azure Communication Services number, signed
  in as an Entra app with the pod's federated workload token, never a
  key. A page's text says how to answer: reply `ACK` (or A, OK, YES) to
  take the page that last texted you, `ESC` (or E, NEXT) to tell its next
  step now; the desk texts back what it did. Only a number on someone's
  **Me** is heard. A text is at most two segments.
- **Discord** is a direct message from the bot, which needs the person to
  share a server with it: a card, and for a page, **Acknowledge** and
  **Pass it on** buttons that act as the person whose account pressed.
- Every message is a row before it is sent, tried again like a channel's
  delivery, and listed on **Notifications** with its answer. Its
  **receipt** goes past sent where the medium says: SMS delivery reports
  mark it on the device, and opening the page's link on the desk marks it
  opened. The page's view lists who was told, by what, and how far each
  got.

The older single webhook stays: with `NOTIFY_WEBHOOK_URL` set, dispatch
also POSTs one JSON message on open and on every state change, signed the
same way when `NOTIFY_WEBHOOK_SECRET` is set:

```json
{"event": "incident.opened", "at": "...", "incident": {...}, "actor": "dispatch", "url": "https://dispatch.example.org/incidents/12"}
{"event": "incident.state_changed", "at": "...", "incident": {...}, "from": "open", "actor": "who", "url": "..."}
```

## Changes

A **change** is one act that alters a live thing: a deploy, an apply, a
rotation. The desk keeps each one by its **service**, as a producer names
it, and by the **things** it touched, the catalog entities by blueprint
and key. A change also carries its outside reference: the **run** that
made it, such as a pipeline run's URL, and its **record**, the sender and
key of its `change.stated` event. A change is kept once per record.

Changes arrive two ways:

- `POST /v1/changes`, for anything that can post JSON. Its decoder is
  strict: a field the contract does not name is refused, so a caller
  learns at once that a field it sends goes nowhere.
- `POST /v1/change-events`, the suite's five change events (platformkit's
  `change` package and its JSON Schema). Each is decoded strictly with
  `change.Decode` and checked with the contract and the estate's effect
  kinds. Its `sender` is the token's name, as an event's producer is. An
  event is kept once by its sender and key; a second send is a repeat. A
  `change.stated` event makes a change keyed to the statement's things,
  carrying its digest and its predicted effects; `change.routed`,
  `change.decided`, `change.ended`, and `change.observed` attach to it by
  their `of`, whichever arrives first. Such a change is sent on as a
  `change.<kind>` notice once, when its `change.ended` says it ran (`ok`,
  or `failed` part way), at its end; a change refused or never run is not
  announced. Once ended, a change's `at` is its end, when it took effect,
  so incidents near it are joined from then. A batch is kept whole or not at
  all: one refused event answers 400 with its index and reason, so a
  sender can split the batch to find it. Only a token issued on
  **Machines** may send events; a personal token or the shared secret is a
  403.

An incident is **joined** to a change by catalog entity. An incident on a
service is on the Orrery workload its namespace resolved to, and on every
entity that workload **leans on**. Leaning is read from the catalog's
relations, as the routing document's `settings.changes` names them:

```yaml
settings:
  changes:
    # A relation whose source leans on its target, or "<-relation" for one
    # whose target leans on its source. "blueprint:relation" and
    # "<-blueprint:relation" count only edges from that blueprint.
    leans_on: [depends_on, consumes, services, "<-azure_resource:service"]
    depth: 2
    # The estate's own effect kinds, and base kinds whose watch it changes.
    # A base kind keeps its family and unit; only its watch may differ.
    kinds:
      - {name: stops, family: disruption, means: It stops or restarts., unit: copies, watch: 45m}
```

The incident's page lists every change to its service, its workload, or
what the workload leans on, from two hours before it opened. With the
catalog out of reach the page lists those to the service and the workload
alone, and says so in the log.

### The watch

The watch learns what a change did, apart from the change, so watching
adds no time to it. It starts when a change is both stated and ended, and
ran: `change.ended` with outcome `ok`, or `failed`, since a failed apply
may have changed part of what it touched. A change refused before it ran,
or one that states no effect of a watched kind, starts no watch; the log
says which.

- **What it watches.** Each subject of each disruption effect, and each
  thing that leans on it (the catalog's relations, as `leans_on` names
  them), for the kind's **window**: the kit's base kinds watch `stops` for
  30 minutes and `cuts` for 2 hours. An estate changes only a kind's
  watch, in `settings.changes.kinds`.
- **What it reads.** The estate's own signals: restarts, failed health
  checks, refused callers, the error rate. It reads them through a port
  (`internal/signals`) with an adapter for each store: Prometheus, Azure
  Monitor (Log Analytics, read as the pod's workload identity), and Azure
  platform (each resource's own platform metrics and Resource Health,
  read through Azure Resource Manager as the pod's workload identity, with
  no workspace and nothing set up per resource). Which query reads which
  signal for which subjects is in the routing document, never in code:

  ```yaml
  settings:
    changes:
      watch:
        stores:
          prom: {kind: prometheus, url: "http://prometheus.example.org:9090"}
          logs: {kind: azure-monitor, workspace: 00000000-0000-0000-0000-000000000000}
          azure: {kind: azure-platform}
        queries:
          - blueprint: k8s_workload
            signal: restarts
            kinds: [stops]
            store: prom
            query: 'sum(increase(kube_pod_container_status_restarts_total{namespace="{namespace.title}", pod=~"{name}-.*"}[{window}]))'
            above: 0
          - blueprint: azure_resource
            where: {type: Microsoft.Storage/storageAccounts}
            signal: availability
            kinds: [stops]
            store: azure
            query: 'metrics {resource_id} Availability minimum'
            below: 100
  ```

  A query takes the subject's catalog fields as parameters, each quoted
  as the store's language quotes a string: `{key}`, `{title}`,
  `{blueprint}`, any field (`{name}`, and `{labels.app}` for a part of an
  object field), a relation's first target (`{namespace.key}`,
  `{namespace.title}`), and the first thing of a kind that points at it
  (`{<-k8s_namespace:workload.title}`). `{window}` is the kind's window in
  seconds, such as `1800s`. `where` names parameters by the same names and
  the value each must equal, so one blueprint takes different queries by
  a field such as `type`; a subject no query matches is blind. A
  Prometheus query is read at the window's end; an Azure Monitor one and
  an Azure platform one over the window. A number above `above` or below
  `below` is harm; a query that sets neither is harm above 0.
- **Azure platform queries.** One line, in one of two forms:

  ```text
  health <resource>
  metrics <resource> <metric> <aggregation> [namespace <ns>] [grain <grain>] [seen <metric>] [where <filter>]
  ```

  `health` reads the resource's Resource Health history (Availability
  Statuses, List) and answers the minutes it was `Unavailable` or
  `Degraded` in the window; a window partly `Unknown`, or older than the
  history, is no data unless it saw harm, and a type Resource Health does
  not cover is a failed store. `metrics` reads one platform metric at a
  one-minute grain, or the `grain` it names for a metric that keeps no
  minutes (an ISO 8601 duration such as `PT5M`), and answers it under its
  aggregation across every point and series: the sum for `total` and `count`, the least for
  `minimum`, the greatest for `maximum`, the mean for `average`. `where`
  is the Metrics API's own `$filter` on the metric's dimensions, to the
  end of the line, such as `ResponseType eq 'AuthorizationError' or
  ResponseType eq 'ClientThrottlingError'`. Azure sends no series for a
  filter that matched nothing, so `seen`, for a total or a count, names a
  metric whose points prove the resource reported: with none of its own,
  the read is 0 when the seen metric has points, and no data when it has
  none. A parameter inside a filter is quoted as OData quotes a string.
  The identity needs `Microsoft.Insights/Metrics/Read` and
  `Microsoft.ResourceHealth/AvailabilityStatuses/read` where it reads.
  Reading platform metrics is billed per call past a free monthly
  allowance (Azure Monitor pricing, "Platform metric queries"); Resource
  Health is free.
- **Seen, clean, or blind.** Each kind on each subject is recorded as
  `harm`, `clean`, or `blind`, with the signals it read. Blind is never
  clean: a subject whose blueprint has no query, a store with no adapter,
  a parameter the subject lacks, a query that found no data, and a store
  that failed are all blind. One query that saw harm makes the kind harm;
  otherwise one blind query makes it blind.
- **Incidents joined.** An incident that opens inside the window on a
  thing the change touched, felt, or that leans on one is joined to the
  change.
- **The verdict is an event.** When the window closes the desk writes
  `change.observed` (the kit's `Observed`: each sight with the path the
  change took, and the joined incidents), keeps it with the change, and
  saves it once for each place it goes: Annals, through platformkit's
  `annals` client, and the catalog, `POST /v1/ingest/change-events` on
  `ORRERY_URL` with `{"events":[...]}` and the Orrery source token. A
  worker hands each one on until it is taken. A refusal of what the event
  says is final, since sending it again never changes it: Annals refusing
  the event (422, or the kit's own check), or the hub answering 400 with
  the `index` of the event it refused. The send is kept as `refused` with
  the reason, logged, and never tried again. Any other failure (no answer,
  a timeout, a 5xx, a 404, a 400 that names no event) is tried again after
  30 seconds, 2, 10, and 30 minutes, then every hour. None is dropped, and
  a send never holds up the watch.
- **It survives a restart.** A watch is a row from the moment the change
  ends; the worker closes each one whose window has passed, however long
  the desk was down. When the catalog cannot say what leans on a subject,
  the watch waits up to half an hour past its window, then closes without
  those things and logs it.

`GET /v1/changes` carries each change's `watch`: its state (`watching`,
then `done`), its window, its verdict, its sights and incidents, and
whether each place took its observation.

Two alerts come from the watch, raised through the desk's own alert path
by the producer `dispatch`, as a late heartbeat's alert is:

- `ChangeMissed`: a change routed to no one whose watch saw harm, or a
  change an incident was joined to.
- `ChangeUnwatched`: a change that ended with no watch at all. Either its
  statement never came (ten minutes after its `change.ended`), or every
  kind on everything it touched was blind.

Both are `warning`, so each is a notice to the channels as
`alert.firing`, labelled with the `change`, and pages nobody (Only an alert
that pages opens an incident). The weight is chosen because neither is
harm happening now: a miss is evidence that a gate rule let harm through,
and the harm itself pages through its own alerts and incidents; a change
with no watch is a blind spot. Both are fixed in working hours, in the
gate's rules or the watch's queries, and the weekly score counts them. An
estate that wants either to page writes a route that raises it
(`alertname=ChangeMissed => page`).

## Routing as a file

The desk's routing can live in git as one YAML document: settings (with
how changes are read), people, groups, chains, schedules, subscriptions,
and heartbeats.
`GET /v1/config` answers it; `PUT /v1/config` makes the desk match it.
The whole document is checked before anything changes; a section given
replaces what the desk has and a section left out is left alone, except
that people are added or changed and never removed. A subscription names
its channel; a channel's URL is a secret added on **Notifications**. A
personal token may not apply a document. The binary does both:

```bash
DISPATCH_URL=https://dispatch.example.org DISPATCH_TOKEN=... dispatch export > routing.yaml
DISPATCH_URL=https://dispatch.example.org DISPATCH_TOKEN=... dispatch apply routing.yaml
```

## The assistant

With `ASSIST_BACKEND` set, an incident's page offers the assistant,
which reads and writes and never acts. **Catch me up**, **Draft a status
update**, and **Draft the postmortem** each send Claude the incident's
record (fields, roles, alerts, every timeline entry, changes to its
service, and the catalog's word on its workload) and keep what it wrote
on the page. **Investigate** also lets it call read-only tools: the
incident's record, changes to a service, and the read tools the catalog
hub lends it (below), at most twelve calls. Every call and its answer is
kept as numbered evidence, and the finding must cite it. Whatever it
suggests doing is for a person to do. Each ask is posted to Annals as
`assist.<kind>` by the person who asked. It asks `claude-opus-5-5`
(`ASSIST_MODEL` names another).

The assistant asks Claude through the Ion engine, which the image
carries with the Claude Code CLI. `ASSIST_BACKEND` decides how Ion
reaches Claude, and nothing else changes:

- `claude-code`: a Claude subscription, through the CLI. Give the pod
  `CLAUDE_CODE_OAUTH_TOKEN`, and mount Claude Code's managed settings at
  `/etc/claude-code/managed-settings.json` with the same token and the
  deny list below, so a rotated token reaches the next run without a
  restart.
- `api`: an Anthropic API key in `ANTHROPIC_API_KEY`. With
  `ASSIST_BASE_URL`, Ion sends Anthropic's API to that gateway instead,
  such as an enterprise AI gateway, and the key is the gateway's.

Dispatch holds no reader of logs, metrics, or anything else outside its
own record. An investigation's data comes through the **hub's tool
door**: a tool there is a command that runs on one of the hub's runners
with the runner's key, so the assistant holds no key to any system. With
`ORRERY_URL` and `ASSIST_TOOLS_TOKEN` set, each investigation asks the
hub which tools it lends for the incident's service
(`GET /v1/tools?entity=sw_workload:<workload>`) and which run on no
entity (`GET /v1/tools`), and takes only those of tier `read`. The token
is a hub access token whose role holds only `tools.read`, so the hub
lists and runs nothing else for it; dispatch leaving out any other tier
is a second lock. The assistant is told each tool's id, title,
description, the entity it runs on, and a JSON Schema of its inputs, and
calls one through the single tool `hub_tool` with
`{"tool": "<id>", "inputs": {...}}`, which dispatch sends as
`POST /v1/tools/{id}/call`. A tool the hub did not list for this
investigation is refused without a call. What the tool printed is the
answer, with a line saying so when the hub cut it at 64 KB; a run that
did not succeed, a missing tier (403), and a runner that does not serve
the tool now (409) are tool errors the assistant reads. The evidence
keeps each call under the hub tool's id. Without the URL or the token,
or when the hub lends nothing, the investigation runs on the incident's
record alone and the log says so.

The investigation's tools are served from inside dispatch over MCP on
the loopback interface only, as the server `dispatch`. Two locks keep
the assistant to them. Dispatch writes Ion's permissions to deny every
tool but `mcp__dispatch__*` and `ToolSearch`, which both backends obey.
On `claude-code`, the managed settings also deny every tool of the CLI's
own and every tool Ion lends it:

```json
{"permissions": {"allow": ["mcp__dispatch", "ToolSearch"],
 "deny": ["Bash", "BashOutput", "KillShell", "Monitor", "Read", "Write", "Edit", "MultiEdit",
          "NotebookEdit", "Glob", "Grep", "WebFetch", "WebSearch", "Task", "Agent", "Skill",
          "SlashCommand", "Workflow", "mcp__ion-extensions"]}}
```

## Agents

Each person can make a token that acts as them on **Me**. On the API, a
change made with it is that person's, as on the desk. The API also serves
the desk as an **MCP server** at `POST /mcp` (streamable HTTP, stateless),
behind the same token check, so an agent can work the desk beside its
person without copying anything in or out.

Any token reads:

| Tool | Answers |
| --- | --- |
| `board` | Who the token acts as, who is on call, open incidents by severity, the unresolved incidents its person owns, the ones nobody owns, the ones others own, the ones settling (every alert stopped, each closing itself at its `closes_at`, so none is work), their open action items, and the postmortems they owe (resolved in thirty days, none written). |
| `list_incidents` | Incidents by `state` (a comma list or `all`), `owner` (an email, `me`, or `nobody`), `service`, `severity`, `q`, `limit`. |
| `get_incident` | One incident with its alerts, timeline, roles, links, changes near it, the catalog's word on its workload, the moves it allows now, its postmortem and action items, and templates for a status update and a postmortem. |
| `list_action_items` | Action items not done, every owner's or one `owner`'s. |
| `who_is_on_call`, `outcomes` | As `GET /v1/schedules` and `GET /v1/outcomes`. |
| `send_event` | Sends an event as the token. |

A personal token also acts, as its person: `open_incident`,
`assign_incident` (`me` takes it, `nobody` clears it), `move_incident`,
`add_note` (`public` makes it a status update), `set_severity`,
`set_public`, `set_role`, `link_incident` (or merge), `save_postmortem`,
`add_action_item`, `set_action_item_done`, `my_list`, and
`acknowledge_page`. Every change lands on the timeline under the person's
name, as it would from the desk, and answers with the incident as it
stands after it. Any other token is refused.

## Orrery

With `ORRERY_URL` and `ORRERY_PUSH_TOKEN` set, every incident is pushed to
the hub as an `it_incident` entity, keyed `dispatch-<id>`, related to its
workload when the namespace resolved to one, so Orrery shows open
incidents on the thing. The blueprint is offered first on the ingest API
(`PUT /ingest/blueprints`), and is also written as model YAML in
`deploy/orrery-catalog/model/incidents.yaml` for the catalog to import.
`ORRERY_TOKEN` is the read token for resolving namespaces and for
walking the catalog's relations to see what leans on what (Changes);
when it is unset, the push token is used for reads as well. The calls go through
platformkit's `orrery` client, one per token.

The watch posts each `change.observed` to the hub's
`POST /v1/ingest/change-events` with the push token, so the catalog keeps
each thing's proof beside it.

The app does not describe itself to the hub. `orrery.yaml` at the
repository root says what this repository builds: the service
`github/orchard-suite/dispatch/cmd/dispatch`, the pillars it covers, the APIs it
provides and calls, and its logo, `logo.svg`. Muster, the forge reader,
reads it from the repository and writes them into the catalog.

## Annals

With `ANNALS_URL` and `ANNALS_TOKEN` set, every decision dispatch makes is
posted to Annals as one event, through platformkit's `annals`
client, under the source `dispatch`. Incident decisions:

| Verb | When | Actor |
| --- | --- | --- |
| `incident.open` | Opened on the desk, on the API, or by Alertmanager | The person (`person`, their email); the token's name (`system`); or `alertmanager` (`system`) |
| `incident.acknowledge`, `incident.mitigate`, `incident.resolve`, `incident.reopen` | A state change | The person on the desk, or the token on the API, with the body's `actor` as `on_behalf_of`. Alertmanager reopening a resolved incident is `alertmanager`; the auto-resolve rule is `dispatch`. |

```json
{
  "at": "2026-11-01T12:04:00Z",
  "source": "dispatch",
  "event_key": "incident:12:acknowledge:87",
  "actor": {"kind": "person", "id": "oncall@example.com"},
  "action": {
    "verb": "incident.acknowledge",
    "target": {"system": "dispatch", "kind": "incident", "id": "12", "title": "Checkout failing"},
    "outcome": "ok"
  },
  "trace_id": "4bf92f3577b34da6a3ce929d0e0e4736",
  "labels": {"severity": "critical", "service": "shop"}
}
```

Routing decisions, each keyed so a retried post counts once:

| Verb | When | Actor |
| --- | --- | --- |
| `notice.accept`, `notice.repeat`, `notice.drop` | An event taken (labelled with its kind, weight, move, how many channels, messages, and pages it made, and for how many people their own choice for the kind decided how they were told, as `chosen`), a repeat of a key, or an acknowledge or resolve that moved nothing | The producer (`system`) |
| `page.start`, `page.escalate`, `page.acknowledge`, `page.unanswered` | A chain's page started, told its next step (by its wait, or by a person), was acknowledged, or ran out | The producer, `dispatch`, or the person |
| `delivery.give_up`, `message.give_up` | A send to a channel or a person failed for good | `dispatch` |
| `person.save`, `group.save`, `chain.save`, `channel.add`, `subscription.add`, `subscription.pause`, `token.issue`, `settings.save`, and each `.remove` | An admin, or a person on **Me**, changed how notices are routed | The person |
| `person.choose` | A person set, on **Me**, how they hear of one kind of event: labelled with the `kind` and `media` (the media, `list` for list only, or `rules` when they cleared the row) | The person |
| `kinds.declare` | A product replaced the kinds it declares: labelled with how many `kinds` it now has and how many it `dropped` | The producer (`system`) |
| `change.observed` | The watch closed on a change: its target is the change (the system and key of its `change.stated` event), its `cause` that event, each sight an effect with its `verdict`, each joined incident an item, labelled with the `watch` and how many sights were `clean`, `harm`, and `blind`. Saved and handed on until taken, unlike the decisions above | `dispatch` (`system`) |

Retries are not decisions and are not posted. `event_key` of an incident
decision ends in the id of the timeline entry the change wrote, so a
retried post is stored once and a second resolve after a reopen is stored
again. A post never blocks or fails the change: it runs on its own, and a
refusal or a failure is logged and dropped, never retried.

## Telemetry

JSON logs on stdout with `trace_id`. With `OTEL_EXPORTER_OTLP_ENDPOINT`
set, spans and metrics go over OTLP. platformkit's `telemetry` and `faro`
packages provide the logger, the export, `http.server.request.duration`,
`dispatch.build_info`, `dispatch.browser.batches`, and the process
metrics; the rest are the desk's own, in `internal/metrics`:

| Metric | By |
| --- | --- |
| `http.server.request.duration` | route, method, status |
| `dispatch.build_info` | version |
| `dispatch.incidents.opened` | severity, service |
| `dispatch.incidents.open` (gauge) | severity |
| `dispatch.time_to_acknowledge_seconds` (histogram) | severity, service |
| `dispatch.time_to_restore_seconds` (histogram) | severity, service |
| `dispatch.webhook.received` | source, outcome |
| `dispatch.browser.batches` | outcome |
| `dispatch.notifications.sent` | outcome |
| `dispatch.deliveries` | outcome: `sent`, `retry`, `failed` |
| `dispatch.messages` | outcome: `sent`, `retry`, `failed`, `skipped` |
| `dispatch.pages` | outcome: `started`, `escalated`, `acknowledged`, `unanswered` |
| `dispatch.alerts` | source, outcome: `incident`, `notice`, `quieted`, `dropped` |
| `dispatch.heartbeats` | outcome: `beat`, `late`, `back` |
| `dispatch.replies` | source (`sms`, `discord`), outcome: `acknowledged`, `escalated`, `unknown`, `refused` |
| `dispatch.receipts` | source, outcome: `delivered`, `undelivered` |
| `dispatch.assist` | outcome: `done`, `failed` |
| `dispatch.orrery.pushes` | outcome |
| `dispatch.annals.posts` | outcome: `posted`, `refused`, `failed` |
| `dispatch.changes` | source (the producer), outcome: `kept`, `repeat` |
| `dispatch.change_events` | source (the sender), outcome: `kept`, `repeat`, `refused` |
| `dispatch.watches` | outcome: `started`, `skipped`, `observed`, `missed`, `unwatched` |
| `dispatch.watch.sights` | outcome: `clean`, `harm`, `blind` |
| `dispatch.watch.sends` | source (the target: `annals`, `orrery`), outcome: `sent`, `retry`, `refused` |
| `process.cpu.time`, `process.memory.usage` | state, kind |

## Settings

| Variable | Means |
| --- | --- |
| `PUBLIC_URL` | The desk's URL. Required. |
| `DATA_DIR` | Where `dispatch.db` lives. Default `/data`. |
| `SESSION_ADDR`, `API_ADDR`, `STATUS_ADDR` | Listen addresses. Defaults `:8080`, `:8081`, and off. |
| `ALERTMANAGER_TOKEN` | The shared secret the API port accepts beside issued tokens. |
| `DESK_ROLE`, `ADMIN_ROLE` | Bare front-door roles. Defaults `User`, `Admin`. |
| `SECRET_KEY` | Seals channel URLs. Without it no channel can be added; changing it leaves saved channels unopenable. |
| `NOTIFY_WEBHOOK_URL` | Where the older single incident webhook goes. Off when empty. |
| `NOTIFY_WEBHOOK_SECRET` | Signs the older webhook's posts like a webhook channel's. A secret. |
| `ORRERY_URL`, `ORRERY_TOKEN`, `ORRERY_PUSH_TOKEN` | The hub's ingest base URL, a read token, a source token. Without a read token the source token reads too. The source token pushes incidents, so its source needs the `it_incident` blueprint. |
| `BOT_APP_ID` | The Teams bot's single-tenant Entra app. Turns the bot on: `POST /api/messages` on the API port and `bot` channels. Off when empty. |
| `BOT_TENANT_ID`, `BOT_TOKEN_FILE` | The bot's tenant and the federated workload token it signs in with. Default `AZURE_TENANT_ID` and `AZURE_FEDERATED_TOKEN_FILE`. |
| `BOT_CONNECTION` | The Azure Bot's OAuth connection that gives a person's token for the product. Default `orrery`. |
| `BOT_ACTS_FOR` | The producer whose events' actions the bot shows; a click calls `ORRERY_URL` as the person. Default `orrery`. |
| `MAIL_FROM` | The mailbox email comes from. Required with either transport. |
| `MAIL_GRAPH_APP_ID` | Sends email through Microsoft Graph as this Entra app, signed in with the federated workload token. It needs Exchange's `Application Mail.Send` role on `MAIL_FROM`. |
| `MAIL_GRAPH_TENANT_ID`, `MAIL_GRAPH_TOKEN_FILE` | Where the mail app signs in and its token file. Default `AZURE_TENANT_ID` and `AZURE_FEDERATED_TOKEN_FILE`. |
| `SMS_ENDPOINT`, `SMS_FROM` | The Azure Communication Services resource (`https://name.communication.azure.com`) and its number in `+country` form. Turns on SMS and `POST /v1/sms`. |
| `SMS_SENDER_NAME` | Who the SMS opt-in on Me says texts the person, such as a team or company name. Default `this desk`. |
| `SMS_POLICY_URL` | An `https` page with the SMS terms and privacy policy, linked from the opt-in on Me. Without it the opt-in links nothing. |
| `SMS_APP_ID`, `SMS_TENANT_ID`, `SMS_TOKEN_FILE` | The Entra app or managed identity that signs in to the resource, its tenant, and its federated token file. Default `AZURE_CLIENT_ID`, `AZURE_TENANT_ID`, and `AZURE_FEDERATED_TOKEN_FILE`. It needs Contributor on the resource; Azure has no narrower role that sends. |
| `AZURE_MONITOR_APP_ID` | The Entra app an action group's secure webhook gets its token for. Turns on Entra tokens at `POST /v1/azuremonitor`. The app has the `ActionGroupsSecureWebhook` app role, assigned to Azure Monitor's app (`461e8683-5575-4561-ac7f-899cc907d62a`), an identifier URI of `api://<app id>`, and only its owners may point an action group at it. |
| `AZURE_MONITOR_TENANT_ID` | The tenant of that app. Default `AZURE_TENANT_ID`. |
| `DISCORD_BOT_TOKEN`, `DISCORD_PUBLIC_KEY` | The Discord application's bot token (a secret) and its public key in hex. Turns on Discord messages, `discord-bot` channels, and `POST /api/discord`; set the application's Interactions Endpoint URL to `PUBLIC_URL/api/discord`. |
| `STATUS_URL` | The public status page's address, for its followers' links. Following is off without it. |
| `INCIDENT_TEAM_ID`, `INCIDENT_ROOMS_FROM` | The Teams team incident rooms open in, and the least severity that gets one on its own. Needs the bot. |
| `GITHUB_TOKEN` | A token that may read and write issues in the repositories Machines names. A secret. Action items open no issues without it. |
| `ASSIST_BACKEND` | Turns on the assistant: `claude-code` for a Claude subscription, `api` for an API key. See The assistant. |
| `ASSIST_MODEL` | The model the assistant asks. Default `claude-opus-5-5`. |
| `ANTHROPIC_API_KEY` | The API key, with `ASSIST_BACKEND=api`. A secret. |
| `ASSIST_BASE_URL` | With `ASSIST_BACKEND=api`, a gateway that serves Anthropic's API, such as `https://ai.example.com`. |
| `CLAUDE_CODE_OAUTH_TOKEN` | The Claude subscription token, with `ASSIST_BACKEND=claude-code`. A secret. |
| `ION_BIN`, `ION_DATA_DIR`, `ION_SOCKET_PATH` | The engine, its working directory, and its socket. The image sets `/usr/local/bin/ion` and `/ion`; keep `ION_DATA_DIR` short, because the engine puts Unix sockets under it. |
| `ASSIST_TOOLS_TOKEN` | A hub access token whose role holds only `tools.read`. With `ORRERY_URL`, it lends an investigation the hub's read tools (The assistant). A secret. Without it an investigation has no data tools. |
| `MAIL_SMTP_URL` | Sends email through an SMTP server instead: `smtp://user:pass@host:587` (STARTTLS, required before signing in) or `smtps://user:pass@host:465`. A secret; never logged. Set one transport, not both. |
| `ANNALS_URL`, `ANNALS_TOKEN` | Annals's API base URL and a token issued for the source `dispatch`. Off unless both are set. |
| `WATCH_APP_ID`, `WATCH_TENANT_ID`, `WATCH_TOKEN_FILE` | The Entra app or managed identity the watch reads Azure stores as, its tenant, and its federated token file. Default `AZURE_CLIENT_ID`, `AZURE_TENANT_ID`, and `AZURE_FEDERATED_TOKEN_FILE`. It needs Log Analytics Reader on what an Azure Monitor store reads, and metrics and Resource Health reads on what an Azure platform store reads. Without one every Azure store is blind. |
| `OTEL_EXPORTER_OTLP_ENDPOINT`, `OTEL_SERVICE_NAME` | OTLP export. |
| `FARO_URL`, `FARO_API_KEY` | The browser telemetry relay. |

## Running it

```bash
make run
curl -H 'Authorization: Bearer local-secret' --data @internal/server/testdata/alertmanager_firing.json localhost:8081/v1/alertmanager
curl -H 'X-Forwarded-Email: you@example.com' -H 'X-Forwarded-Groups: Admin' localhost:8080/
```

`make check` runs gofmt, vet, and the tests, and `make land` runs it on the
tree that lands and signs a landing proof the release verifies; CI runs it
too, before every release. Releases are cut by CI from
conventional commits on `main`; the image is `ghcr.io/orchard-suite/dispatch:<version>`.
