# Local-first Bridge control adapter

This document refines MAR-71 after Bridge enrollment landed.

## Invariant

Bridge control is optional. bridgectl remains authoritative for local execution and must continue working when Bridge or the network is unavailable.

The control adapter must not send all agent data through Bridge. It synchronizes presence and session lifecycle. Analytics continues through the durable telemetry collector.

## Development target

Use `wss://control.bridge.orchael.dev/v1/control` for development integration. Do not change the production default to the development hostname.

## First implementation milestone

After enrollment provides a dedicated control credential, the adapter:

1. loads Bridge enrollment without exposing secrets;
2. connects outbound over WSS;
3. authenticates the installation with the control-only credential;
4. negotiates protocol version/capabilities;
5. publishes an authoritative active-session snapshot;
6. publishes lifecycle changes with monotonic revisions;
7. sends heartbeats;
8. reconnects with bounded exponential backoff and jitter;
9. reconciles by sending a fresh snapshot after reconnect.

No Bridge configuration means no control connection is attempted.

## Reliability

The socket is not the source of truth. Never block or fail local Supervisor/session operations because a control send failed. Queue only bounded ephemeral notifications; if continuity is uncertain, discard the queue and reconcile from current local state.

Future output replay should read from a durable local session journal rather than relying on WebSocket delivery. Do not add unbounded in-memory transcript buffering.

## Separation of credentials

The existing `brc_` credential is telemetry-only and must not authenticate control. The Bridge enrollment protocol must add a separate installation/control credential before this adapter can authenticate.

## Deferred

Remote start/stop/input, PTY output streaming, interaction-state semantics and MCP control are later milestones. Commands must eventually route through existing Supervisor/session APIs rather than duplicate lifecycle logic.
