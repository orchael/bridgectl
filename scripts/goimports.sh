#!/usr/bin/env bash
set -euo pipefail

# `make deps` installs goimports into `go env GOBIN` (or GOPATH/bin), but
# that directory isn't necessarily on the caller's PATH — a fresh shell
# that hasn't re-sourced its profile since `make setup` appended to it, or
# a pre-commit hook subprocess. Resolve it the same way `make deps`
# reports the install location so `make fmt` and the pre-commit hook find
# it reliably regardless of the caller's PATH.
tool_bin="$(go env GOBIN)"
if [ -z "$tool_bin" ]; then
  tool_bin="$(go env GOPATH | cut -d: -f1)/bin"
fi
export PATH="$tool_bin:$PATH"

command -v goimports >/dev/null 2>&1 || {
  echo "goimports is not installed. Run 'make deps' to install it." >&2
  exit 1
}

exec goimports "$@"
