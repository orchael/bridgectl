#!/usr/bin/env bash
set -euo pipefail

# Persist the Go tool directory for future interactive Bash or Zsh sessions.
tool_bin="$(go env GOBIN)"
if [ -z "$tool_bin" ]; then
  go_path="$(go env GOPATH)"
  tool_bin="${go_path%%:*}/bin"
fi

if [ -n "${SETUP_SHELL_RC:-}" ]; then
  shell_rc="$SETUP_SHELL_RC"
else
  case "${SHELL:-}" in
    */bash) shell_rc="$HOME/.bashrc" ;;
    */zsh) shell_rc="${ZDOTDIR:-$HOME}/.zshrc" ;;
    *) echo "Unsupported shell: ${SHELL:-unset}. Add $tool_bin to PATH in your shell configuration." >&2; exit 1 ;;
  esac
fi

printf -v path_line 'export PATH=%q:"$PATH"' "$tool_bin"
if ! grep -Fqx -- "$path_line" "$shell_rc" 2>/dev/null; then
  printf '\n# Go development tools (make setup)\n%s\n' "$path_line" >> "$shell_rc"
fi

printf 'Go tools PATH configured in %s.\n' "$shell_rc"
printf 'For this terminal, run: %s\n' "$path_line"
