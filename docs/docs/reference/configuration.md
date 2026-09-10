---
title: Configuration Reference
---

`bridgectl server start` can load YAML from:

1. `--config <path>`
2. `~/.config/bridgectl/bridge.yaml`
3. `$XDG_CONFIG_HOME/bridgectl/config.yaml`
4. The platform user config directory

Flags override values from the config file.

## Minimal Local Config

```yaml
server:
  listen: "127.0.0.1:9445"

allowed_paths:
  - "/home"
  - "/tmp"

providers:
  codex:
    binary: "node"
    args: ["./node_modules/@openai/codex/bin/codex.js"]
    startup_timeout: "60s"
```

## Remote Step CA Config

```yaml
server:
  listen: "100.x.y.z:9445"
  san:
    - "machine.tailnet-name.ts.net"

step_ca:
  url: "https://ca-host.tailnet-name.ts.net:9443"
  root: "/home/me/.config/bridgectl/certs/step-ca-root.crt"
  provisioner: "bridge-jwk"
  provisioner_password_file: "/home/me/.config/bridgectl/step-ca-password"

allowed_paths:
  - "/home/me/repos"
```

## Important Fields

| Field | Purpose |
| --- | --- |
| `server.listen` | TCP bind address. If omitted and no `--listen` is set, local Unix socket mode is used. |
| `server.san` | Server certificate SANs used by remote clients for TLS verification. |
| `tls.ca_bundle`, `tls.cert`, `tls.key` | Explicit mTLS material for non-Step-CA deployments. |
| `auth.jwt_public_keys` | Static JWT issuer public keys. |
| `step_ca.clients` | Startup-loaded JWT public keys for known Step CA clients. |
| `sessions.max_per_project` | Per-project session limit. |
| `sessions.max_global` | Global session limit. |
| `sessions.event_buffer_size` | Per-session replay buffer size. |
| `input.max_size_bytes` | Maximum input payload accepted per write. |
| `providers.<name>.binary` | Provider executable. |
| `providers.<name>.args` | Arguments prepended when starting the provider. |
| `providers.<name>.required_env` | Environment variables required before the provider is considered healthy. |
| `allowed_paths` | Parent paths under which sessions may run. |

## Provider Fallbacks

When `feature_flags.provider_fallbacks` is true, a provider can declare fallback providers:

```yaml
providers:
  claude:
    binary: "./node_modules/@anthropic-ai/claude-code/bin/claude.exe"
    required_env: ["CLAUDE_CODE_OAUTH_TOKEN"]
    fallbacks: ["codex"]
```

If `claude` is unavailable, the server can select `codex` for the session.

## Codex Authentication

Codex selects credentials from the environment of the daemon or prepared
session. An already-running daemon does not inherit later client shell exports.

1. Existing ChatGPT account credentials in `CODEX_HOME/auth.json`. When
   `CODEX_HOME` is absent, check `~/.codex/auth.json`, then
   `~/.config/bridgectl/codex-home/auth.json`. An explicit home is isolated.
2. A valid `CODEX_AUTH` JSON bootstrap, written to the explicit home or
   `~/.config/bridgectl/codex-home` with directory mode 0700 and file mode 0600.
3. `CODEX_API_KEY`, then `OPENAI_API_KEY`, then an existing API-key auth file.
   Environment API keys are written in native `auth.json` format.

The chosen file is the child process's credential source. Bootstrap/API-key
environment values are removed from its effective environment so they cannot
override account auth. A bootstrap is not rewritten over an existing account,
including after daemon restart. Codex can refresh the desktop's private copy.
Startup probes and session health use the same source selection.

Health is read-only: it verifies a credential source and rejects unreadable or
non-directory paths, but does not attempt bootstrap writes. A missing home under
an unwritable parent can therefore pass health and fail command preparation.
That failure is returned before Codex starts. Preparation also restores mode
0700 on an existing owned bootstrap directory, so health does not reject such a
directory solely because its current permission bits omit write access.

Validation checks JSON structure and nonempty access/refresh tokens, not whether
the remote account still accepts them. Malformed or absent credentials fall
through to the next source. If the server rejects a selected account, refresh
the desktop login or rotate its secret; bridgectl does not replay the session
with a billable API key. See [official Codex authentication documentation](https://learn.chatgpt.com/docs/auth).

To rotate the bootstrap, stop the daemon and provider sessions, replace the
daemon's secret environment, remove only the selected home's `auth.json`, then
restart. For the default search path, clear both native and managed auth files
when replacing all sources. No marker/fingerprint files need clearing; retain
other Codex state. On AI Desktops, `ai-desktops secrets reload` performs the
rotation and interrupts active sessions. `CODEX_AUTH` may still hold the old
snapshot after automatic token refresh; the local `auth.json` is authoritative.

## Persistence

Use `--db-path` to persist session metadata and PTY chunks:

```bash
bin/bridgectl server start --db-path ~/.config/bridgectl/sessions.db
```

On restart, terminal session history can be replayed as terminal history.
