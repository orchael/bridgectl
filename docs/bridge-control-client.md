# Local-first Bridge control adapter

Implements MAR-71 against the merged control-plane contract from Bridge PR #16
(`docs/control-plane-v1.md` in the Bridge repo, protocol v1).

## Invariant

Bridge control is optional. bridgectl remains authoritative for local execution and must continue working when Bridge or the network is unavailable. A local Supervisor operation never waits for Bridge acknowledgement.

The control adapter does not send all agent data through Bridge. It synchronizes presence and session lifecycle only. Analytics continues through the durable telemetry collector, over a separate connection, with independent failure modes.

## Package layout

- `internal/bridgecontrol` — the WebSocket client, protocol types, revision bookkeeping, and connection-status reporting. Has no dependency on `cmd/bridgectl`.
- `internal/bridge` — `ControlObserver` (`control_observer.go`) is a small, PTY-content-free sibling of `TelemetryObserver`. `Supervisor` calls it on every lifecycle transition (start, writer-attach, detach, stop, terminal exit) via `WithControlObserver`.
- `internal/localserver` — the daemon composition root. Reads `config.Control` (an `endpoint` + `credential_file`, mirroring how telemetry's `collector_url`/`collector_credential_file` are wired) and, when both are present and the credential is well-formed, constructs the client and attaches it to the `Supervisor` before starting it.
- `cmd/bridgectl` — enrollment persistence (`bridge.go`) and `whoami`/`doctor` reporting.

## Enrollment compatibility

Bridge's device-token response gained `schema_version`, `control_endpoint`, and `control_credential` in PR #16. `schema_version < 2` (including its absence, decoding to `0`) means no control support: enrollment persists exactly as before PR #16, and `controlProvisioned(enrollment, secret)` (`cmd/bridgectl/bridge.go`) is the single source of truth used by `whoami`, `doctor`, and daemon wiring to decide whether control is usable.

An enrollment created before PR #16 stays a fully valid Bridge/telemetry enrollment. `doctor` reports `control - not provisioned — run bridgectl bridge login --force` rather than failing. Re-running `bridgectl login --force` against an upgraded Bridge is the documented migration path to pick up the new `bri_` credential; it also re-validates and re-persists organization/telemetry state, so it's safe to run even if telemetry was already configured.

The `bri_` control credential and `brc_` telemetry credential are validated with distinct regexes (`localserver.ControlCredentialPattern` / `CollectorCredentialPattern`) and are never accepted in place of one another, at both the enrollment-write path and the daemon-read path.

## Wire protocol (protocol v1, exact match to Bridge PR #16)

`internal/bridgecontrol/protocol.go` mirrors Bridge's `control/protocol.go` message-for-message: `hello`/`hello_ack`/`heartbeat`/`session_snapshot`/`session_started`/`session_updated`/`session_stopped`/`ack`/`error`, the same envelope shape, the same session status enum, and the same size limits (512 KiB messages, 500 sessions per snapshot, 128-byte message IDs, 64-byte `bridgectl_version`). `session_snapshot.sessions` is always sent as a concrete array (`[]`, never omitted or `null`) since Bridge requires an explicit empty array to report zero active sessions.

## Session revisions

Revisions are scoped to `session_id`, assigned lazily by the client **at send time** (`Client.sendEvent`/`sendSnapshot`), not at the moment a local state change occurs. This means a lifecycle notification dropped from bridgectl's bounded in-memory queue never creates a numeric gap on the wire — Bridge only ever sees a strictly increasing count of what was actually sent, which always satisfies its "current+1" contract.

`internal/bridgecontrol/revision.go`'s `RevisionStore` persists the last-assigned revision per session to `bridge-control-revisions.json` in the state directory. This matters only for a bridgectl **daemon restart** while a session is recovered (`SessionInfo.Recovered`): without it, a fresh in-memory counter would restart at revision 1, and Bridge's snapshot upsert (`revision >= current`) would silently ignore that row as stale, permanently failing to reconcile that session's true status.

## Initial snapshot and reconciliation

Immediately after `hello_ack`, the client sends an authoritative `session_snapshot` built from `Supervisor.List("")`, filtered to non-terminal sessions (`bridgecontrol.ActiveSnapshots`). The same snapshot is sent again after every reconnect and whenever Bridge sends an `error` with `code: "revision_gap"` or `reconcile_required: true`. No event replay is attempted — reconciliation is always "ask the Supervisor what's true right now," never "replay what I remember."

Per-session fields sent: `session_id`, `provider`, `project_id`, `status`, `revision`, `occurred_at`. Never PTY output, environment variables, credentials, or filesystem contents.

## Heartbeats

Sent every `heartbeat_interval_seconds` from `hello_ack` (30s from Bridge today). They establish connected/online/last-seen only — nothing about agent activity, waiting state, or progress. That's MAR-85's scope.

## Failure detection and reconnect

Two independent failure signals feed the same reconnect path:

1. An explicit read/write error (closed connection, reset, oversized/malformed message).
2. **Idle-read timeout**: every `conn.Read` is individually bounded to `3 × heartbeat_interval` (floor 30s). A silently dead connection — a network partition, a NAT/firewall black hole, a container taken off its network — does not reliably surface as a write error: a small write can be accepted into the local kernel socket buffer and report success even though nothing is actually being delivered. Bounding every read closes that gap and mirrors Bridge's own 90s server-side idle timeout for a 30s heartbeat interval. Verified against a real dev deployment: disconnecting the `control` service from its network is invisible at the TCP-write layer but is detected via this idle timeout within one bounded window.

Reconnection uses full-jitter bounded exponential backoff (`internal/bridgecontrol/backoff.go`): base 1s, cap 60s, floor 250ms, delay uniformly sampled from `[0, min(cap, base·2^attempt)]`. No unbounded retry loop, no busy loop, no unbounded memory (the event queue is a fixed-size 64-entry channel that drops the oldest pending notification under sustained backpressure rather than growing).

A `superseded` close (Bridge's generation-fencing evicting an older connection for the same installation once a newer `hello` succeeds) is treated as a normal disconnect-and-retry, not a special error path — confirmed against real Bridge behavior during testing (a stray second daemon process racing the same credential).

## Doctor / whoami

`doctor` reads a small `bridge-control-status.json` written by the client on every state transition (`connecting`/`connected`/`disconnected`/`auth_rejected`/`unavailable`), rather than opening its own probe connection: a second connection authenticated with the same installation credential would trigger Bridge's generation-fencing and evict the daemon's real, live connection. `whoami` never depends on a live network request; it reports `configured`/`not configured` from local enrollment state only.

## Deferred

Remote start/stop/input, PTY output streaming, interaction-state semantics (MAR-85: "waiting for approval", agent progress) and MCP control are later milestones. Commands must eventually route through existing Supervisor/session APIs rather than duplicate lifecycle logic.
