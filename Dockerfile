# syntax=docker/dockerfile:1
# BUILD_FROM selects the binary source:
#   source   - build from Go source (default, for local docker build)
#   prebuilt - use binaries already compiled by GoReleaser
ARG BUILD_FROM=source
ARG INCLUDE_E2E_SCRIPTS=false

# Source build stage
FROM golang:1.25 AS source

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 go build -o /out/bridgectl ./cmd/bridgectl && \
    CGO_ENABLED=0 go build -o /out/bridge-ca ./cmd/bridge-ca

# Pre-built binaries stage (GoReleaser provides these in the build context)
FROM scratch AS prebuilt
COPY bridgectl bridge-ca /out/

# Select binary source — BuildKit skips whichever stage is not referenced
FROM ${BUILD_FROM} AS build

# e2e test helpers — conditional multi-stage selector.
# When INCLUDE_E2E_SCRIPTS=true, the e2e-scripts stage contains the helper;
# when false, the stage is empty so the final COPY is a no-op.
# When true, copy the e2e helper into a staging directory.
FROM busybox AS e2e-scripts-true
COPY e2e/scripts/opencode_repl.js /scripts/opencode_repl.js

# When false, create the same path as an empty directory so the COPY is a no-op.
FROM busybox AS e2e-scripts-false
RUN mkdir -p /scripts

FROM e2e-scripts-${INCLUDE_E2E_SCRIPTS} AS e2e-scripts

# Runtime stage
FROM ubuntu:24.04

WORKDIR /app

RUN apt-get update && \
    apt-get install -y --no-install-recommends bubblewrap ca-certificates curl && \
    curl -fsSL https://deb.nodesource.com/setup_24.x | bash - && \
    apt-get install -y --no-install-recommends nodejs && \
    apt-get clean && rm -rf /var/lib/apt/lists/*

# Install step CLI for Step CA integration (Tier 2 PKI).
# The binary is used by bridgectl to obtain certificates from a Step CA instance.
ARG STEP_VERSION=0.30.6
RUN ARCH=$(dpkg --print-architecture) && \
    curl -fsSL "https://github.com/smallstep/cli/releases/download/v${STEP_VERSION}/step-cli_${ARCH}.deb" -o /tmp/step-cli.deb && \
    dpkg -i /tmp/step-cli.deb && \
    rm /tmp/step-cli.deb

RUN useradd -m -s /bin/bash bridge && \
    mkdir -p /home/bridge/.gemini /home/bridge/.local/bin && \
    chown -R bridge:bridge /home/bridge/.gemini /home/bridge/.local/bin

# Install Antigravity CLI (agy) — replaces the retired @google/gemini-cli npm package.
# Auth uses OAuth2 credentials mounted at ~/.gemini/oauth_creds.json.
RUN curl -fsSL https://antigravity.google/cli/install.sh | HOME=/home/bridge bash && \
    chown bridge:bridge /home/bridge/.local/bin/agy

COPY --from=build /out/bridgectl /usr/local/bin/bridgectl
COPY --from=build /out/bridge-ca /usr/local/bin/bridge-ca
COPY .nvmrc /app/.nvmrc
RUN corepack enable && corepack prepare pnpm@11.22.0 --activate
COPY package.json pnpm-lock.yaml pnpm-workspace.yaml /app/
RUN pnpm install --frozen-lockfile --prod && pnpm store prune && \
    ln -sf /app/node_modules/.bin/codex /usr/local/bin/codex && \
    ln -sf /app/node_modules/.bin/claude /usr/local/bin/claude && \
    ln -sf /app/node_modules/.bin/opencode /usr/local/bin/opencode && \
    ln -sf /home/bridge/.local/bin/agy /usr/local/bin/agy
COPY config/bridge.yaml /app/config/bridge.yaml
COPY config/bridge-docker.yaml /app/config/bridge-docker.yaml
COPY config/bridge-docker-stepca.yaml /app/config/bridge-docker-stepca.yaml
COPY docker-entrypoint.sh /app/entrypoint.sh
RUN chmod +x /app/entrypoint.sh

# e2e test helpers — excluded from production images by default.
# Pass --build-arg INCLUDE_E2E_SCRIPTS=true to include them (e.g. in e2e compose).
# When false, the e2e-scripts stage is empty so this COPY is a no-op and adds no layer.
COPY --from=e2e-scripts /scripts/ /app/scripts/

EXPOSE 9445

ENTRYPOINT ["/app/entrypoint.sh"]
