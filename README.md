# Bridgectl

[![CI](https://github.com/orchael/bridgectl/actions/workflows/ci.yml/badge.svg)](https://github.com/orchael/bridgectl/actions/workflows/ci.yml)
[![Publish](https://github.com/orchael/bridgectl/actions/workflows/publish.yml/badge.svg)](https://github.com/orchael/bridgectl/actions/workflows/publish.yml)
[![License](https://img.shields.io/github/license/orchael/bridgectl)](LICENSE)
[![GitHub Release](https://img.shields.io/github/v/release/orchael/bridgectl)](https://github.com/orchael/bridgectl/releases)

A standalone gRPC daemon and SDK that manages AI agent subprocess lifecycles and exposes a PTY transport so any client can attach to, interact with, and replay the terminal output of a running AI agent — regardless of when it connected.

Supported providers: **Claude**, **Codex**, **OpenCode**, **Gemini**

---

## How It Works

```
Your App (Go)
    ↕ Go SDK  or  raw gRPC
bridgectl daemon
    ↕ PTY
AI Agent process (claude / codex / opencode / gemini)
```

The bridge daemon spawns AI agents inside PTYs, buffers their output in a bounded ring buffer with sequence numbers, and serves a gRPC API. Clients can attach at any time, replay buffered output, and stream live PTY bytes. Authentication uses mTLS + JWT (Ed25519).

See the Docusaurus documentation under [docs/](docs/) for architecture details.

---

## Quick Start (run the server)

### Prerequisites

- [Go 1.26+](https://go.dev/dl/)
- **Node.js 24.x** (for provider CLIs -- use the version in `.nvmrc`). Install via any of these methods:
  - [nvm](https://github.com/nvm-sh/nvm) (recommended): `nvm install` after cloning
  - **macOS Homebrew**: `brew install node@24`
  - **Windows winget**: `winget install OpenJS.NodeJS --version 24`
  - **Windows Chocolatey**: `choco install nodejs --version=24`
  - **Ubuntu/Debian**: use [NodeSource](https://github.com/nodesource/distributions): `curl -fsSL https://deb.nodesource.com/setup_24.x | sudo -E bash - && sudo apt-get install -y nodejs`
  - **Direct download**: [nodejs.org/en/download](https://nodejs.org/en/download/)
- (optional) `protoc` + `protoc-gen-go` + `protoc-gen-go-grpc` -- only if modifying `.proto` files

Run `scripts/setup-node.sh` to verify your Node.js installation meets the project requirements, or `scripts/setup-node.sh --install` to attempt automatic installation via Homebrew on macOS.

### 1. Clone and configure

```bash
git clone https://github.com/orchael/bridgectl.git
cd bridgectl

# Set up Node.js (pick one):
nvm install           # if using nvm (recommended)
# Or verify your system Node: scripts/setup-node.sh
```

Set up `env-secrets` once for this repo. `env-secrets` in this environment uses AWS Secrets Manager:

```bash
cp .env.example .env
$EDITOR .env
cat > /tmp/bridgectl.secrets.env <<'EOF'
CLAUDE_CODE_OAUTH_TOKEN=
OPENAI_API_KEY=
EOF
$EDITOR /tmp/bridgectl.secrets.env
env-secrets aws secret upsert \
  --name bridgectl/dev \
  --file /tmp/bridgectl.secrets.env \
  --profile <aws-profile> \
  --region <aws-region>
```

The repo-local [`.env.example`](.env.example) contains only the non-secret `ENV_SECRETS_*` settings. After you copy it to `.env`, the main `make` targets will call `env-secrets aws` automatically and load the secret named by `ENV_SECRETS_AWS_SECRET`.

### 2. Build and install

```bash
nvm install
corepack enable
corepack prepare pnpm@11.22.0 --activate
pnpm install --frozen-lockfile
make build-cli
```

### 3. Start the daemon

```bash
bin/bridgectl server start
```

This starts the bridge in local Unix socket mode. Use `bin/bridgectl server start --listen <tailscale-ip>:9445` when you want secure remote access over Tailscale.

### 4. Try an interactive session

```bash
make chat-claude     # or chat-opencode, chat-codex, chat-gemini
```

### Docker

```bash
make up
```

Mounts `~/repos` → `/repos` and `./certs` → `/app/certs`. The prebuilt image is available at `ghcr.io/orchael/bridgectl`.

### Smoke Test

```bash
make smoke
```

This validates the repo Dockerfile and Compose stack by starting the bridge in Docker and running an authenticated gRPC health check.
It also verifies config-driven provider fallback by requesting a deliberately unavailable smoke provider and asserting the configured fallback provider is selected.

### Ubuntu Package Install

Supported releases: Ubuntu **24.04 (noble)** and **25.04 (plucky)** on `amd64`.

**Quick install:**

```bash
curl -fsSL https://orchael.github.io/bridgectl/install.sh | sudo bash
sudo systemctl enable --now bridgectl
```

**Manual apt setup (noble example):**

```bash
sudo install -d -m 0755 /etc/apt/keyrings
curl -fsSL https://orchael.github.io/bridgectl/apt/bridgectl-archive-keyring.asc \
  | sudo gpg --dearmor -o /etc/apt/keyrings/bridgectl.gpg
echo "deb [arch=amd64 signed-by=/etc/apt/keyrings/bridgectl.gpg] \
  https://orchael.github.io/bridgectl/apt noble main" \
  | sudo tee /etc/apt/sources.list.d/bridgectl.list >/dev/null
sudo apt-get update
sudo apt-get install -y bridgectl
sudo systemctl enable --now bridgectl
```

**Supported Ubuntu suites:** `noble` (24.04 LTS) and `plucky` (25.04). Replace `noble` above with `plucky` if you are on Ubuntu 25.04. The repository does not publish a `stable` or `jammy` suite — using any other suite name will result in a "does not have a Release file" error from apt.

**Ansible:** use `ansible_distribution_release` for the suite and `dpkg --print-architecture` for the arch. Do not use `ansible_architecture` — it returns the kernel arch (`x86_64`) rather than the Debian package arch (`amd64`), which causes apt to look for a non-existent `binary-x86_64/Packages` path.

```yaml
- name: Get Debian architecture
  ansible.builtin.command: dpkg --print-architecture
  register: dpkg_arch
  changed_when: false

- name: Ensure /etc/apt/keyrings exists
  ansible.builtin.file:
    path: /etc/apt/keyrings
    state: directory
    mode: '0755'

- name: Download bridgectl signing key
  ansible.builtin.get_url:
    url: https://orchael.github.io/bridgectl/apt/bridgectl-archive-keyring.asc
    dest: /tmp/bridgectl-keyring.asc
    mode: '0644'

- name: Dearmor signing key
  ansible.builtin.command: >
    gpg --dearmor -o /etc/apt/keyrings/bridgectl.gpg /tmp/bridgectl-keyring.asc
  args:
    creates: /etc/apt/keyrings/bridgectl.gpg

- name: Add bridgectl apt repository
  ansible.builtin.apt_repository:
    repo: "deb [arch={{ dpkg_arch.stdout }} signed-by=/etc/apt/keyrings/bridgectl.gpg] https://orchael.github.io/bridgectl/apt {{ ansible_distribution_release }} main"
    state: present
    filename: bridgectl
  notify: Update apt cache
```

The packaged service installs a minimal config at `/etc/bridgectl/bridge.yaml` and listens on `127.0.0.1:9445` by default. It does not bundle provider CLIs or API keys. For production use you must install the provider CLIs separately, add provider configuration, and decide how the service account should access the target repositories.

---

## Installing the Go SDK

Add the SDK to your Go module:

```bash
go get github.com/orchael/bridgectl/pkg/bridgeclient
```

### Minimal example

```go
import (
    "context"
    "github.com/orchael/bridgectl/pkg/bridgeclient"
    bridgev1 "github.com/orchael/bridgectl/gen/bridge/v1"
)

// Connect (no TLS — for local dev with auth disabled)
client, err := bridgeclient.New(
    bridgeclient.WithTarget("127.0.0.1:9445"),
)
if err != nil { ... }
defer client.Close()

ctx := context.Background()

// Start an agent session
_, err = client.StartSession(ctx, &bridgev1.StartSessionRequest{
    ProjectId: "my-project",
    SessionId: "session-001",
    RepoPath:  "/path/to/repo",
    Provider:  "claude",
})

// Attach and stream output
stream, err := client.AttachSession(ctx, &bridgev1.AttachSessionRequest{
    SessionId: "session-001",
})
stream.RecvAll(ctx, func(ev *bridgev1.AttachSessionEvent) error {
    fmt.Print(string(ev.Payload))
    return nil
})

// Send input
client.WriteInput(ctx, &bridgev1.WriteInputRequest{
    SessionId: "session-001",
    Data:      []byte("hello\n"),
})
```

### With mTLS + JWT (production)

```go
client, err := bridgeclient.New(
    bridgeclient.WithTarget("bridge.internal:9445"),
    bridgeclient.WithMTLS(bridgeclient.MTLSConfig{
        CABundlePath: "certs/ca-bundle.crt",
        CertPath:     "certs/client.crt",
        KeyPath:      "certs/client.key",
        ServerName:   "bridge.local",
    }),
    bridgeclient.WithJWT(bridgeclient.JWTConfig{
        PrivateKeyPath: "certs/jwt-signing.key",
        Issuer:         "my-service",
        Audience:       "bridge",
    }),
)
```

Full Go SDK reference: [docs/docs/reference/go-sdk.md](docs/docs/reference/go-sdk.md)

---

## Secure by default

bridgectl supports mTLS for workload identity and JWTs for authorization. Bring your existing PKI or use the Step CA integration for automated enrollment, short-lived certificates, and renewal.

| Mode | Transport | Identity | Use case |
|------|-----------|----------|----------|
| `local` | Plaintext (loopback) | None | Development |
| `tls` | Server TLS | JWT | Trusted networks |
| `mtls` | Mutual TLS | Client cert + JWT | Production |

Supported certificate sources: **Step CA**, **Kubernetes cert-manager**, **SPIFFE/SPIRE**, **Vault PKI**, **AWS Private CA**, **enterprise CAs**, and **manually managed X.509 certificates**.

Production deployments should use mTLS unless the environment provides an equivalent trusted transport boundary.

> **Secure by default. Bring your own identity.**

See [Security Overview](docs/docs/security/overview.md) for details.

---

## Using grpcurl

Install [grpcurl](https://github.com/fullstorydev/grpcurl) to call the bridge from a shell.

**With mTLS** (after starting a secure TCP server):

```bash
grpcurl \
  -cacert certs/ca-bundle.crt \
  -cert certs/dev-client.crt \
  -key certs/dev-client.key \
  -servername bridge.local \
  -import-path proto -proto bridge/v1/bridge.proto \
  127.0.0.1:9445 bridge.v1.BridgeService/Health
```

**Without TLS** (auth disabled):

```bash
# Health check
grpcurl -plaintext -import-path proto -proto bridge/v1/bridge.proto \
  127.0.0.1:9445 bridge.v1.BridgeService/Health

# Start a session
grpcurl -plaintext -import-path proto -proto bridge/v1/bridge.proto \
  -d '{"project_id":"dev","session_id":"s1","repo_path":"/tmp","provider":"claude"}' \
  127.0.0.1:9445 bridge.v1.BridgeService/StartSession

# Send input
grpcurl -plaintext -import-path proto -proto bridge/v1/bridge.proto \
  -d '{"session_id":"s1","client_id":"c1","data":"aGVsbG8K"}' \
  127.0.0.1:9445 bridge.v1.BridgeService/WriteInput

# Stream output
grpcurl -plaintext -import-path proto -proto bridge/v1/bridge.proto \
  -d '{"session_id":"s1","client_id":"c1"}' \
  127.0.0.1:9445 bridge.v1.BridgeService/AttachSession

# Stop a session
grpcurl -plaintext -import-path proto -proto bridge/v1/bridge.proto \
  -d '{"session_id":"s1"}' \
  127.0.0.1:9445 bridge.v1.BridgeService/StopSession
```

Note: `data` is base64-encoded bytes. `grpcurl` does not support JWT injection — use the Go SDK for JWT-authenticated calls.

Full API reference: [docs/docs/reference/grpc-api.md](docs/docs/reference/grpc-api.md)

Full documentation source: [docs/docs/intro.md](docs/docs/intro.md)

---

## Providers

| Provider | Binary | Required auth |
|----------|--------|---------------|
| `claude` | `./node_modules/.bin/claude` | `CLAUDE_CODE_OAUTH_TOKEN` |
| `opencode` | `./node_modules/.bin/opencode` | `OPENAI_API_KEY` |
| `codex` | `./node_modules/.bin/codex` | Existing account, `CODEX_AUTH`, or API key ([precedence](docs/docs/reference/configuration.md#codex-authentication)) |
| `gemini` | `agy` | OAuth2 credentials (`~/.gemini/oauth_creds.json`) |

Providers are configured in `config/bridge-dev.yaml`. See [docs/docs/reference/configuration.md](docs/docs/reference/configuration.md) for configuration reference.

---

## Makefile Targets

| Target | Description |
|--------|-------------|
| `make build` | Build `bin/bridgectl` and `bin/bridge-ca` |
| `make test` | Run unit tests with race detection |
| `make test-e2e` | Run the Dockerized end-to-end test suite |
| `make test-e2e-unprotected` | Manually run live Docker SDK e2e tests for Codex/Claude protected and unprotected modes |
| `make test-cover` | Run tests with coverage report |
| `make smoke` | Run Docker-based smoke validation |
| `make smoke-apt-local` | Build a `.deb`, create a local apt repo, and verify install in Ubuntu containers |
| `make smoke-ec2` | Provision an EC2 host, install from the hosted apt repo, and run a health check |
| `make chat-claude` | Interactive PTY session with Claude |
| `make chat-opencode` | Interactive PTY session with OpenCode |
| `make chat-codex` | Interactive PTY session with Codex |
| `make chat-gemini` | Interactive PTY session with Gemini |
| `make proto` | Regenerate protobuf Go code (after editing `.proto`) |
| `make lint` | Run golangci-lint |
| `make fmt` | Format code with gofmt + goimports |
| `make dev-setup` | Build binaries and generate dev certificates |
| `make docs-build` | Build the Docusaurus documentation site |
| `make setup-node` | Check Node.js installation and print setup guidance |
| `make certs` | Initialize a bridge CA in `certs/` |
| `make clean` | Remove build artifacts |

---

## bridge-ca: Certificate and Key Management (Deprecated)

> **Deprecated:** `bridge-ca` is deprecated. For new deployments, use `bridgectl server start` which auto-generates PKI, or use the [Step CA integration](docs/docs/security/step-ca.md) for automated certificate lifecycle management. The `cross-sign` and `verify` subcommands remain available during the deprecation period. `bridge-ca` will be removed in the next major release. See [#154](https://github.com/orchael/bridgectl/issues/154) for details.

```bash
bridge-ca init          # Initialize a new ECDSA P-384 CA
bridge-ca issue         # Issue a server or client certificate
bridge-ca cross-sign    # Cross-sign an external CA for multi-tenant trust
bridge-ca bundle        # Build a trust bundle from multiple CA certs
bridge-ca jwt-keygen    # Generate an Ed25519 keypair for JWT signing
bridge-ca verify        # Verify a certificate against a trust bundle
```

Run `bridge-ca <command> --help` for flags.

---

## Project Structure

```
cmd/bridgectl/                 User-session server and CLI
cmd/bridge-ca/                 Certificate management CLI
pkg/bridgeclient/              Go SDK
proto/bridge/v1/               Protobuf service definitions
gen/bridge/v1/                 Generated protobuf Go code (do not edit)
internal/auth/                 mTLS, JWT, gRPC interceptors
internal/bridge/               Session supervisor, event buffer, registry, policy
internal/config/               YAML configuration loader
internal/pki/                  CA management, cert issuance, cross-signing
internal/provider/             Stdio/PTY provider adapters
internal/server/               gRPC server implementation + rate limiting
examples/chat/                 Interactive PTY passthrough example
docs/                          Docusaurus documentation site
config/                        Default configuration files
scripts/                       Development helper scripts
```

---

## Documentation

- [docs/docs/intro.md](docs/docs/intro.md) — Documentation entry point
- [docs/docs/getting-started/local-server.md](docs/docs/getting-started/local-server.md) — Local server quick start
- [docs/docs/guides/web-ui.md](docs/docs/guides/web-ui.md) — Web UI example
- [docs/docs/guides/step-ca-tailscale.md](docs/docs/guides/step-ca-tailscale.md) — Step CA over Tailscale setup
- [docs/docs/reference/go-sdk.md](docs/docs/reference/go-sdk.md) — Go SDK reference
- [docs/docs/reference/grpc-api.md](docs/docs/reference/grpc-api.md) — Full gRPC API reference

---

## Publishing a Release

Releases are triggered via **Actions → Publish → Run workflow** with a `patch`, `minor`, or `major` input. The workflow bumps the version, creates a Git tag, builds the container image, builds and signs `.deb` packages, and publishes the apt repository to GitHub Pages.

### Required GitHub repository secrets

| Secret | Description |
|--------|-------------|
| `APT_REPO_GPG_PRIVATE_KEY_B64` | Base64-encoded GPG private key used to sign the apt repository |
| `APT_REPO_GPG_PASSPHRASE` | Passphrase for the GPG key (omit if the key has no passphrase) |
| `AWS_SMOKE_ROLE_ARN` | (optional) IAM role ARN for the EC2 smoke test; skipped if not set |
| `SMOKE_AWS_REGION` | (optional) AWS region for the EC2 smoke test |

#### Generating the apt signing key

```bash
# Generate a dedicated signing key (no passphrase)
gpg --batch --gen-key <<'EOF'
Key-Type: RSA
Key-Length: 4096
Name-Real: AI Agent Bridge APT Signing
Name-Email: apt-signing@orchael.com
Expire-Date: 0
%no-protection
%commit
EOF

# Export and base64-encode the private key
gpg --export-secret-keys --armor apt-signing@orchael.com | base64 -w 0
```

Add the base64 output as `APT_REPO_GPG_PRIVATE_KEY_B64` in **Settings → Secrets and variables → Actions**. The public key is exported automatically into the apt repository at publish time as `bridgectl-archive-keyring.asc`.

---

## License

MIT License - see [LICENSE](LICENSE) file for details.
