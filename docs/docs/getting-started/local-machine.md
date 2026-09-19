---
title: Prepare a Local Machine
---

Use this setup on every machine that will run a bridge server or use the example clients.

## Prerequisites

- Go 1.26 or newer
- Node.js 24.x and Corepack
- pnpm 11.22.0 through Corepack
- Docker Compose v2 if you use the compose examples
- At least one provider CLI credential:
  - `CLAUDE_CODE_OAUTH_TOKEN` for Claude
  - `OPENAI_API_KEY` or Codex auth files for Codex
  - `OPENAI_API_KEY` for OpenCode
  - OAuth2 credentials for Gemini (`agy`) at `~/.gemini/oauth_creds.json`

## Install on Linux (apt)

On Ubuntu, you can install `bridgectl` from the apt repository instead of building from source:

```bash
sudo install -d -m 0755 /etc/apt/keyrings
curl -fsSL https://orchael.github.io/bridgectl/apt/bridgectl-archive-keyring.asc \
  | sudo gpg --dearmor -o /etc/apt/keyrings/bridgectl.gpg
echo "deb [arch=$(dpkg --print-architecture) signed-by=/etc/apt/keyrings/bridgectl.gpg] \
  https://orchael.github.io/bridgectl/apt noble main" \
  | sudo tee /etc/apt/sources.list.d/bridgectl.list >/dev/null
sudo apt-get update
sudo apt-get install -y bridgectl
```

**Supported suites:** `noble` (24.04 LTS) and `plucky` (25.04). Replace `noble` with `plucky` if you are on Ubuntu 25.04.

If you install via apt, you still need to clone the repository to install the provider CLIs (see **Install Provider CLIs** below), but you can skip the **Clone and Build** step.

## Clone and Build

```bash
git clone https://github.com/orchael/bridgectl.git
cd bridgectl

nvm install
nvm use
corepack enable
corepack prepare pnpm@11.22.0 --activate

make deps
make setup
# Run the PATH command printed by make setup in this terminal.

make build-cli
```

The build writes `bin/bridgectl`. Use `make build` instead to regenerate the gRPC stubs and also build `bin/bridge-ca`.

`make deps` checks your Node.js version, installs the protobuf Go generators,
`goimports`, and `golangci-lint`, and downloads Go modules. It installs missing
`protoc` through Homebrew (without confirmation prompts) or apt (using sudo when
needed). On other systems, install `protoc` with your package manager first.
Set `PROTOC_INCLUDE` if your protobuf headers are outside the Homebrew prefix or
`/usr/include`.

`make setup` persists the Go tools directory from `GOBIN` or `GOPATH` in Bash's
`.bashrc` and active login profile, or Zsh's `.zshrc`. It prints a command to update
the current terminal because Make cannot change its parent shell's environment.
It also installs `pre-commit` (via Homebrew or apt if missing) and registers the
`pre-commit`/`pre-push` git hooks from `.pre-commit-config.yaml`, so run `make setup`
at the start of every session — including agent sessions — not just once per machine.
`make tools` and `make setup-node` remain available separately; `make dev-setup`
is a separate workflow for development certificates, provider CLIs, and host setup.

## Install Provider CLIs

The repository pins the provider CLIs in the root `package.json`.

```bash
pnpm install --frozen-lockfile
```

Export the provider credentials that match the provider you plan to run:

```bash
export OPENAI_API_KEY="..."
export CLAUDE_CODE_OAUTH_TOKEN="..."
```

Only the variables required by the selected provider need to be set.

For Gemini, install the Antigravity CLI (`agy`) instead of setting an API key:

```bash
curl -fsSL https://antigravity.google/cli/install.sh | bash
```

Authenticate with `agy auth login`. Credentials are stored at `~/.gemini/oauth_creds.json`.

## Repository Paths

By default, all repository paths are allowed. To restrict which directories sessions can access, set `allowed_paths` in the bridge config.

For local testing, clone the demo repository:

```bash
mkdir -p ~/repos
git clone https://github.com/orchael/bridge-demo.git ~/repos/bridge-demo
```
