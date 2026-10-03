---
title: CLI Reference
---

The main CLI is `bridgectl`. Build it with `make build-cli`.

## Run

The quickest way to start working. Auto-starts a local server if needed, creates a session, and attaches your terminal:

```bash
bridgectl run [directory]
bridgectl run --provider codex ~/repos/my-project
bridgectl run --timeout 1h .
```

| Flag | Short | Default | Description |
| --- | --- | --- | --- |
| `--provider` | `-p` | `claude` | AI provider: `claude`, `codex`, `opencode`, `gemini`, `echo`. |
| `--timeout` | `-t` | `30m` | Session timeout. |
| `--no-tty` | | `false` | Run without a terminal (for scripting and tests). |

The directory argument defaults to `.` if omitted. Press **Ctrl-]** to detach without stopping the session.

## Server Commands

```bash
bridgectl server init
bridgectl server start
bridgectl server status
bridgectl server stop
bridgectl server issue-client --name <client-name>
bridgectl server renew-cert
```

Common server flags:

| Flag | Description |
| --- | --- |
| `--listen <addr>` | Enables secure TCP mode, for example `0.0.0.0:9445` or a Tailscale IP. |
| `--san <name>` | Adds DNS names or IP addresses to the server certificate. Repeat or pass comma-separated values. |
| `--config <path>` | Loads a YAML config. If omitted, the server checks user config locations. |
| `--db-path <path>` | Enables persisted session metadata and PTY history. |
| `--step-ca-url <url>` | Uses Step CA for server certificate issuance. |
| `--step-ca-root <path>` | Root certificate for the Step CA. |
| `--step-ca-provisioner <name>` | Step CA provisioner, such as `bridge-jwk` or `acme`. |

## Client Credential Commands

```bash
bridgectl client init \
  --step-ca-url https://ca-host:9443 \
  --provisioner bridge-jwk \
  --target bridge-host:9445

bridgectl client enroll \
  --target bridge-host:9445 \
  --ca ~/.config/bridgectl/certs/step-ca-root.crt \
  --cert ~/.config/bridgectl/certs/<name>.crt \
  --key ~/.config/bridgectl/certs/<name>.key

bridgectl client renew
```

`client init` obtains an mTLS certificate. When `--target` is set, it also enrolls a JWT public key with the target bridge server.

## Session Commands

`session list` displays runtime `STATUS` and provider `INTERACTION` separately.
Claude and Codex enable structured reporting by default. See
[Session Status](../guides/session-status.md) for the states and capabilities.
The automatically configured `session report-claude-hook --config <private-file>`
helper accepts Claude hook JSON on stdin and reports only metadata to the local
session observer; it does not grant permissions or answer questions.

```bash
bridgectl session list
bridgectl session watch <session-id>
bridgectl session attach <session-id>
bridgectl session attach --take-over <session-id>
bridgectl session attach --release <session-id>
bridgectl session stop <session-id>
bridgectl session diagnose <session-id>
bridgectl session diagnose <session-id> --json
```

Remote session commands accept:

| Flag | Description |
| --- | --- |
| `--remote <host[:port]>` | Remote bridge host. Port defaults to `9445`. |
| `--cert <path>` | Client mTLS certificate override. |
| `--key <path>` | Client private key override. |
| `--jwt-key <path>` | JWT signing private key override. |
| `--server-name <name>` | TLS server name override when dialing by IP or alternate DNS name. |

### session diagnose

`session diagnose` reports what the local bridgectl server currently believes
about one session, for developers and AI agents debugging a mismatch with
Bridge or another replica. The daemon builds the report (`DiagnoseSession` RPC)
from its Supervisor state plus the control client's local status and revision
files. It does not read telemetry, Bridge, terminal output or logs, and it never
infers interaction state. Against a daemon that predates the RPC (for example
one not yet restarted after an upgrade), the command builds the same report
locally from `GetSession`.

- It is a **snapshot of current authoritative state, not an event replay.**
- It works the same when Bridge is unavailable or not enrolled, opens no port
  and sends nothing to Bridge. With `--remote <host>` the report describes that
  machine's daemon (its control status and revisions); `bridgectl_version` is the
  daemon's version.
- **Privacy boundary:** the output contains no PTY output, transcript, prompts,
  responses, chain-of-thought, environment variables, credentials, OAuth
  material, filesystem contents or paths, session `error` text, control
  `last_error`, or client IDs (writer presence is a boolean). The only
  provider-supplied free text is the pending-request `id` (an opaque
  identifier, length-bounded, control characters removed). Provider pending
  *summaries* are deliberately **not** included, because they can contain
  working directories, full commands and credentials; only a boolean
  `summary_available` is reported.
- Unknown stays unknown: a value bridgectl cannot report is `null` (or
  `"unknown"` for enums), never a guess.
- Failure (malformed or unknown session ID, server not running) exits non-zero.
  With `--json` stdout carries `{"schema_version":1,"error":{"code":...,"message":...}}`;
  `code` is one of `invalid_session_id`, `session_not_found`,
  `server_unavailable`, `internal`.

Example `--json` output (pretty-printed here; the real output is one line):

```json
{
  "schema_version": 1,
  "bridgectl_version": "v1.2.3",
  "session_id": "11111111-1111-4111-8111-111111111111",
  "provider": "claude",
  "project_id": "proj",
  "status": "running",
  "exit_code": null,
  "created_at": "2026-03-01T10:00:00Z",
  "stopped_at": null,
  "interaction_state": "waiting_for_approval",
  "interaction_capability": {
    "interaction_state_supported": true,
    "approval_state_supported": true,
    "pending_summary_supported": true,
    "remote_response_supported": true,
    "structured_approval_supported": true
  },
  "interaction_updated_at": "2026-03-01T10:01:00Z",
  "interaction_last_report_at": "2026-03-01T10:02:00Z",
  "pending_request": {"id": "req-1", "id_sha256": "sha256:3f2a…", "type": "approval", "kind": "command", "summary_available": true},
  "lifecycle_revision_wire": 4,
  "interaction_revision_wire": 3,
  "interaction_revision_local": 7,
  "active_writer": true,
  "observer_count": 2,
  "control": {"state": "connected", "updated_at": "2026-03-01T10:59:50Z", "last_connected_at": "2026-03-01T10:59:50Z", "stale": false}
}
```

| Field | Meaning |
| --- | --- |
| `schema_version` | Always first. Fields of a published version never change; incompatible changes bump it. |
| `bridgectl_version` | Version of the bridgectl daemon that built the report. |
| `session_id`, `provider`, `project_id` | Session identity. |
| `status` | Supervisor `SessionState`: `starting`, `running`, `attached`, `stopping`, `stopped`, `failed` (`unknown` if unreported). |
| `exit_code` | Process exit code once recorded, else `null`. |
| `created_at`, `stopped_at` | RFC 3339 UTC, or `null`. |
| `interaction_state` | `working`, `waiting_for_input`, `waiting_for_approval`, `idle`, `unknown`. Independent of `status`. |
| `interaction_capability` | The five provider capability flags. `null` = capability unknown (server reported none); all `false` = explicitly unsupported, so `interaction_state` can only be `unknown`. |
| `interaction_updated_at` | Last change of interaction state or pending-request identity. Not a session-wide "last activity" time. |
| `interaction_last_report_at` | Last authoritative provider report, including repeats. |
| `pending_request` | `null`, or `{id, id_sha256, type (input\|approval), kind, summary_available}`. `id_sha256` is `sha256:` plus the digest of the original, untruncated request ID (`id` itself is bounded and sanitized). `kind` is a provider-assigned category from a fixed vocabulary: `command`, `file_change`, `tool`, `question`, `other`, or `unknown` (provider did not classify it; any other provider value is reported as `other`). `summary_available` is true when the provider supplied a summary; its text is never included. |
| `lifecycle_revision_wire` | Lifecycle revision last allocated by the Bridge control client for this session; `null` if it has none (not enrolled, or forgotten after a terminal state). |
| `interaction_revision_wire` | The interaction revision Bridge compares; `null` likewise. |
| `interaction_revision_local` | The Supervisor's own interaction revision. Not restart-durable and a different sequence from the wire revisions; never compare them to each other. |
| `active_writer` | Whether a writer is attached (boolean only). |
| `observer_count` | Read-only observers attached. |
| `control.state` | Local control status file: `not_provisioned`, `connecting`, `connected`, `disconnected`, `auth_rejected`, `unavailable`, or `unknown` (no file, unreadable, unrecognized). |
| `control.updated_at` | When that status last changed or heartbeated. |
| `control.last_connected_at` | Last time the control client was observed connected; kept across later failures and daemon restarts. `null` if never recorded (including status files from older bridgectl versions). |
| `control.stale` | `updated_at` is older than two minutes. A stale `connected` entry means no live client is behind it. |

Without `--json`, the same fields are printed as a short sectioned report
rendered from the identical model.

## bridge-ca (Deprecated)

:::warning Deprecated
`bridge-ca` is deprecated. Use `bridgectl server start` (auto-PKI) or [Step CA integration](../security/step-ca) for all new deployments. `bridge-ca` will be removed in the next major release. See [#154](https://github.com/orchael/bridgectl/issues/154).
:::

`bridge-ca` is a standalone certificate management CLI. It was the primary way to set up PKI before `bridgectl server start` gained auto-PKI support and Step CA integration was added.

For new deployments:
- **Single machine / development**: `bridgectl server start` auto-generates a self-signed CA and server/client certificates.
- **Multi-machine / production**: Use [Step CA integration](../security/step-ca) for automated enrollment, short-lived certificates, and renewal.
- **Existing enterprise CA**: Use the [filesystem provider](../security/existing-ca) with certificates from your PKI.

The `cross-sign` and `verify` subcommands remain available during the deprecation period as they have no equivalent in the current auto-PKI or Step CA workflows.

```bash
bridge-ca init          # Initialize a new ECDSA P-384 CA
bridge-ca issue         # Issue a server or client certificate
bridge-ca cross-sign    # Cross-sign an external CA for multi-tenant trust
bridge-ca bundle        # Build a trust bundle from multiple CA certs
bridge-ca jwt-keygen    # Generate an Ed25519 keypair for JWT signing
bridge-ca verify        # Verify a certificate against a trust bundle
```
