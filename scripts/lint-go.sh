#!/usr/bin/env bash
set -euo pipefail

export GOLANGCI_LINT_CACHE="${GOLANGCI_LINT_CACHE:-/tmp/golangci-lint}"
export GOCACHE="${GOCACHE:-/tmp/go-build}"
export GOMODCACHE="${GOMODCACHE:-/tmp/go-mod}"
export GOFLAGS="${GOFLAGS:-} -buildvcs=false"

golangci-lint run ./...
