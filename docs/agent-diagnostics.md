# Agent diagnostic contract

bridgectl is authoritative for local agent execution and session state. This contract gives operators and AI agents a bounded, machine-readable view of that truth without exposing transcripts, credentials, environment variables, or filesystem contents.

## CLI contract

The implementation target is:

```bash
bridgectl session diagnose <session-id>
bridgectl session diagnose <session-id> --json
```

The command MUST read the current Supervisor/session APIs rather than reconstructing state from telemetry or Bridge.

The JSON form is the stable integration surface. Start with `schema_version: 1` and include:

- session identity, provider and project ID
- runtime status
- interaction state and capability flags
- lifecycle and interaction revisions
- created/updated/last-activity timestamps when available
- pending request identity/type and provider-supplied safe summary when available
- active-writer presence, without client secrets
- local control connection status and last successful connection time when available
- the bridgectl version and diagnostic schema version

Do not include PTY output, prompts, responses, chain-of-thought, environment variables, credentials, filesystem contents, OAuth material, or arbitrary provider payloads.

## Semantics

A diagnostic snapshot answers **what bridgectl believes now**. It is not an event replay. Bridge remains an eventually consistent coordination replica and may compare its state with this snapshot.

Unknown and unsupported are explicit states. Never infer interaction state from runtime status, connectivity, terminal text, CPU usage, or elapsed time.

Revisions retain their existing meanings. Consumers may use them to identify stale replicas but MUST NOT assume lifecycle and interaction revisions share one sequence.

## Verification

Tests should construct Supervisor sessions and assert that JSON diagnostics agree with the existing public session/status APIs for starting, running, waiting for input, waiting for approval, idle, stopped and failed states where the provider supports them.

Security tests must prove forbidden raw fields cannot enter serialized diagnostics.

## Follow-up

Bridge's diagnostic API/MCP should consume this contract and compare the authoritative snapshot with its registry, telemetry/analysis metadata and API representation. Production access should use the existing outbound control relationship; Bridge must not open an inbound port on developer machines.
