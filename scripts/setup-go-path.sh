#!/usr/bin/env bash
set -euo pipefail

bash_login_file() {
  local startup_dir="$1" name
  for name in .bash_profile .bash_login .profile; do
    if [ -f "$startup_dir/$name" ]; then
      printf '%s\n' "$startup_dir/$name"
      return
    fi
  done
  printf '%s/.bash_profile\n' "$startup_dir"
}

persist_go_path() {
  local shell_rc="$1" tool_bin="$2" path_line
  # The guard prevents duplicate PATH entries when a login profile sources .bashrc.
  printf -v path_line 'case ":$PATH:" in *:%q:*) ;; *) export PATH=%q:"$PATH" ;; esac' "$tool_bin" "$tool_bin"
  if ! grep -Fqx -- "$path_line" "$shell_rc" 2>/dev/null; then
    printf '\n# Go development tools (make setup)\n%s\n' "$path_line" >> "$shell_rc"
  fi
  printf 'Go tools PATH configured in %s.\n' "$shell_rc"
}

setup_go_path() {
  local tool_bin go_path
  tool_bin="$(go env GOBIN)"
  if [ -z "$tool_bin" ]; then
    go_path="$(go env GOPATH)"
    tool_bin="${go_path%%:*}/bin"
  fi

  if [ -n "${SETUP_SHELL_RC:-}" ]; then
    persist_go_path "$SETUP_SHELL_RC" "$tool_bin"
  else
    case "${SHELL:-}" in
      */bash)
        persist_go_path "$HOME/.bashrc" "$tool_bin"
        persist_go_path "$(bash_login_file "$HOME")" "$tool_bin"
        ;;
      */zsh) persist_go_path "${ZDOTDIR:-$HOME}/.zshrc" "$tool_bin" ;;
      *) echo "Unsupported shell: ${SHELL:-unset}. Add $tool_bin to PATH in your shell configuration." >&2; return 1 ;;
    esac
  fi

  printf 'For this terminal, run: case ":$PATH:" in *:%q:*) ;; *) export PATH=%q:"$PATH" ;; esac\n' "$tool_bin" "$tool_bin"
}

if [[ "${BASH_SOURCE[0]}" == "$0" ]]; then
  setup_go_path
fi
