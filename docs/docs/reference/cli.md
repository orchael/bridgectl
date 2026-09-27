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
```

Remote session commands accept:

| Flag | Description |
| --- | --- |
| `--remote <host[:port]>` | Remote bridge host. Port defaults to `9445`. |
| `--cert <path>` | Client mTLS certificate override. |
| `--key <path>` | Client private key override. |
| `--jwt-key <path>` | JWT signing private key override. |
| `--server-name <name>` | TLS server name override when dialing by IP or alternate DNS name. |

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
