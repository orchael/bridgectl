#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
TMP_DIR="$(mktemp -d)"
GNUPGHOME="$TMP_DIR/gnupg"
REPO_DIR="$TMP_DIR/repo"
PACKAGES_DIR="$TMP_DIR/packages"
HEALTHCHECK_BIN="$TMP_DIR/plain-healthcheck"
SUITES="${SUITES:-noble plucky}"
: "${GOCACHE:=/tmp/bridgectl-go-build}"
: "${GOFLAGS:=-buildvcs=false}"

export GOCACHE
export GOFLAGS

cleanup() {
  local rc=$?
  for suite in $SUITES; do
    docker rm -f "apt-smoke-$suite" >/dev/null 2>&1 || true
  done
  rm -rf "$TMP_DIR"
  exit "$rc"
}

trap cleanup EXIT

mkdir -p "$GNUPGHOME"
chmod 700 "$GNUPGHOME"

gpg --batch \
  --homedir "$GNUPGHOME" \
  --pinentry-mode loopback \
  --passphrase '' \
  --quick-generate-key \
  "AI Agent Bridge Smoke <smoke@bridgectl.local>" \
  rsa3072 sign 0

export GNUPGHOME
VERSION="0.0.0-smoke" OUTPUT_DIR="$PACKAGES_DIR" "$ROOT_DIR/scripts/build-deb.sh"
REPO_DIR="$REPO_DIR" PACKAGES_DIR="$PACKAGES_DIR" "$ROOT_DIR/scripts/build-apt-repo.sh"

go build -o "$HEALTHCHECK_BIN" ./e2e/cmd/plain-healthcheck

run_suite() {
  local suite="$1"
  local container="apt-smoke-$suite"
  local image_tag=""

  case "$suite" in
    noble)
      image_tag="24.04"
      ;;
    plucky)
      image_tag="25.04"
      ;;
    *)
      echo "APT SMOKE FAILED: unsupported suite=$suite" >&2
      exit 1
      ;;
  esac

  docker run -d \
    --name "$container" \
    --add-host=host.docker.internal:host-gateway \
    -v "$REPO_DIR:/opt/aptrepo:ro" \
    -v "$HEALTHCHECK_BIN:/usr/local/bin/plain-healthcheck:ro" \
    ubuntu:"$image_tag" \
    bash -lc '
      set -euo pipefail
      export DEBIAN_FRONTEND=noninteractive
      apt-get update
      apt-get install -y ca-certificates gnupg
      install -d /etc/apt/keyrings
      gpg --dearmor -o /etc/apt/keyrings/bridgectl.gpg /opt/aptrepo/bridgectl-archive-keyring.asc
      echo "deb [arch=amd64 signed-by=/etc/apt/keyrings/bridgectl.gpg] file:/opt/aptrepo '"$suite"' main" > /etc/apt/sources.list.d/bridgectl.list
      apt-get update
      apt-get install -y bridgectl
      /usr/bin/bridgectl server start --config /etc/bridgectl/bridge.yaml >/tmp/bridge.log 2>&1 &
      bridge_pid=$!
      for i in $(seq 1 15); do
        if ! kill -0 "$bridge_pid" 2>/dev/null; then
          cat /tmp/bridge.log >&2
          exit 1
        fi
        sleep 1
      done
      tail -f /dev/null
    ' >/dev/null

  # The container's own apt-get update/install sequence (ca-certificates,
  # gnupg, then bridgectl itself) runs entirely before bridgectl can start,
  # so this budget has to absorb two apt-get update passes and a package
  # install under whatever mirror speed the CI runner gets that day, not
  # just bridgectl's own startup time. 30s was tight enough to flake on
  # ordinary network variance; 2 minutes gives real headroom while staying
  # well inside the job's 25-minute timeout.
  for _ in $(seq 1 120); do
    if docker exec "$container" /usr/local/bin/plain-healthcheck >/dev/null 2>&1; then
      echo "APT SMOKE PASSED: suite=$suite"
      return
    fi
    sleep 1
  done

  docker exec "$container" sh -lc 'cat /tmp/bridge.log' >&2 || true
  docker logs "$container" >&2 || true
  echo "APT SMOKE FAILED: suite=$suite" >&2
  exit 1
}

for suite in $SUITES; do
  run_suite "$suite"
done
