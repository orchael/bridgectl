#!/usr/bin/env bash
set -euo pipefail

test_dir="$(mktemp -d)"
trap 'rm -rf "$test_dir"' EXIT
export SETUP_SHELL_RC="$test_dir/custom-rc"
source "$(dirname "$0")/setup-go-path.sh"

# Bash login startup precedence must match Bash, without hiding an existing profile.
test "$(bash_login_file "$test_dir")" = "$test_dir/.bash_profile"
touch "$test_dir/.profile"
test "$(bash_login_file "$test_dir")" = "$test_dir/.profile"
touch "$test_dir/.bash_login"
test "$(bash_login_file "$test_dir")" = "$test_dir/.bash_login"
touch "$test_dir/.bash_profile"
test "$(bash_login_file "$test_dir")" = "$test_dir/.bash_profile"

# Configure both shell modes and keep repeated setup and sourcing idempotent.
for startup_file in "$test_dir/.bashrc" "$(bash_login_file "$test_dir")"; do
  persist_go_path "$startup_file" "$test_dir/tools with spaces"
  persist_go_path "$startup_file" "$test_dir/tools with spaces"
  test "$(grep -c '^case ' "$startup_file")" = 1
done
bash --noprofile --norc -c '
  source "$1/.bashrc"
  source "$1/.bash_profile"
  expected="$1/tools with spaces"
  test "${PATH%%:*}" = "$expected"
  remaining="${PATH#*:}"
  case ":$remaining:" in *":$expected:"*) exit 1 ;; esac
' _ "$test_dir"
echo "Go PATH setup tests passed."
