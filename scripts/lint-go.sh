#!/usr/bin/env bash
set -euo pipefail

export GOLANGCI_LINT_CACHE="${GOLANGCI_LINT_CACHE:-/tmp/golangci-lint}"
export GOCACHE="${GOCACHE:-/tmp/go-build}"
export GOMODCACHE="${GOMODCACHE:-/tmp/go-mod}"
export GOFLAGS="${GOFLAGS:-} -buildvcs=false"

# See scripts/goimports.sh for why this is needed: golangci-lint lives in
# `go env GOBIN` (or GOPATH/bin), which may not be on PATH for this
# invocation (pre-commit hook subprocess, fresh shell, etc.).
tool_bin="$(go env GOBIN)"
if [ -z "$tool_bin" ]; then
  tool_bin="$(go env GOPATH | cut -d: -f1)/bin"
fi
export PATH="$tool_bin:$PATH"

command -v golangci-lint >/dev/null 2>&1 || {
  echo "golangci-lint is not installed. Run 'make deps' to install it." >&2
  exit 1
}

golangci-lint run ./...
