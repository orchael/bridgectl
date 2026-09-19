# Bridge control-plane adapter

This document defines the intended boundary for MAR-71. Bridge control is optional and wraps the existing bridgectl runtime rather than replacing it.

## Connection direction

An enrolled bridgectl instance establishes an outbound authenticated connection to its configured Bridge instance. Bridge does not require a public bridgectl listener, inbound NAT/firewall changes, or Tailscale for Bridge-managed control.

The initial protocol should support:

- version/capability handshake;
- machine heartbeat and online/offline state;
- session started/updated/stopped lifecycle events;
- active-session reconciliation after reconnect;
- authorized commands targeted at existing sessions;
- bounded session output needed for interactive control;
- acknowledgements, errors, timeouts, reconnect and backoff.

Commands must route through the existing Supervisor/session APIs. The adapter must not duplicate provider, PTY, authorization, or lifecycle logic.

## Standalone behavior

With no Bridge configuration, no Bridge connection is attempted. Existing listeners, direct gRPC/SDK access, user-managed step-ca, local PKI, Docker/Kubernetes/apt deployment, and example/web behavior remain unchanged.

Bridge-managed mode must not remove the ability to keep the normal local/direct listener enabled. This permits the existing example/web and local SDK clients to coexist with Bridge control where configured.

## Separation from telemetry

The control channel carries live machine/session state and bounded interactive control traffic. It is not the analytics transport. Telemetry collection and upload remain separately configurable and must not be required to use Bridge control.

## Self-hosted Bridge

The endpoint is derived from the Bridge base URL saved during enrollment. The protocol must work unchanged against the hosted service or a customer-operated Bridge instance.

See Linear MAR-71 for implementation and regression requirements.
