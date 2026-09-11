#!/usr/bin/env bash
set -euo pipefail

CLIENT_STATE=/home/testuser/.config/bridgectl
RESULTS_DIR=/results
RESULTS_FILE="$RESULTS_DIR/test-results.txt"
STEP_CA_ROOT=/step-ca/shared/root_ca.crt

mkdir -p "$RESULTS_DIR"

cleanup() {
  local rc=$?
  echo "" >> "$RESULTS_FILE"
  if [ $rc -eq 0 ]; then
    echo "=== RESULT: PASS ===" | tee -a "$RESULTS_FILE"
  else
    echo "=== RESULT: FAIL (exit code $rc) ===" | tee -a "$RESULTS_FILE"
  fi
  cat "$RESULTS_FILE"
}
trap cleanup EXIT

echo "=== Remote Step CA E2E Test ===" | tee "$RESULTS_FILE"
echo "Started: $(date -u +%Y-%m-%dT%H:%M:%SZ)" | tee -a "$RESULTS_FILE"
echo "" | tee -a "$RESULTS_FILE"

echo "==> Setting up client user..."
useradd -m -s /bin/bash testuser 2>/dev/null || true

echo "==> Waiting for Step CA root cert..."
for i in $(seq 1 60); do
  if [ -f "$STEP_CA_ROOT" ]; then
    echo "    Step CA root found." | tee -a "$RESULTS_FILE"
    break
  fi
  if [ "$i" -eq 60 ]; then
    echo "ERROR: timed out waiting for Step CA root" | tee -a "$RESULTS_FILE"
    exit 1
  fi
  sleep 1
done

echo "" | tee -a "$RESULTS_FILE"
echo "==> Step: bridgectl client init (Step CA + enroll)" | tee -a "$RESULTS_FILE"
mkdir -p "$CLIENT_STATE/certs"
# The SDK tests discover a local CA bundle; client init uses STEP_CA_ROOT directly.
cp "$STEP_CA_ROOT" "$CLIENT_STATE/certs/ca-bundle.crt"

PW_FILE=$(mktemp)
echo -n "$STEP_CA_PROVISIONER_PASSWORD" > "$PW_FILE"

BRIDGECTL_STATE_DIR="$CLIENT_STATE" bridgectl client init \
  --step-ca-url "$STEP_CA_URL" \
  --step-ca-root "$STEP_CA_ROOT" \
  --provisioner bridge-jwk \
  --step-ca-provisioner-password-file "$PW_FILE" \
  --name remote-stepca-client \
  --target "$BRIDGE_SERVER" 2>&1 | tee -a "$RESULTS_FILE"

rm -f "$PW_FILE"

echo "" | tee -a "$RESULTS_FILE"
echo "==> Step: bridgectl identity show" | tee -a "$RESULTS_FILE"
BRIDGECTL_STATE_DIR="$CLIENT_STATE" bridgectl identity show 2>&1 | tee -a "$RESULTS_FILE"

echo "" | tee -a "$RESULTS_FILE"
echo "==> Preparing test repository..."
if [ -d "/tmp/bridgectl/.git" ]; then
  git -C /tmp/bridgectl pull origin main 2>/dev/null || true
else
  git clone --depth 1 https://github.com/orchael/bridge-demo /tmp/bridgectl-src
  cp -a /tmp/bridgectl-src/. /tmp/bridgectl/
  rm -rf /tmp/bridgectl-src
fi
chmod -R a+rwX /tmp/bridgectl

echo "" | tee -a "$RESULTS_FILE"
echo "==> Running Go e2e test suite..." | tee -a "$RESULTS_FILE"
E2E_TEST_TIMEOUT="${E2E_TEST_TIMEOUT:-600s}"

gotestsum \
  --format short-verbose \
  --junitfile "$RESULTS_DIR/remote-stepca-e2e.xml" \
  --raw-command -- test2json -t -p remote-stepca remote-stepca-e2e \
  -test.v \
  -test.timeout "$E2E_TEST_TIMEOUT" \
  -server "$BRIDGE_SERVER" \
  -client-state "$CLIENT_STATE" \
  -repo /tmp/bridgectl 2>&1 | tee -a "$RESULTS_FILE"
