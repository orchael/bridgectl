#!/usr/bin/env bash
set -euo pipefail

CERT_DIR="${CERT_DIR:-/app/certs}"
RUNTIME_CERT_DIR="${RUNTIME_CERT_DIR:-/run/bridge-certs}"
BRIDGE_CONFIG="${BRIDGE_CONFIG:-/app/config/bridge-docker.yaml}"
BRIDGE_CN="${BRIDGE_CN:-bridge.local}"
BRIDGE_SANS="${BRIDGE_SANS:-bridge.local,bridge,localhost,127.0.0.1}"
BRIDGE_CLIENT_CN="${BRIDGE_CLIENT_CN:-dev-client}"
STEP_CA_URL="${STEP_CA_URL:-}"
STEP_CA_ROOT="${STEP_CA_ROOT:-}"
STEP_CA_PROVISIONER="${STEP_CA_PROVISIONER:-}"
STEP_CA_PROVISIONER_PASSWORD="${STEP_CA_PROVISIONER_PASSWORD:-}"

exec_as_bridge() {
  local quoted=""
  printf -v quoted '%q ' "$@"
  exec su -m -s /bin/bash bridge -c "cd /app && export HOME=/home/bridge && exec ${quoted}"
}

# Running as root — prepare directories the bridge user needs.
mkdir -p "$CERT_DIR" "$RUNTIME_CERT_DIR"
chown bridge:bridge "$CERT_DIR"
mkdir -p /home/bridge/.gemini /home/bridge/.config
# Use non-recursive chown on /home/bridge itself, then chown subdirectories
# individually.  Mounted files (e.g. oauth_creds.json) may be read-only
# bind-mounts, so a blanket `chown -R` would fail on them.
chown bridge:bridge /home/bridge
chown bridge:bridge /home/bridge/.gemini 2>/dev/null || true
chown bridge:bridge /home/bridge/.config 2>/dev/null || true

# Mirror what systemd RuntimeDirectory=bridge does: create and own
# the runtime dir so the bridge process can write the system addr file.
mkdir -p /run/bridge
chown bridge:bridge /run/bridge

# Ensure bridge user can read/write mounted workspace volumes such as /repos.
for _vol in /repos /workspace; do
  if [ -d "$_vol" ]; then
    chown bridge:bridge "$_vol"
  fi
done

if [ -n "$STEP_CA_URL" ] && [ -n "$STEP_CA_ROOT" ]; then
  # Step CA mode: bridgectl handles PKI via EnsurePKI(stepCA).
  # We only need to bootstrap the step CLI with the CA root and provisioner
  # password so that `step ca certificate` works non-interactively.
  echo "==> Step CA mode enabled: ${STEP_CA_URL}"

  # Wait for the Step CA root cert to be available (shared volume).
  for i in $(seq 1 30); do
    if [ -f "$STEP_CA_ROOT" ]; then
      break
    fi
    echo "    Waiting for Step CA root cert at ${STEP_CA_ROOT}..."
    sleep 1
  done
  if [ ! -f "$STEP_CA_ROOT" ]; then
    echo "ERROR: Step CA root cert not found at ${STEP_CA_ROOT} after 30s"
    exit 1
  fi

  # Bootstrap the step CLI so it trusts the Step CA instance.
  su -m -s /bin/bash bridge -c "
    export HOME=/home/bridge
    step ca bootstrap --ca-url '${STEP_CA_URL}' --fingerprint \$(step certificate fingerprint '${STEP_CA_ROOT}') --force 2>/dev/null || true
  "

  # Write provisioner password for non-interactive cert requests.
  if [ -n "$STEP_CA_PROVISIONER_PASSWORD" ]; then
    STEP_PASS_FILE="/run/bridge-certs/step-provisioner-password"
    mkdir -p "$(dirname "$STEP_PASS_FILE")"
    echo "$STEP_CA_PROVISIONER_PASSWORD" > "$STEP_PASS_FILE"
    chown bridge:bridge "$STEP_PASS_FILE"
    chmod 600 "$STEP_PASS_FILE"
  fi

  # Create a minimal dev-client cert using the Tier-1 path so the entrypoint
  # can produce a client cert for immediate use. The server cert is handled by
  # bridgectl server start --step-ca-url.
  if [ ! -f "$CERT_DIR/ca.crt" ]; then
    echo "==> Generating local CA for dev-client credentials..."
    bridge-ca init --name bridge-ca --out "$CERT_DIR"

    echo "==> Issuing dev-client certificate..."
    bridge-ca issue --type client --cn "$BRIDGE_CLIENT_CN" \
      --ca "$CERT_DIR/ca.crt" --ca-key "$CERT_DIR/ca.key" \
      --out "$CERT_DIR"

    echo "==> Generating JWT signing keypair..."
    bridge-ca jwt-keygen --out "$CERT_DIR/jwt-signing"

    chmod 644 "$CERT_DIR"/*
  fi
else
  # Tier-1 auto-PKI: generate everything locally.
  if [ ! -f "$CERT_DIR/ca.crt" ]; then
    echo "==> Initializing CA..."
    bridge-ca init --name bridge-ca --out "$CERT_DIR"

    echo "==> Issuing server certificate..."
    bridge-ca issue --type server --cn "$BRIDGE_CN" \
      --san "$BRIDGE_SANS" \
      --ca "$CERT_DIR/ca.crt" --ca-key "$CERT_DIR/ca.key" \
      --out "$CERT_DIR"

    echo "==> Issuing client certificate..."
    bridge-ca issue --type client --cn "$BRIDGE_CLIENT_CN" \
      --ca "$CERT_DIR/ca.crt" --ca-key "$CERT_DIR/ca.key" \
      --out "$CERT_DIR"

    echo "==> Generating JWT signing keypair..."
    bridge-ca jwt-keygen --out "$CERT_DIR/jwt-signing"

    echo "==> Building trust bundle..."
    bridge-ca bundle --out "$CERT_DIR/ca-bundle.crt" "$CERT_DIR/ca.crt"

    chmod 644 "$CERT_DIR"/*
  fi
fi

# Copy certs to runtime directory with proper permissions.
mkdir -p "$RUNTIME_CERT_DIR"
cp -f "$CERT_DIR"/* "$RUNTIME_CERT_DIR"/ 2>/dev/null || true
chown -R bridge:bridge "$RUNTIME_CERT_DIR"
# Set permissions on files that exist (Step CA mode may not have all files yet).
find "$RUNTIME_CERT_DIR" -name '*.crt' -exec chmod 644 {} + 2>/dev/null || true
find "$RUNTIME_CERT_DIR" -name '*.pub' -exec chmod 644 {} + 2>/dev/null || true
find "$RUNTIME_CERT_DIR" -name '*.key' -exec chmod 600 {} + 2>/dev/null || true

if [ -n "${CLAUDE_CODE_OAUTH_TOKEN:-}" ]; then
  echo "==> Verifying Claude API-key auth..."
  CLAUDE_AUTH_STATUS="$(su -m -s /bin/bash bridge -c 'cd /app && export HOME=/home/bridge && ./node_modules/.bin/claude auth status' || true)"
  if [[ "$CLAUDE_AUTH_STATUS" != *'"loggedIn": true'* ]]; then
    echo "Claude auth verification failed"
    echo "$CLAUDE_AUTH_STATUS"
    exit 1
  fi
fi

echo "==> Seeding Claude onboarding state..."
su -m -s /bin/bash bridge -c 'cd /app && export HOME=/home/bridge && node <<'\''EOF'\''
const fs = require("fs");
const path = require("path");

const statePath = path.join(process.env.HOME, ".claude.json");
const settingsDir = path.join(process.env.HOME, ".claude");
const settingsPath = path.join(settingsDir, "settings.json");
const pkg = require("./node_modules/@anthropic-ai/claude-code/package.json");

let state = {};
try {
  state = JSON.parse(fs.readFileSync(statePath, "utf8"));
} catch (_) {
  state = {};
}

state.theme = state.theme || "dark";
state.hasCompletedOnboarding = true;
state.lastOnboardingVersion = pkg.version;

// Pre-trust the e2e repo path so the trust dialog is suppressed
state.projects = state.projects || {};
const trustedPaths = ["/tmp/bridgectl"];
for (const p of trustedPaths) {
  state.projects[p] = state.projects[p] || {};
  state.projects[p].hasTrustDialogAccepted = true;
  state.projects[p].projectOnboardingSeenCount = 1;
}

// Pre-approve the CLAUDE_CODE_OAUTH_TOKEN so the "use this API key?" dialog is suppressed.
// Claude stores the last 20 chars of the key as the approval token.
const apiKey = process.env.CLAUDE_CODE_OAUTH_TOKEN || "";
if (apiKey) {
  const keyToken = apiKey.slice(-20);
  state.customApiKeyResponses = state.customApiKeyResponses || {};
  const approved = state.customApiKeyResponses.approved || [];
  if (!approved.includes(keyToken)) approved.push(keyToken);
  state.customApiKeyResponses.approved = approved;
  state.customApiKeyResponses.rejected = (state.customApiKeyResponses.rejected || []).filter(k => k !== keyToken);
}

fs.writeFileSync(statePath, JSON.stringify(state, null, 2) + "\n");

fs.mkdirSync(settingsDir, { recursive: true });
let settings = {};
try {
  settings = JSON.parse(fs.readFileSync(settingsPath, "utf8"));
} catch (_) {
settings = {};
}
settings.skipDangerousModePermissionPrompt = true;
fs.writeFileSync(settingsPath, JSON.stringify(settings, null, 2) + "\n");
EOF'

echo "==> Seeding Antigravity CLI (agy) onboarding state..."
su -m -s /bin/bash bridge -c 'cd /app && export HOME=/home/bridge && node <<'\''EOF'\''
const fs = require("fs");
const path = require("path");

const geminiDir = path.join(process.env.HOME, ".gemini");
const agySettingsDir = path.join(geminiDir, "antigravity-cli");
const settingsPath = path.join(agySettingsDir, "settings.json");
const trustedFoldersPath = path.join(geminiDir, "trustedFolders.json");

fs.mkdirSync(agySettingsDir, { recursive: true });

let settings = {};
try {
  settings = JSON.parse(fs.readFileSync(settingsPath, "utf8"));
} catch (_) {
  settings = {};
}

// Disable auto-update notifications so agy does not try to update during e2e tests
settings.general = settings.general || {};
settings.general.enableAutoUpdateNotification = false;
settings.general.autoUpdate = false;

fs.writeFileSync(settingsPath, JSON.stringify(settings, null, 2) + "\n");

let trustedFolders = {};
try {
  trustedFolders = JSON.parse(fs.readFileSync(trustedFoldersPath, "utf8"));
} catch (_) {
  trustedFolders = {};
}

trustedFolders["/tmp/bridgectl"] = "TRUST_FOLDER";

fs.writeFileSync(trustedFoldersPath, JSON.stringify(trustedFolders, null, 2) + "\n");
EOF'

echo "==> Seeding Codex onboarding state..."
su -m -s /bin/bash bridge -c 'cd /app && export HOME=/home/bridge && node <<'\''EOF'\''
const fs = require("fs");
const path = require("path");

const codexDir = path.join(process.env.HOME, ".codex");
const authPath = path.join(codexDir, "auth.json");
const configPath = path.join(codexDir, "config.toml");

fs.mkdirSync(codexDir, { recursive: true });

const apiKey = process.env.OPENAI_API_KEY || "";
if (apiKey) {
  fs.writeFileSync(
    authPath,
    JSON.stringify(
      {
        auth_mode: "apikey",
        OPENAI_API_KEY: apiKey,
      },
      null,
      2,
    ) + "\n",
  );
}

const configToml = [
  "model = \"gpt-5.4\"",
  "",
  "[projects.\"/repos\"]",
  "trust_level = \"trusted\"",
  "",
  "[projects.\"/repos/penduin\"]",
  "trust_level = \"trusted\"",
  "",
  "[projects.\"/tmp/bridgectl\"]",
  "trust_level = \"trusted\"",
  "",
  "[notice.model_migrations]",
  "\"gpt-5.3-codex\" = \"gpt-5.4\"",
  "",
].join("\n");

fs.writeFileSync(configPath, configToml);
EOF'

echo "==> Starting bridge as non-root user..."
if [ "$#" -gt 0 ]; then
  exec_as_bridge "$@"
fi

BRIDGE_CMD=(bridgectl server start --config "$BRIDGE_CONFIG")
if [ -n "$STEP_CA_URL" ] && [ -n "$STEP_CA_ROOT" ]; then
  BRIDGE_CMD+=(--step-ca-url "$STEP_CA_URL" --step-ca-root "$STEP_CA_ROOT")
  if [ -n "$STEP_CA_PROVISIONER" ]; then
    BRIDGE_CMD+=(--step-ca-provisioner "$STEP_CA_PROVISIONER")
  fi
  # Pass the provisioner password file for non-interactive JWK cert requests
  # (Docker containers have no TTY for interactive password prompts).
  STEP_PASS_FILE="/run/bridge-certs/step-provisioner-password"
  if [ -f "$STEP_PASS_FILE" ]; then
    BRIDGE_CMD+=(--step-ca-provisioner-password-file "$STEP_PASS_FILE")
  fi
fi
exec_as_bridge "${BRIDGE_CMD[@]}"
