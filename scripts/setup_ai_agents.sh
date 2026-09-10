#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_DIR="$(dirname "$SCRIPT_DIR")"

cd "$PROJECT_DIR"

if [ ! -f package.json ]; then
  echo "package.json not found in $PROJECT_DIR"
  exit 1
fi

if [ ! -f .nvmrc ]; then
  echo ".nvmrc not found in $PROJECT_DIR"
  exit 1
fi

required_node_engine="$(sed -nE 's/.*"node"[[:space:]]*:[[:space:]]*"([^"]+)".*/\1/p' package.json | head -n1)"
required_node_major="$(printf '%s' "$required_node_engine" | grep -Eo '[0-9]+' | head -n1)"
nvmrc_major="$(tr -d '[:space:]' < .nvmrc)"

if [ -z "$required_node_major" ]; then
  echo "failed to parse package.json engines.node"
  exit 1
fi

if [ "$required_node_major" != "$nvmrc_major" ]; then
  echo "package.json engines.node ($required_node_engine) does not match .nvmrc ($nvmrc_major)"
  exit 1
fi

install_node_with_nodesource_linux() {
  echo "==> nvm not found; installing Node.js $required_node_major via NodeSource"

  if command -v apt-get >/dev/null 2>&1; then
    sudo apt-get update
    sudo apt-get install -y ca-certificates curl gnupg
    sudo mkdir -p /etc/apt/keyrings
    curl -fsSL https://deb.nodesource.com/gpgkey/nodesource-repo.gpg.key |
      sudo gpg --dearmor -o /etc/apt/keyrings/nodesource.gpg
    echo "deb [signed-by=/etc/apt/keyrings/nodesource.gpg] https://deb.nodesource.com/node_${required_node_major}.x nodistro main" |
      sudo tee /etc/apt/sources.list.d/nodesource.list >/dev/null
    sudo apt-get update
    sudo apt-get install -y nodejs
    return
  fi

  if command -v dnf >/dev/null 2>&1 || command -v yum >/dev/null 2>&1; then
    local repo_path="/etc/yum.repos.d/nodesource-nodejs.repo"
    local sys_arch
    sys_arch="$(uname -m)"
    sudo tee "$repo_path" >/dev/null <<EOF
[nodesource-nodejs]
name=Node.js Packages for Linux RPM based distros - ${sys_arch}
baseurl=https://rpm.nodesource.com/pub_${required_node_major}.x/nodistro/nodejs/${sys_arch}
priority=9
enabled=1
gpgcheck=1
gpgkey=https://rpm.nodesource.com/gpgkey/ns-operations-public.key
module_hotfixes=1
EOF
    if command -v dnf >/dev/null 2>&1; then
      sudo dnf install -y nodejs
    else
      sudo yum install -y nodejs
    fi
    return
  fi

  echo "unsupported Linux package manager; install Node.js $required_node_major manually"
  exit 1
}

activate_required_node() {
  export NVM_DIR="${NVM_DIR:-$HOME/.nvm}"
  if [ -s "$NVM_DIR/nvm.sh" ]; then
    # shellcheck disable=SC1090
    . "$NVM_DIR/nvm.sh"
  fi

  if command -v nvm >/dev/null 2>&1; then
    echo "==> Installing Node.js $required_node_major via nvm"
    nvm install "$required_node_major"
    nvm use "$required_node_major" >/dev/null
    if [ -n "${NVM_BIN:-}" ]; then
      export PATH="$NVM_BIN:$PATH"
    fi
    hash -r
    return
  fi

  if [ "$(uname -s)" = "Linux" ]; then
    install_node_with_nodesource_linux
    hash -r
    return
  fi

  echo "nvm is not available and automatic Node.js installation is only supported on Linux at this time"
  exit 1
}

activate_required_node

actual_node_major="$(node -p 'process.versions.node.split(".")[0]')"
if [ "$actual_node_major" != "$required_node_major" ]; then
  echo "active Node.js major version ($actual_node_major) does not match required version ($required_node_major)"
  exit 1
fi

echo "==> Installing pinned AI agent CLIs into $PROJECT_DIR/node_modules"
corepack pnpm install --frozen-lockfile

echo "==> Verifying installed CLI versions"
./node_modules/.bin/claude --version
./node_modules/.bin/codex --version || true
./node_modules/.bin/opencode --version || true
if command -v agy >/dev/null 2>&1; then
  agy --version || true
else
  echo "  agy (Antigravity CLI) not found on PATH — install: curl -fsSL https://antigravity.google/cli/install.sh | bash"
fi

echo
echo "==> Installed local agent binaries:"
echo "  Claude:   $PROJECT_DIR/node_modules/.bin/claude"
echo "  Codex:    $PROJECT_DIR/node_modules/.bin/codex"
echo "  OpenCode: $PROJECT_DIR/node_modules/.bin/opencode"
echo "  Gemini:   agy (native binary — install via https://antigravity.google/cli/install.sh)"
