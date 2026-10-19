# Optional polling parity — execution plan

Status: partial, not full parity. Spec: owner's request in this conversation to bring pmss-fleet-manager's optional polling into idlerthing. Baseline: main at 6535875. Scope is broad; never claim parity until each task is verified. Existing manual SSH load polling is the secure, default-off seam.

## Constraints
- No production hostnames, accounts, credentials, keys, screenshots of real boxes or private topology in the public repo.
- No network calls for boxes that have not explicitly opted in. Preserve host-key verification, bounded output, timeout, public-address checks and no-credential-in-export rules.
- Separate read-only polling from mutating qBittorrent, Docker and torrent actions; never make those implicit in a data checkbox.
- No automatic egress before the owner enables a specific feature. Avoid new dependencies where Go stdlib or existing x/crypto suffices.

## Task 1: Read-only host metrics
- Interface: existing `pollSeedboxLoad` / `Server.sshPoll` in `internal/web/ssh_poll.go`; seedbox detail and form in `internal/web/seedboxes.go`, templates.
- Tests first: parser accepts bounded synthetic snapshot and refuses malformed, non-finite or oversized values; disabled box does not poll. Run focused RED then GREEN.
- Add explicit per-box host CPU/load and memory toggles, migrate defaults to off, collect via one bounded SSH command, show timestamp and errors, no raw output. Validate full suite.

## Task 2: Disk and traffic metrics
- Producer: SSH snapshot of read-only PMSS quota/traffic/cgroup or /proc files. Consumer: per-box cards. Tests for missing files, units, period, null vs zero, and privacy.
- Prefer provider API where equivalent. Do not claim per-box disk I/O from one cumulative /proc/diskstats read; sample or label the interval.

## Task 3: qBittorrent observations
- Requires a read-only WebUI probe tunneled through the verified SSH client, with a bounded response, authentication guard, and explicit per-box consent. Do not add a checkbox without its collector. Test that an opted-out box never opens the tunnel and that auth failures do not trigger repeated login attempts. Requires separate design for handling WebUI credentials; no mutating endpoints.

## Task 4: Container and directory observations
- Separate opt-ins and bounded low-priority probes, with strict home scope; no implicit Docker control or unbounded `du`. Test timeouts and concurrent edits.

## Task 5: Mutating controls
- Separate action enablement and authorization design before code: CSRF, host-key pinning, confirmations, audit, per-action controls. Must not piggyback on polling consent. Tests for disabled/unauthorized, races and failure rollback.

## Task 6: Product integration and final review
- Dashboard/list/detail consistency, stale/failure state, scheduling opt-in, restore/import/export, docs and synthetic screenshots.
- Full Go suite, vet, license/notices, race where feasible, CodeQL/CI, fresh whole-branch security/quality review and regression fixes. Do not merge on pending/failing CI.

Ruling: implement tasks as separate reviewable PRs where necessary — merging a partial slice must not be represented as full pmss-fleet-manager parity. The public customer feed does not replace private host polling.
