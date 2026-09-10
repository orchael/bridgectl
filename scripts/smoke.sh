#!/usr/bin/env bash
set -euo pipefail

compose=(docker compose -f docker-compose.yml -f docker-compose.smoke.yaml)

cleanup() {
  "${compose[@]}" logs bridge || true
  "${compose[@]}" down -v --remove-orphans || true
}

trap cleanup EXIT

echo "==> Building and starting smoke stack"
mkdir -p certs
"${compose[@]}" up -d --build bridge

echo "==> Waiting for auto-generated certs to appear"
for i in $(seq 1 60); do
  if [ -f certs/ca-bundle.crt ] && [ -f certs/dev-client.crt ] && [ -f certs/dev-client.key ] && [ -f certs/jwt-signing.key ]; then
    break
  fi
  sleep 1
done
if [ ! -f certs/ca-bundle.crt ]; then
  echo "SMOKE TEST FAILED: certs not generated after 60s" >&2
  exit 1
fi

echo "==> Waiting for bridge health via gRPC"
go run ./e2e/cmd/smoke \
  -target 127.0.0.1:9445 \
  -cacert certs/ca-bundle.crt \
  -cert certs/dev-client.crt \
  -key certs/dev-client.key \
  -jwt-key certs/jwt-signing.key \
  -issuer smoke-step-client
