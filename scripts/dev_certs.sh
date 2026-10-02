#!/usr/bin/env bash
set -euo pipefail

# Generate development certificates for local testing.
# Requires: bin/bridge-ca (run 'make build' first)

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_DIR="$(dirname "$SCRIPT_DIR")"
CA_BIN="$PROJECT_DIR/bin/bridge-ca"
CERTS_DIR="$PROJECT_DIR/certs"

if [ ! -x "$CA_BIN" ]; then
    echo "bridge-ca not found. Run 'make build' first."
    exit 1
fi

# Idempotent: this script is a prerequisite of dev-session-claude/dev-session-codex
# (via `make dev-setup`), so it runs on every session start. Without this guard
# it would mint a brand-new CA and reissue every cert each time, which is both
# wasteful and prints a wall of irrelevant output ahead of the actual session.
#
# Check every artifact this script produces, not just ca.crt: if a prior run
# failed partway through (e.g. after `init` but before `issue`/`bundle`/
# `jwt-keygen`), ca.crt alone would exist and this would skip regeneration,
# leaving the rest permanently missing.
EXPECTED_FILES=(
    "$CERTS_DIR/ca.crt" "$CERTS_DIR/ca.key"
    "$CERTS_DIR/bridge.local.crt" "$CERTS_DIR/bridge.local.key"
    "$CERTS_DIR/dev-client.crt" "$CERTS_DIR/dev-client.key"
    "$CERTS_DIR/ca-bundle.crt"
    "$CERTS_DIR/jwt-signing.pub" "$CERTS_DIR/jwt-signing.key"
)
present=0
for f in "${EXPECTED_FILES[@]}"; do
    [ -f "$f" ] && present=$((present + 1))
done
if [ "$present" -eq "${#EXPECTED_FILES[@]}" ]; then
    echo "==> Dev certificates already exist in $CERTS_DIR — skipping (remove the directory to regenerate)"
    exit 0
elif [ "$present" -gt 0 ]; then
    echo "==> $CERTS_DIR has a partial certificate set ($present/${#EXPECTED_FILES[@]} expected files)." >&2
    echo "    A prior run likely failed partway through. Remove $CERTS_DIR and rerun to regenerate." >&2
    exit 1
fi

echo "==> Generating dev certificates in $CERTS_DIR"

# 1. Initialize bridge CA
echo "--- Initializing bridge CA"
$CA_BIN init --name "bridgectl-dev" --out "$CERTS_DIR"

# 2. Issue bridge server cert
echo "--- Issuing bridge server certificate"
$CA_BIN issue --type server --cn "bridge.local" \
    --san "bridge.local,localhost,127.0.0.1" \
    --ca "$CERTS_DIR/ca.crt" --ca-key "$CERTS_DIR/ca.key" \
    --out "$CERTS_DIR"

# 3. Issue dev client cert (for testing without consumer project CAs)
echo "--- Issuing dev client certificate"
$CA_BIN issue --type client --cn "dev-client" \
    --ca "$CERTS_DIR/ca.crt" --ca-key "$CERTS_DIR/ca.key" \
    --out "$CERTS_DIR"

# 4. Build trust bundle (just the bridge CA for dev)
echo "--- Building trust bundle"
$CA_BIN bundle --out "$CERTS_DIR/ca-bundle.crt" "$CERTS_DIR/ca.crt"

# 5. Generate JWT signing keypair
echo "--- Generating JWT signing keypair"
$CA_BIN jwt-keygen --out "$CERTS_DIR/jwt-signing"

echo ""
echo "==> Dev certificates generated successfully!"
echo ""
echo "Files:"
echo "  CA:          $CERTS_DIR/ca.crt"
echo "  Server cert: $CERTS_DIR/bridge.local.crt"
echo "  Server key:  $CERTS_DIR/bridge.local.key"
echo "  Client cert: $CERTS_DIR/dev-client.crt"
echo "  Client key:  $CERTS_DIR/dev-client.key"
echo "  Trust bundle: $CERTS_DIR/ca-bundle.crt"
echo "  JWT pub key: $CERTS_DIR/jwt-signing.pub"
echo "  JWT priv key: $CERTS_DIR/jwt-signing.key"
echo ""
echo "To start the bridge with dev certs, create a config like:"
echo ""
echo "  server:"
echo "    listen: \"0.0.0.0:9445\""
echo "  tls:"
echo "    ca_bundle: \"$CERTS_DIR/ca-bundle.crt\""
echo "    cert: \"$CERTS_DIR/bridge.local.crt\""
echo "    key: \"$CERTS_DIR/bridge.local.key\""
echo "  auth:"
echo "    jwt_public_keys:"
echo "      - issuer: \"dev\""
echo "        key_path: \"$CERTS_DIR/jwt-signing.pub\""
