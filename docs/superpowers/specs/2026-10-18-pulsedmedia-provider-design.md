# PulsedMedia provider integration for idlerthing

Status: draft for approval
Target repo: idlerthing
Repo path: `docs/superpowers/specs/2026-10-18-pulsedmedia-provider-design.md`

## Intent

idlerthing tracks hosting services as inventory: specs, price, renewal dates,
labels, notes. It is passive — it stores what you enter and never contacts the
service. PulsedMedia exposes a read-only customer API that reports, per service,
the product, renewal date, recurring amount and usage figures (disk used against
quota, traffic sent and received, CPU-hours, torrent count).

This adds an optional PulsedMedia provider to idlerthing. A provider holding an
API key gets its feed fetched on a schedule, cached locally, and rendered as a
fleet view over the seedboxes linked to it. Durable fields (plan, next due,
price, quota) reconcile into the inventory; volatile figures (used bytes, CPU,
torrent count) stay in the cache and are rendered only.

The feed client is adapted from the AGPL-licensed `pmss-fleet-manager` project
(same author, same licence). Only that project's HTTPS customer-API half is
reused; none of its SSH, agent or on-box components are.

## Goals

- A PulsedMedia provider, identified by an API key, that syncs the customer-API
  feed into a local cache.
- A fleet view over the seedboxes linked to that provider, rendered from the
  cache.
- Cancelled and suspended services hidden by default and revealable with a
  toggle; they never count towards totals or cost.
- The API key never leaving the server through any read path, export or API.
- Additive to the existing seedbox model, list and forms — no replacement.

## Non-goals

- No SSH, qBittorrent, Docker or host-probe support. Nothing is executed on a
  tracked box; the feed is the only source of live data.
- No new top-level section; the fleet view lives under the seedboxes section.
- No new import/export format. `GET /export/json` and `idlerthing import`
  already round-trip a fleet.
- No alerting in v1. Threshold alerts are not ported; idlerthing's existing
  due-soon logic is unchanged.

## Anti-goals

- Any inbound listener, remote shell, or credential for a tracked box.
- The API key appearing in `/export/json`, the CSV zip, `/api/providers`, a
  rendered HTML attribute, a log line, or a world-readable database copy.
- Volatile usage figures written into the inventory columns.
- Feed data appearing on unauthenticated routes.

## Constraints

- Go 1.26 stdlib plus the existing `modernc.org/sqlite`; no new dependencies.
- The SQLite pool is a single writer connection (`SetMaxOpenConns(1)`). No
  network call may run inside a write transaction.
- Migrations are hand-rolled `PRAGMA user_version` files under
  `internal/db/migrations/`.
- Every first-party file carries the AGPL header; `make license-check` and
  `make notices-check` gate CI.

## Decisions

- The key lives on a **Provider** catalog row, not on each seedbox: the feed is
  one account-wide request, so one key and one fetch is the only shape that
  fits.
- Precedence is **the feed updates a durable field only when you have not
  changed it**, implemented as a three-way merge against the previous feed value
  (see below). No per-field dirty flag and no new columns.
- Volatile figures never touch inventory columns.

## Precedence

A "fill only when empty" rule is wrong: `next_due` advances every billing cycle,
so a field filled once would go stale and the dashboard would report the box
overdue forever. The previous feed value is already stored in the cache, so
compare against it:

```
if inventory value is empty        -> write the feed value
else if inventory == previous feed -> write the new feed value   (we wrote it)
else                               -> keep inventory              (human edited)
```

That gives updatable renewals with manual edits preserved, and needs no dirty
flag. Clearing a field by hand makes the next sync refill it — acceptable and
documented.

A service is active iff its `status` equals `"active"` case-insensitively;
everything else is hidden by default. Never enumerate cancelled/suspended
spellings.

## Design

### Data model (migration `0014`)

```sql
ALTER TABLE providers ADD COLUMN pm_api_key TEXT NOT NULL DEFAULT '';
ALTER TABLE seedboxes ADD COLUMN pm_service_id INTEGER;

CREATE TABLE pm_services (
  provider_id INTEGER NOT NULL,
  service_id  INTEGER NOT NULL,
  product TEXT NOT NULL DEFAULT '', grp TEXT NOT NULL DEFAULT '',
  domain TEXT NOT NULL DEFAULT '', status TEXT NOT NULL DEFAULT '',
  registered TEXT NOT NULL DEFAULT '', next_due TEXT NOT NULL DEFAULT '',
  billing_cycle TEXT NOT NULL DEFAULT '',
  recurring_amount REAL NOT NULL DEFAULT 0,
  currency TEXT NOT NULL DEFAULT '',
  username TEXT NOT NULL DEFAULT '', login_url TEXT NOT NULL DEFAULT '',
  permanent_address TEXT NOT NULL DEFAULT '',
  tracked_since TEXT NOT NULL DEFAULT '',
  sent_bytes INTEGER NOT NULL DEFAULT 0,
  received_bytes INTEGER NOT NULL DEFAULT 0,
  cpu_hours INTEGER NOT NULL DEFAULT 0,
  cpu_since TEXT NOT NULL DEFAULT '',
  disk_used_bytes INTEGER NOT NULL DEFAULT 0,
  disk_quota_bytes INTEGER NOT NULL DEFAULT 0,
  torrents INTEGER NOT NULL DEFAULT 0,
  generated_at TEXT NOT NULL DEFAULT '',
  synced_at TEXT NOT NULL,
  PRIMARY KEY (provider_id, service_id)
);
```

`pm_service_id` is a plain integer, deliberately not a foreign key: cache rows
are replaced wholesale each sync, and a FK would either block that or cascade
into the inventory. A stale id simply means "no feed data for this seedbox".

### `internal/pulsedmedia`

A near-port of `pmss-fleet-manager`'s `internal/pmapi`: `Feed`, `Service`,
`Access`, `Usage`, the tolerant money `Amount` decoder, `MonthsPerCycle`,
`Monthly`, `Parse`, `Fetch`. `Fetch` keeps that project's hardening:

- `X-API-Key` header sent only to the exact feed URL; a redirect is refused
  rather than followed, so the key cannot travel to a host the feed chooses.
- 30 s timeout, 4 MiB response cap, 401/403 mapped to a sentinel auth error.
- An `IDLER_PMAPI_URL` override so tests can point at a mock server.

No other package imports `net/http` for this feature.

### Sync

- Fetch first, then open the write transaction — never hold the writer across
  the network.
- Replace the provider's cache rows in one transaction. On a fetch error keep
  the previous rows and record the error for the view.
- Triggers: an explicit sync control on the fleet view, plus a refresh when the
  cache is older than 6 h. Single-flight per provider. Pages always render from
  the cache and report when it was last synced, so a slow or failing fetch never
  blocks a page load.
- After a successful sync, apply the precedence rule to the durable fields on
  linked seedboxes: `pricings.next_due_date` (`next_due`), `pricings.price`
  (`recurring_amount`), `pricings.currency` (feed `currency`), `pricings.term`
  (from `billing_cycle`), and `seedboxes.disk_as_mb` (`disk_quota_bytes`).
  Create the pricing row when the seedbox has none.

  `term` is required: idlerthing stores the price for the whole cycle and
  divides by `TermMonths(term)` for monthly cost, so a price written without its
  term reports a wrong monthly figure. Map `monthly`, `quarterly`,
  `semi-annually` (`semiannually`), `annually` (`yearly`), `biennially` and
  `triennially` to idlerthing's existing term constants; for any other cycle
  leave both `price` and `term` unwritten rather than guess. A quota of 0 writes
  nothing.

### Linking a seedbox to a feed service

Current behavior (tightened after review): on sync, a seedbox with no link is
auto-linked only when its hostname uniquely matches the provider service's
`permanent_address` host or `domain` (case-insensitive). Username substrings
and a single-service account no longer suffice: either can attach unrelated
inventory and overwrite billing data. Unmatched rows stay NULL; the seedbox
form offers a manual service dropdown for providers with a key.

### Fleet view

`GET /seedboxes/fleet` — a route under the seedboxes section, linked from the
section page. A dedicated handler and template rather than a second mode on the
generic section machinery: smaller diff, and the rows come from the cache rather
than the section's list function. Only linked seedboxes appear.

Layout follows the standard fleet pattern: headline tiles (total storage, total
traffic, next renewal, monthly cost) over per-box cards (status, disk used
against quota, sent/received, CPU-hours, torrent count, plan, next due, price).
Durable fields are read from the inventory rows, which the sync has already
reconciled, so the view performs no precedence logic of its own; only the
volatile figures come from the cache. One precedence rule, one place.

Cancelled and suspended services are hidden by default. A "show cancelled"
toggle (a query parameter, not a stored setting) reveals them muted; they are
excluded from the headline tiles and cost in both states.

### Key handling

`providers` is a name-only catalog — `CatalogItem{ID, Name}` — feeding generic
list, template, CSV and JSON paths. The key must be invisible to all of them:

- Keep the generic catalog type free of the key; read it with dedicated
  queries, not through `CatalogStore.List`.
- `/export/json` and the CSV zip never emit `pm_api_key`; a test greps a full
  export for the key value.
- `/api/providers` keeps its shape: no key, no `has_key` field.
- The catalog form follows the "write-only secret" pattern: a boolean saying a
  key is set, an empty field meaning "keep existing", an explicit clear action,
  and no echo into HTML.
- Import ignores `pm_api_key` on every path.
- Deleting a provider removes its cache rows.

### Unauthenticated routes

Seedboxes linked to a provider are excluded from `/public` in v1, so feed data
never renders outside the login wall. The licence and source routes are
untouched.

## Left to the builder

- Template and CSS reuse: existing card partials versus a new partial.
- Unit conversion: inventory stores whole MB (1024-based), the feed sends bytes.
- Whether a stale or failed sync renders a banner or a card badge.
- Timezone handling for `next_due` rollover: reuse idlerthing's existing
  rollover rather than adding a second notion of "due".

## Not in this design

- Threshold alerts (disk percentage, traffic percentage, renewal window).
- Auto-creating seedboxes for feed services with no inventory row.
- A sanitised shareable export, or a hand-authored import schema.
