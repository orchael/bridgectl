#!/usr/bin/env bash
set -euo pipefail

threshold="${GO_COVERAGE_THRESHOLD:-75}"
profile="$(mktemp)"
log_file="$(mktemp)"
trap 'rm -f "$profile" "$log_file"' EXIT

export GOCACHE="${GOCACHE:-/tmp/go-build}"
export GOMODCACHE="${GOMODCACHE:-/tmp/go-mod}"

packages=(
  ./internal/auth
  ./internal/server
  ./internal/provider
  ./internal/bridge
  ./internal/config
  ./internal/pki
  ./internal/redact
  ./pkg/...
)

go test "${packages[@]}" -coverprofile="$profile" >"$log_file" 2>&1 || test_exit=$?
cat "$log_file"
if [[ "${test_exit:-0}" -ne 0 ]]; then
  exit "$test_exit"
fi

coverage="$(go tool cover -func="$profile" | awk '/^total:/{gsub("%","",$3); print $3}')"
if [[ -z "$coverage" ]]; then
  echo "failed to calculate coverage" >&2
  exit 1
fi

awk -v cov="$coverage" -v threshold="$threshold" 'BEGIN {
  if (cov + 0 < threshold + 0) {
    printf("coverage %.1f%% is below threshold %.1f%%\n", cov, threshold) > "/dev/stderr"
    exit 1
  }
}'

printf 'coverage %.1f%% meets threshold %.1f%%\n' "$coverage" "$threshold"
