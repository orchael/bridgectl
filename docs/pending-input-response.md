# Request-bound remote response (MAR-66)

Bridge may send a `respond` command only for an existing pending input request.
This is not a remote terminal or a generic task-submission API. Plain PTY
providers and approvals are unsupported. Codex app-server advertises remote
response only for a captured, current, single, non-secret input question;
structured approval is false. Multiple-question forms are left for a later
typed response interface instead of guessing where one free-form answer belongs.

## Provider evidence

Real Codex 0.153.4 testing exposed an MAR-85 gap: a second WebSocket observer
receives thread status broadcasts but not the TUI owner's pending request.
The provider now puts a loopback WebSocket relay between the unmodified TUI and
its companion app-server. It forwards the normal protocol and observes the
owning connection's request IDs and question IDs. Only one TUI connection may
use that relay. Responses are JSON-RPC results for the exact original request,
using the provider's structured answer map; no terminal bytes or new turns are
generated. The TUI continues receiving the provider's resolution notification.

The question field in the real protocol is `question`, not `text`. Requests
from unrelated threads are rejected. If status arrives before request metadata,
the later request adds its identity without inventing a new waiting state.
Quiet waiting no longer ends merely because no notification arrives in 60s.

`WriteInput` still does not resolve interaction state. Remote response likewise
leaves the pending request intact until a later provider transition; the provider
tracks submission separately to reject duplicate answers. A stale RPC answer can
never become a new terminal command or text for another turn.

## Writer ownership

`Supervisor.RespondToInput` validates text, active runtime and exact pending
request, then requires the existing writer slot to be empty. It holds the same
session lock used by attach/claim/stop for the bounded provider write (maximum
five seconds). Existing writers receive no eviction, demotion or interruption.
Callers must release their local writer before remote response is available.
Remote failures neither stop the session nor disable direct local input.

## Command identity and replay

The outbound control client's authenticated hello supplies its installation and
organization scope. A command must match both and carry a user, session,
pending request, action, UUID command ID and deadline. The only allowed action
is `respond`; text is at most 16 KiB and excludes terminal control characters.

Before invoking Supervisor, the client durably creates and fsyncs a mode-0600
claim in `bridge-control-commands`. It stores only a credential-keyed HMAC and
the result, never response text. Same-ID/same-payload retries return that result;
changed payloads conflict. A pre-write crash or ambiguous write retains an
`unknown` claim and never automatically resubmits. A second command ID for the
same already-submitted provider request is rejected independently by the
provider. Recovered sessions are not controllable.

The ledger is capped at 10,000 entries and fails closed if full or unavailable.
Retain it until retiring the installation state; do not delete claims to retry
an uncertain command. Credential rotation makes old fingerprints conflict rather
than risking replay. Local operation remains available even when remote delivery
or ledger persistence fails.

## Verification

Automated tests cover local writer conflict, runtime/request validation,
cross-installation/organization commands, changed payloads, malformed/oversized
text, restart replay, uncertain outcomes and the transport-acceptance invariant.

`MAR66_LIVE=1 go test ./internal/provider -run '^TestMAR66RealTUIWaiting$' -v`
runs the real TUI, reaches waiting input, submits a structured answer, rejects
a duplicate and requires authoritative working then idle.

For the full development-browser cycle, explicitly supply the enrolled
development credential file:

```
MAR66_BRIDGE_LIVE=1 MAR66_CREDENTIAL_FILE=/path/to/bridge-credentials.json \
  go test ./internal/provider -run '^TestMAR66BridgeLive$' -v -timeout 15m
```

That test creates a real Supervisor session, releases its bootstrap writer,
publishes state through `wss://control.bridge.orchael.dev/v1/control`, and waits
for the Bridge browser to respond. It never answers on the browser's behalf.
Use Bridge's opt-in `respond.spec.ts` acceptance test on
`https://bridge.orchael.dev`. Production endpoint defaults are unchanged.
