# Production Bridge on an ai-desktop

The minimum version for the first production Bridge acceptance is **v1.4.0**.
It includes authoritative interaction state, supported pending responses and
Codex structured approvals/cancellation, MAR-95 bounded activity and general
instructions, and MAR-106 upload batching/completeness reporting. Bridge login
and outbound control/reconciliation were already present in v1.3.0.

## Install or upgrade

For an existing ai-desktop with the Orchael apt repository configured:

```sh
sudo apt-get update
sudo apt-get install -y bridgectl=1.4.0
bridgectl --version
```

For a new Ubuntu noble/plucky machine, configure the signed apt repository
using the [installation guide](../getting-started/installation.md), then install
the same pinned package. Release assets also include
`bridgectl_1.4.0_amd64.deb` and `bridgectl_1.4.0_arm64.deb` at
`https://github.com/orchael/bridgectl/releases/tag/v1.4.0`. For example:

```sh
gh release download v1.4.0 --repo orchael/bridgectl --pattern "bridgectl_1.4.0_$(dpkg --print-architecture).deb"
sudo apt-get install -y "./bridgectl_1.4.0_$(dpkg --print-architecture).deb"
```

These commands require v1.4.0 to have finished publishing. Do not substitute a
development build or `latest` when verifying the release.

## First existing desktop

Run as the desktop user (normally `ubuntu`), before starting sessions:

```sh
bridgectl login https://bridge.orchael.com
systemctl --user restart bridgectl
bridgectl whoami
bridgectl doctor
bridgectl run --provider codex /workspace/bridge
```

Use the actual checkout path in the last command. Complete Google sign-in and
organization approval in the browser. A desktop already enrolled elsewhere must
use `bridgectl login https://bridge.orchael.com --force` to replace enrollment.
Login receives `wss://control.bridge.orchael.com/v1/control` from Bridge and saves
it; no endpoint editing is needed. Egress HTTPS/WSS on 443 must be available.
Bridge never needs an inbound desktop port or the desktop's provider credentials.

The ai-desktops user service explicitly reads
`~/.config/bridgectl/config.yaml`. Login discovers that existing file unless a
higher-priority state-directory `bridge.yaml` already exists. Before enrollment
on a previously customized desktop, confirm the service and login use the same
config; preserve existing provider/security settings when reconciling duplicate
configs. Do not print the credentials JSON. The standard unmodified ai-desktop
layout is covered by the production enrollment regression test.

Restart is necessary for a running daemon: control and telemetry configuration
are loaded at startup. This unit uses `KillMode=control-group`, so finish active
work before restarting. With no running daemon, simply log in and run the
session; no restart command is necessary. Existing provider authentication and
workspace permissions remain prerequisites and are supplied by ai-desktops.

## Telemetry

Login configures a separate HTTPS collector endpoint and `brc_` credential. It
defaults telemetry to enabled only if `enabled` was not explicitly set, and
preserves explicit standalone collectors and `enabled: false`. Capture/upload
and the outbound control client run independently; local execution continues
when either Bridge endpoint is unavailable. Payload storage credentials stay
in Bridge's API, never on the desktop.

Default event kinds are lifecycle/context and question/answer analytics.
For broader redacted observed content, add these options to the existing
`telemetry` block, preserving login's endpoint and credential-file fields:

```yaml
telemetry:
  enabled: true
  kinds: [all]
  include_redacted_text: true
```

Then restart the daemon before the acceptance session. This opt-in captures
available provider output and user input; it is not a promise that every
provider event is exposed or that disk/queue pressure cannot lose evidence.
The bounded spool, capture completeness and delivery status must be monitored.
MAR-106's broader lossless-capture acceptance remains open. Bridge writes each
accepted complete segment to its production S3 prefix before acknowledging it.
Do not confuse bounded live activity (control) with telemetry retention (S3).

## Acceptance and ai-desktops follow-up

Verify the session appears in Bridge, runtime and interaction states are
distinct, activity is bounded, supported input/approve/cancel actions work, and
a new instruction is executed once. Disconnect only Bridge access and confirm
local work continues; reconnect and verify state reconciliation and telemetry
delivery. Codex supplies the strongest current structured interaction support;
other provider capabilities must be checked individually.

Reviewed ai-desktops main currently pins `v1.1.1` in
`internal/provision/cloudinit.go` and `packer/variables.pkrvars.hcl`; its
preinstalled-image path rejects a version mismatch. That repository needs a
separate coordinated pin update and AMI rebuild for future desktops. Manually
upgrading one existing desktop is sufficient for initial acceptance. Bridge's
`orchael/bridge:profiles/developer` agent policy profile is optional and unrelated
to enrollment; it is not required for an existing desktop to connect.
