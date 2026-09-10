#!/usr/bin/env bash
set -euo pipefail

# Builds the Debian package, installs it in an Ubuntu container, and verifies
# that the packaged provider runtime installer can run as a non-root login user.

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
TMP_DIR="$(mktemp -d)"
PACKAGES_DIR="$TMP_DIR/packages"
CONTAINER="provider-runtime-user-smoke-$$"

: "${SUITE:=noble}"
: "${GOCACHE:=/tmp/bridgectl-go-build}"
: "${GOFLAGS:=-buildvcs=false}"

export GOCACHE
export GOFLAGS

cleanup() {
  docker rm -f "$CONTAINER" >/dev/null 2>&1 || true
  rm -rf "$TMP_DIR"
}
trap cleanup EXIT

case "$SUITE" in
  noble)  IMAGE="ubuntu:24.04" ;;
  plucky) IMAGE="ubuntu:25.04" ;;
  *)
    echo "PROVIDER RUNTIME USER SMOKE FAILED: unsupported suite=$SUITE" >&2
    exit 1
    ;;
esac

VERSION="0.0.0-provider-runtime-smoke" OUTPUT_DIR="$PACKAGES_DIR" "$ROOT_DIR/scripts/build-deb.sh"
DEB_PATH="$PACKAGES_DIR/bridgectl_0.0.0-provider-runtime-smoke_amd64.deb"
DEB_BASENAME="$(basename "$DEB_PATH")"

docker run -d \
  --name "$CONTAINER" \
  -v "$DEB_PATH:/tmp/$DEB_BASENAME:ro" \
  "$IMAGE" \
  bash -lc '
    set -euo pipefail
    export DEBIAN_FRONTEND=noninteractive
    apt-get update -qq
    dpkg -i /tmp/'"$DEB_BASENAME"' || true
    apt-get install -f -y -qq
    if ! id -u ubuntu >/dev/null 2>&1; then
      useradd -m -s /bin/bash ubuntu
    fi
    install -d -m 0755 /tmp/fakebin
    cat >/tmp/fakebin/node <<'"'"'EOF_NODE'"'"'
#!/bin/sh
if [ "$1" = "--version" ]; then
  echo "v24.0.0"
  exit 0
fi
exit 0
EOF_NODE
    cat >/tmp/fakebin/npm <<'"'"'EOF_NPM'"'"'
#!/bin/sh
set -eu
if [ "$1" != "ci" ]; then
  echo "unexpected npm command: $*" >&2
  exit 1
fi
mkdir -p node_modules/.bin
for cli in claude codex opencode; do
  cat >"node_modules/.bin/$cli" <<EOF_CLI
#!/bin/sh
echo "$cli smoke-version"
EOF_CLI
  chmod 0755 "node_modules/.bin/$cli"
done
EOF_NPM
    chmod 0755 /tmp/fakebin/node /tmp/fakebin/npm
    touch /tmp/provider-runtime-smoke-ready
    tail -f /dev/null
  ' >/dev/null

for _ in $(seq 1 60); do
  if docker exec "$CONTAINER" test -f /tmp/provider-runtime-smoke-ready >/dev/null 2>&1; then
    break
  fi
  sleep 1
done

if ! docker exec "$CONTAINER" test -f /tmp/provider-runtime-smoke-ready >/dev/null 2>&1; then
  docker logs "$CONTAINER" >&2 || true
  echo "PROVIDER RUNTIME USER SMOKE FAILED: container setup did not complete" >&2
  exit 1
fi

docker exec "$CONTAINER" su - ubuntu -c \
  'if PATH=/tmp/fakebin:$PATH INSTALL_DIR=relative/providers /usr/lib/bridgectl/install-provider-runtime >/tmp/relative-install-dir.out 2>&1; then cat /tmp/relative-install-dir.out >&2; exit 1; fi'
docker exec "$CONTAINER" su - ubuntu -c \
  'grep -q "INSTALL_DIR must be an absolute path" /tmp/relative-install-dir.out'
docker exec "$CONTAINER" su - ubuntu -c \
  'if PATH=/tmp/fakebin:$PATH XDG_DATA_HOME=relative-data /usr/lib/bridgectl/install-provider-runtime >/tmp/relative-xdg-data-home.out 2>&1; then cat /tmp/relative-xdg-data-home.out >&2; exit 1; fi'
docker exec "$CONTAINER" su - ubuntu -c \
  'grep -q "INSTALL_DIR must be an absolute path" /tmp/relative-xdg-data-home.out'

docker exec "$CONTAINER" su - ubuntu -c \
  'PATH=/tmp/fakebin:$PATH /usr/lib/bridgectl/install-provider-runtime'

docker exec "$CONTAINER" su - ubuntu -c \
  'test -x "$HOME/.local/share/bridgectl/providers/node_modules/.bin/codex"'
docker exec "$CONTAINER" su - ubuntu -c \
  'test -w "$HOME/.local/share/bridgectl/providers"'
docker exec "$CONTAINER" sh -c \
  'test ! -d /opt/bridgectl'

echo "PROVIDER RUNTIME USER SMOKE PASSED: suite=$SUITE"
