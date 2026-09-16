#!/usr/bin/env bash
set -euo pipefail

export GOCACHE="${GOCACHE:-/tmp/go-build}"
export GOMODCACHE="${GOMODCACHE:-/tmp/go-mod}"
export GOFLAGS="${GOFLAGS:-} -buildvcs=false"

mapfile -t packages < <(go list ./... | grep -v '/node_modules/')

if [ "${#packages[@]}" -eq 0 ]; then
  echo "no Go packages found"
  exit 1
fi

JUNIT_DIR="${JUNIT_DIR:-test-results}"
mkdir -p "$JUNIT_DIR"

if command -v gotestsum &>/dev/null; then
  gotestsum \
    --format short-verbose \
    --junitfile "$JUNIT_DIR/unit-tests.xml" \
    -- -race -count=1 "${packages[@]}"
else
  go test -race -count=1 "${packages[@]}"
fi
