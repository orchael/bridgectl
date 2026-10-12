---
title: Installation
---

## apt (Ubuntu)

Install `bridgectl` from the signed apt repository:

```bash
sudo install -d -m 0755 /etc/apt/keyrings
curl -fsSL https://orchael.github.io/bridgectl/apt/bridgectl-archive-keyring.asc \
  | sudo gpg --dearmor -o /etc/apt/keyrings/bridgectl.gpg
echo "deb [arch=$(dpkg --print-architecture) signed-by=/etc/apt/keyrings/bridgectl.gpg] \
  https://orchael.github.io/bridgectl/apt noble main" \
  | sudo tee /etc/apt/sources.list.d/bridgectl.list >/dev/null
sudo apt-get update
sudo apt-get install -y bridgectl
systemctl --user enable --now bridge.service
```

**Supported suites:** `noble` (24.04 LTS) and `plucky` (25.04). Replace `noble` with `plucky` if you are on Ubuntu 25.04.

Run `systemctl --user` as the login user who will run the server. The package
ships a user unit named `bridge.service`; it does not ship a system unit named
`bridgectl.service`. If your host supplies a custom user unit, such as an
ai-desktop's `bridgectl.service`, use that unit instead and do not enable both.

## Upgrade a running server

Installing a newer package replaces `/usr/bin/bridgectl`, but an already
running server keeps using the old executable until its owning user service
restarts. Check sessions before restarting because the restart ends them:

```bash
sudo apt-get update
sudo apt-get install -y bridgectl
bridgectl session list
bridgectl doctor
```

After active sessions finish, restart the unit your host already uses:

```bash
systemctl --user status bridge.service bridgectl.service
systemctl --user daemon-reload
```

For a standalone package installation:

```bash
systemctl --user restart bridge.service
bridgectl doctor
```

For an ai-desktop with its custom user unit:

```bash
systemctl --user restart bridgectl.service
bridgectl doctor
```

`bridgectl doctor` reports when the running server version differs from the
installed CLI. Do not enable `bridge.service` on a host that already uses a
custom bridge unit.

## GitHub Releases

Download pre-built binaries from [GitHub Releases](https://github.com/orchael/bridgectl/releases).

## Build from Source

See [Prepare a Local Machine](./local-machine.md) for full build instructions.

```bash
git clone https://github.com/orchael/bridgectl.git
cd bridgectl
make build-cli
```

The build writes `bin/bridgectl`.
