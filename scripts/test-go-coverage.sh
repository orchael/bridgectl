#!/usr/bin/env bash
set -euo pipefail

export GOCACHE="${GOCACHE:-/tmp/go-build}"
export GOMODCACHE="${GOMODCACHE:-/tmp/go-mod}"

# Include the real CLI exercised by the e2e suite in the coverage report.
cli_coverage_dir="$(mktemp -d)"
trap 'rm -rf "$cli_coverage_dir"' EXIT
export BRIDGECTL_CLI_COVERAGE_DIR="$cli_coverage_dir"

mapfile -t packages < <(go list ./... | grep -v '/node_modules/')

if [ "${#packages[@]}" -eq 0 ]; then
  echo "no Go packages found"
  exit 1
fi

go test -race -covermode=atomic -coverprofile=coverage.out "${packages[@]}"
go tool covdata textfmt -i="$cli_coverage_dir" -o="$cli_coverage_dir/cli.out"
# Both profiles use atomic counters; repeated blocks are additive.
tail -n +2 "$cli_coverage_dir/cli.out" >> coverage.out
