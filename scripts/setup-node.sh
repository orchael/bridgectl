#!/usr/bin/env bash
set -euo pipefail

# Cross-platform Node.js detection and setup guidance.
#
# This script checks whether Node.js is installed and meets the version
# requirement from .nvmrc. When Node.js is missing or outdated, it prints
# platform-specific installation instructions without forcing any changes.
#
# Usage:
#   scripts/setup-node.sh          Check and advise.
#   scripts/setup-node.sh --install  Attempt automatic install (macOS Homebrew only).

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"

# Read required major version from .nvmrc
if [ ! -f "$PROJECT_ROOT/.nvmrc" ]; then
  echo "ERROR: .nvmrc not found in $PROJECT_ROOT" >&2
  exit 1
fi
REQUIRED_MAJOR="$(tr -d '[:space:]' < "$PROJECT_ROOT/.nvmrc")"

AUTO_INSTALL=0
for arg in "$@"; do
  case "$arg" in
    --install) AUTO_INSTALL=1 ;;
    *) echo "setup-node: unknown argument: $arg" >&2; exit 1 ;;
  esac
done

detect_os() {
  case "$(uname -s)" in
    Darwin)  echo "macos" ;;
    Linux)   echo "linux" ;;
    MINGW*|MSYS*|CYGWIN*) echo "windows" ;;
    *)       echo "unknown" ;;
  esac
}

node_installed() {
  command -v node >/dev/null 2>&1
}

node_major() {
  node --version 2>/dev/null | sed 's/v//;s/\..*//'
}

pnpm_installed() {
  command -v pnpm >/dev/null 2>&1
}

# Runs a corepack command, retrying with sudo if it fails for permission
# reasons (e.g. Node installed system-wide via apt/nodesource, where the
# corepack shims must be symlinked into a root-owned bin directory).
run_corepack() {
  local err_file
  err_file="$(mktemp)"
  if corepack "$@" 2>"$err_file"; then
    rm -f "$err_file"
    return 0
  fi
  cat "$err_file" >&2
  rm -f "$err_file"
  if [ "$(id -u)" -ne 0 ] && command -v sudo >/dev/null 2>&1; then
    echo "Retrying 'corepack $*' with sudo (corepack needs root to update a system-wide Node install)..."
    sudo corepack "$@"
    return $?
  fi
  return 1
}

ensure_pnpm() {
  if pnpm_installed; then
    echo "pnpm detected: $(pnpm --version) ($(command -v pnpm))"
    return 0
  fi

  if ! command -v corepack >/dev/null 2>&1; then
    echo "pnpm not found and corepack is not available." >&2
    echo "Install pnpm manually: https://pnpm.io/installation" >&2
    return 1
  fi

  echo "pnpm not found. Installing via corepack..."
  run_corepack enable || { echo "Failed to run 'corepack enable'." >&2; return 1; }
  run_corepack prepare pnpm@latest --activate || { echo "Failed to run 'corepack prepare pnpm@latest --activate'." >&2; return 1; }

  if pnpm_installed; then
    echo "pnpm installed: $(pnpm --version) ($(command -v pnpm))"
  else
    echo "corepack reported success but pnpm is still not on PATH." >&2
    echo "Open a new shell, or re-check PATH, then rerun 'make deps'." >&2
    return 1
  fi
}

brew_installed() {
  command -v brew >/dev/null 2>&1
}

print_header() {
  echo ""
  echo "=== Node.js Setup Check ==="
  echo ""
  echo "Required Node.js version: ${REQUIRED_MAJOR} (from .nvmrc)"
  echo ""
}

print_nvm_instructions() {
  echo "  Option 1: nvm (recommended)"
  echo "    Install nvm: https://github.com/nvm-sh/nvm"
  echo "    Then run:"
  echo "      nvm install"
  echo "      nvm use"
  echo ""
}

print_macos_instructions() {
  echo "  Option 2: Homebrew (macOS)"
  if brew_installed; then
    echo "    brew install node@${REQUIRED_MAJOR}"
    echo ""
    echo "    Or auto-install with:"
    echo "      scripts/setup-node.sh --install"
  else
    echo "    Install Homebrew first: https://brew.sh"
    echo "    Then: brew install node@${REQUIRED_MAJOR}"
  fi
  echo ""
  echo "  Option 3: Direct download"
  echo "    https://nodejs.org/en/download/"
  echo ""
}

print_linux_instructions() {
  echo "  Option 2: Package manager"
  if command -v apt-get >/dev/null 2>&1; then
    echo "    # Ubuntu / Debian (via NodeSource):"
    echo "    curl -fsSL https://deb.nodesource.com/setup_${REQUIRED_MAJOR}.x | sudo -E bash -"
    echo "    sudo apt-get install -y nodejs"
  elif command -v dnf >/dev/null 2>&1; then
    echo "    # Fedora / RHEL:"
    echo "    curl -fsSL https://rpm.nodesource.com/setup_${REQUIRED_MAJOR}.x | sudo bash -"
    echo "    sudo dnf install -y nodejs"
  elif command -v pacman >/dev/null 2>&1; then
    echo "    # Arch Linux:"
    echo "    sudo pacman -S nodejs npm"
  else
    echo "    Use your distribution's package manager to install Node.js ${REQUIRED_MAJOR}."
  fi
  echo ""
  echo "  Option 3: Direct download"
  echo "    https://nodejs.org/en/download/"
  echo ""
}

print_windows_instructions() {
  echo "  Option 2: winget"
  echo "    winget install OpenJS.NodeJS --version ${REQUIRED_MAJOR}"
  echo ""
  echo "  Option 3: Chocolatey"
  echo "    choco install nodejs --version=${REQUIRED_MAJOR}"
  echo ""
  echo "  Option 4: Direct download"
  echo "    https://nodejs.org/en/download/"
  echo ""
}

print_instructions() {
  local os="$1"
  print_nvm_instructions
  case "$os" in
    macos)   print_macos_instructions ;;
    linux)   print_linux_instructions ;;
    windows) print_windows_instructions ;;
    *)
      echo "  Option 2: Direct download"
      echo "    https://nodejs.org/en/download/"
      echo ""
      ;;
  esac
}

try_brew_install() {
  if ! brew_installed; then
    echo "Homebrew not found. Install Homebrew first: https://brew.sh"
    return 1
  fi
  echo "Installing Node.js ${REQUIRED_MAJOR} via Homebrew..."
  brew install "node@${REQUIRED_MAJOR}"
  # Homebrew keg-only formulae need to be linked or added to PATH.
  if ! node_installed; then
    echo ""
    echo "Node was installed but is keg-only. You may need to add it to PATH:"
    echo "  export PATH=\"\$(brew --prefix)/opt/node@${REQUIRED_MAJOR}/bin:\$PATH\""
    echo ""
    echo "Or link it:"
    echo "  brew link --overwrite node@${REQUIRED_MAJOR}"
    return 1
  fi
}

# Loads nvm into this shell if it is installed (nvm is a shell function, so
# it is invisible to a non-interactive script until nvm.sh is sourced).
# nvm.sh is not written for set -eu, so relax them while it runs.
load_nvm() {
  local nvm_sh="${NVM_DIR:-$HOME/.nvm}/nvm.sh"
  [ -s "$nvm_sh" ] || return 1
  set +eu
  # shellcheck disable=SC1090
  . "$nvm_sh" --no-use >/dev/null 2>&1
  local rc=$?
  set -eu
  [ "$rc" -eq 0 ] && type nvm >/dev/null 2>&1
}

# Installs (if needed) and activates the .nvmrc version through nvm, so the
# developer does not have to run `nvm install && nvm use` by hand. This only
# affects this script's process; a later shell still needs `nvm use` (or an
# nvm default) to pick it up.
try_nvm_use() {
  load_nvm || return 1
  echo "nvm detected: installing/activating Node ${REQUIRED_MAJOR} from .nvmrc..."
  # nvm reads .nvmrc from the current directory, and `nvm use` must run in
  # this shell (not a subshell) for the PATH change to stick.
  local rc=0
  pushd "$PROJECT_ROOT" >/dev/null
  set +eu
  nvm install >/dev/null && nvm use >/dev/null
  rc=$?
  set -eu
  popd >/dev/null
  if [ "$rc" -ne 0 ]; then
    echo "nvm could not install/activate Node ${REQUIRED_MAJOR}." >&2
    return 1
  fi
  return 0
}

# ── Main ──────────────────────────────────────────────────────────────────

OS="$(detect_os)"
print_header

if ! node_installed || [ "$(node_major)" != "$REQUIRED_MAJOR" ]; then
  # nvm owns the Node version when present: run it instead of telling the
  # user to. `nvm use` modifies PATH in this shell, so re-detect afterwards.
  if try_nvm_use; then
    hash -r
    NVM_ACTIVATED=1
  fi
fi

if node_installed; then
  ACTUAL_MAJOR="$(node_major)"
  echo "Node.js detected: v${ACTUAL_MAJOR} ($(command -v node))"

  if [ "$ACTUAL_MAJOR" = "$REQUIRED_MAJOR" ]; then
    echo "Version OK: matches .nvmrc requirement (${REQUIRED_MAJOR})"
    if [ "${NVM_ACTIVATED:-0}" -eq 1 ]; then
      echo "(activated via nvm for this run; run 'nvm use' in your own shell to match)"
    fi
    echo ""

    # Also ensure pnpm is available.
    ensure_pnpm

    echo ""
    echo "=== Node.js setup is complete ==="
    exit 0
  else
    echo "WARNING: Node.js ${ACTUAL_MAJOR} found but ${REQUIRED_MAJOR} is required."
    echo ""
    echo "Upgrade Node.js using one of these methods:"
    echo ""
    print_instructions "$OS"
    exit 1
  fi
else
  echo "Node.js is not installed."
  echo ""

  if [ "$AUTO_INSTALL" -eq 1 ] && [ "$OS" = "macos" ]; then
    try_brew_install
    exit $?
  fi

  echo "Install Node.js ${REQUIRED_MAJOR} using one of these methods:"
  echo ""
  print_instructions "$OS"
  exit 1
fi
