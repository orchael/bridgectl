.PHONY: build proto tools test test-e2e test-e2e-unprotected test-step-ca-e2e test-cover test-cover-maintained lint clean certs dev-certs dev-setup agents-setup setup-hosts fmt smoke smoke-apt-local smoke-deb smoke-provider-runtime-user smoke-container smoke-ec2 up down logs up-local down-local logs-local up-step-ca down-step-ca logs-step-ca step-ca-health step-ca-issue-client chat-example chat-claude chat-opencode chat-codex chat-gemini chat-ca-example chat-ca-claude chat-ca-opencode chat-ca-codex chat-ca-gemini sessions-list sessions-watch sessions-attach orchestrator-claude orchestrator-opencode web-install web-dev web-build web-start docs-install docs-build docs-start build-cli test-cli-e2e test-cli-e2e-docker install-user-service check-deps setup-node

BIN_DIR := bin
BRIDGE_CA := $(BIN_DIR)/bridge-ca
BRIDGE_CLI := $(BIN_DIR)/bridgectl
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -ldflags "-X main.version=$(VERSION)"
CONFIG ?= config/bridge.yaml
DEV_CONFIG ?= config/bridge-dev.yaml
CHAT_TARGET ?= bridge.local:9445
CHAT_PROVIDER ?= claude
CHAT_PROJECT ?= dev
CHAT_REPO ?= /repos/penduin
CHAT_JWT_KEY ?= ../../certs/jwt-signing.key
build: proto
	@mkdir -p $(BIN_DIR)
	go build $(LDFLAGS) -o $(BRIDGE_CA) ./cmd/bridge-ca
	go build $(LDFLAGS) -o $(BRIDGE_CLI) ./cmd/bridgectl

build-cli:
	@mkdir -p $(BIN_DIR)
	go build $(LDFLAGS) -o $(BRIDGE_CLI) ./cmd/bridgectl

tools:
	go install google.golang.org/protobuf/cmd/protoc-gen-go@$(shell go list -m -f '{{.Version}}' google.golang.org/protobuf)
	go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@$(shell go list -m -f '{{.Version}}' google.golang.org/grpc/cmd/protoc-gen-go-grpc)

PROTOC_INCLUDE := $(shell brew --prefix 2>/dev/null)/include
proto:
	protoc \
		--proto_path=proto \
		--proto_path=$(PROTOC_INCLUDE) \
		--go_out=gen --go_opt=paths=source_relative \
		--go-grpc_out=gen --go-grpc_opt=paths=source_relative \
		bridge/v1/bridge.proto

test:
	./scripts/test-go.sh

E2E_ONLY ?=

test-e2e:
	@set +e; \
	E2E_ONLY=$(E2E_ONLY) ./scripts/with_env_secrets.sh docker compose -f e2e/docker-compose.yml up --build --abort-on-container-exit --exit-code-from test-client; \
	rc=$$?; \
	docker compose -f e2e/docker-compose.yml down -v; \
	exit $$rc

test-e2e-unprotected:
	@set +e; \
	E2E_ONLY=unprotected E2E_TEST_TIMEOUT=900s E2E_UNPROTECTED_EXPECT=protected ./scripts/with_env_secrets.sh docker compose -f e2e/docker-compose.yml up --build --abort-on-container-exit --exit-code-from test-client; \
	rc=$$?; \
	docker compose -f e2e/docker-compose.yml down -v; \
	if [ $$rc -ne 0 ]; then exit $$rc; fi; \
	E2E_ONLY=unprotected E2E_TEST_TIMEOUT=900s E2E_UNPROTECTED_EXPECT=enabled ./scripts/with_env_secrets.sh docker compose -f e2e/docker-compose.yml -f e2e/docker-compose.unprotected.yml up --build --abort-on-container-exit --exit-code-from test-client; \
	rc=$$?; \
	docker compose -f e2e/docker-compose.yml -f e2e/docker-compose.unprotected.yml down -v; \
	exit $$rc

test-step-ca-e2e:
	@set +e; \
	docker compose -f e2e/step-ca/docker-compose.yml up --build --abort-on-container-exit --exit-code-from test-client; \
	rc=$$?; \
	docker compose -f e2e/step-ca/docker-compose.yml down -v; \
	exit $$rc

test-remote-mtls:
	@set +e; \
	docker compose -f e2e/remote-mtls/docker-compose.yml up --build --abort-on-container-exit --exit-code-from remote-client; \
	rc=$$?; \
	echo ""; \
	echo "========================================"; \
	if [ $$rc -eq 0 ]; then \
		echo "  test-remote-mtls: PASSED"; \
	else \
		echo "  test-remote-mtls: FAILED (exit $$rc)"; \
	fi; \
	echo "========================================"; \
	docker compose -f e2e/remote-mtls/docker-compose.yml down -v; \
	exit $$rc

.PHONY: test-remote-stepca
test-remote-stepca:
	@set +e; \
	CF=e2e/remote-stepca/docker-compose.yml; \
	rc=0; \
	docker compose -f $$CF build && docker compose -f $$CF up -d || rc=$$?; \
	if [ $$rc -eq 0 ]; then \
		docker compose -f $$CF logs -f remote-client & \
		LOG_PID=$$!; \
		if CLIENT_ID=$$(docker compose -f $$CF ps -a -q remote-client) && [ -n "$$CLIENT_ID" ]; then \
			rc=$$(docker wait "$$CLIENT_ID") || rc=$$?; \
			case "$$rc" in ''|*[!0-9]*) rc=1 ;; esac; \
		else \
			echo "ERROR: could not find remote-client container" >&2; \
			rc=1; \
		fi; \
		kill $$LOG_PID 2>/dev/null; wait $$LOG_PID 2>/dev/null; \
	fi; \
	echo ""; \
	echo "========================================"; \
	if [ $$rc -eq 0 ]; then \
		echo "  test-remote-stepca: PASSED"; \
	else \
		echo "  test-remote-stepca: FAILED (exit $$rc)"; \
	fi; \
	echo "========================================"; \
	docker compose -f $$CF down -v; \
	exit $$rc

test-cover:
	./scripts/test-go-coverage.sh
	go tool cover -html=coverage.out -o coverage.html

test-cover-maintained:
	./scripts/check-go-coverage.sh

lint:
	./scripts/lint-go.sh

clean:
	rm -rf $(BIN_DIR) coverage.out coverage.html

certs: build
	$(BRIDGE_CA) init --name bridgectl --out certs/

dev-certs: build
	./scripts/dev_certs.sh

dev-setup: dev-certs agents-setup setup-hosts
	@echo "Dev environment ready. Certs in certs/"

setup-hosts:
	./scripts/setup_hosts.sh

agents-setup:
	./scripts/setup_ai_agents.sh

fmt:
	gofmt -s -w $(shell find . -name '*.go' -not -path './gen/*' -not -path './node_modules/*')
	goimports -w $(shell find . -name '*.go' -not -path './gen/*' -not -path './node_modules/*')

smoke:
	./scripts/with_env_secrets.sh ./scripts/smoke.sh

smoke-apt-local:
	./scripts/smoke-apt-local.sh

smoke-deb:
	@if [ -z "$$DEB" ]; then echo "Usage: make smoke-deb DEB=<path-to-.deb> SUITE=<noble|plucky>"; exit 1; fi
	./scripts/smoke-deb-docker.sh "$$DEB" "$${SUITE:-noble}"

smoke-provider-runtime-user:
	./scripts/smoke-provider-runtime-user.sh

smoke-container:
	@if [ -z "$$IMAGE" ]; then echo "Usage: make smoke-container IMAGE=<image>"; exit 1; fi
	./scripts/smoke-container.sh "$$IMAGE"

smoke-ec2:
	./scripts/with_env_secrets.sh ./scripts/smoke-ec2.sh

up:
	./scripts/with_env_secrets.sh docker compose up --build

down:
	docker compose down

logs:
	docker compose logs -f

up-local:
	docker compose -f docker-compose.yml -f docker-compose.local.yaml up --build --watch

down-local:
	docker compose -f docker-compose.yml -f docker-compose.local.yaml down

logs-local:
	docker compose -f docker-compose.yml -f docker-compose.local.yaml logs -f

STEP_CA_COMPOSE := docker compose -f step-ca/docker-compose.step-ca.yaml

up-step-ca:
	$(STEP_CA_COMPOSE) up --build

down-step-ca:
	$(STEP_CA_COMPOSE) down

logs-step-ca:
	$(STEP_CA_COMPOSE) logs -f

step-ca-health:
	$(STEP_CA_COMPOSE) exec step-ca step ca health --ca-url https://localhost:9443 --root /home/step/certs/root_ca.crt

STEP_CA_CLIENT_NAME ?= dev-client

step-ca-issue-client:
	$(STEP_CA_COMPOSE) exec bridge su -s /bin/bash bridge -c 'HOME=/home/bridge bridgectl server issue-client --name $(STEP_CA_CLIENT_NAME)'

chat-example:
	./scripts/with_env_secrets.sh go run ./examples/chat \
		--provider $(CHAT_PROVIDER) \
		--project $(CHAT_PROJECT) \
		--timeout 5m \
		$(CHAT_REPO)

chat-claude: CHAT_PROVIDER=claude
chat-claude: chat-example

chat-opencode: CHAT_PROVIDER=opencode
chat-opencode: chat-example

chat-codex: CHAT_PROVIDER=codex
chat-codex: chat-example

chat-gemini: CHAT_PROVIDER=gemini
chat-gemini: chat-example

# chat-ca: connects via Step CA-issued mTLS credentials auto-discovered from ~/.config/bridgectl/certs/
CHAT_CA_REMOTE ?= macbook.tail6198c2.ts.net

chat-ca-example:
	./scripts/with_env_secrets.sh go run ./examples/chat-ca \
		--remote $(CHAT_CA_REMOTE) \
		--provider $(CHAT_PROVIDER) \
		--project $(CHAT_PROJECT) \
		--timeout 5m \
		$(CHAT_REPO)

chat-ca-claude: CHAT_PROVIDER=claude
chat-ca-claude: chat-ca-example

chat-ca-opencode: CHAT_PROVIDER=opencode
chat-ca-opencode: chat-ca-example

chat-ca-codex: CHAT_PROVIDER=codex
chat-ca-codex: chat-ca-example

chat-ca-gemini: CHAT_PROVIDER=gemini
chat-ca-gemini: chat-ca-example

# orchestrator example: LLM-driven agent orchestration on a remote machine
ORCHESTRATOR_MACHINE  ?= macbook.tail6198c2.ts.net
ORCHESTRATOR_REPO     ?= /repos/penduin
ORCHESTRATOR_TASK     ?= "Run the test suite and fix any failing tests"
ORCHESTRATOR_MODEL    ?= gpt-5.6
ORCHESTRATOR_INTERVAL ?= 15s

orchestrator-claude:
	./scripts/with_env_secrets.sh go run ./examples/orchestrator \
		--machine $(ORCHESTRATOR_MACHINE) \
		--task $(ORCHESTRATOR_TASK) \
		--provider claude \
		--project $(CHAT_PROJECT) \
		--model $(ORCHESTRATOR_MODEL) \
		--interval $(ORCHESTRATOR_INTERVAL) \
		--timeout 30m \
		$(ORCHESTRATOR_REPO)

orchestrator-opencode:
	./scripts/with_env_secrets.sh go run ./examples/orchestrator \
		--machine $(ORCHESTRATOR_MACHINE) \
		--task $(ORCHESTRATOR_TASK) \
		--provider opencode \
		--project $(CHAT_PROJECT) \
		--model $(ORCHESTRATOR_MODEL) \
		--interval $(ORCHESTRATOR_INTERVAL) \
		--timeout 30m \
		$(ORCHESTRATOR_REPO)

# sessions example: list / watch / attach subcommands
SESSIONS_REMOTE ?=

sessions-list:
	go run ./examples/sessions list $(if $(SESSIONS_REMOTE),--remote $(SESSIONS_REMOTE))

SESSIONS_WATCH_ID ?=
sessions-watch:
	@test -n "$(SESSIONS_WATCH_ID)" || (echo "usage: make sessions-watch SESSIONS_WATCH_ID=<id>"; exit 1)
	go run ./examples/sessions watch $(if $(SESSIONS_REMOTE),--remote $(SESSIONS_REMOTE)) $(SESSIONS_WATCH_ID)

SESSIONS_ATTACH_ID ?=
sessions-attach:
	@test -n "$(SESSIONS_ATTACH_ID)" || (echo "usage: make sessions-attach SESSIONS_ATTACH_ID=<id>"; exit 1)
	go run ./examples/sessions attach $(if $(SESSIONS_REMOTE),--remote $(SESSIONS_REMOTE)) $(SESSIONS_ATTACH_ID)

# web example: Go server + Vite frontend
WEB_PORT     ?= 8080
WEB_VITE_PORT ?= 5173

web-install:
	cd examples/web/ui && pnpm install

web-dev: web-install
	cd examples/web && air -- --port $(WEB_PORT) --vite-port $(WEB_VITE_PORT) &\
	cd examples/web/ui && pnpm dev

web-build: web-install
	cd examples/web/ui && pnpm build
	cd examples/web/server && go build -o ../web .

web-start: web-build
	cd examples/web && ./web --port $(WEB_PORT) --vite-port 0

docs-install:
	cd docs && pnpm install

docs-build: docs-install
	cd docs && pnpm build

docs-start: docs-install
	cd docs && pnpm start

test-cli-e2e:
	@mkdir -p test-results
	@if command -v gotestsum >/dev/null 2>&1; then \
		gotestsum --format short-verbose --junitfile test-results/cli-e2e.xml \
			-- -count=1 -race -timeout 120s ./e2e/bridgectl/; \
	else \
		go test -v -count=1 -race -timeout 120s ./e2e/bridgectl/; \
	fi

test-cli-e2e-docker:
	./scripts/test-cli-e2e-docker.sh

install-user-service:
	@echo "Installing bridgectl user service..."
	@OS=$$(uname -s); \
	if [ "$$OS" = "Darwin" ]; then \
		mkdir -p ~/Library/LaunchAgents; \
		cp packaging/com.orchael.bridgectl.plist ~/Library/LaunchAgents/; \
		launchctl load ~/Library/LaunchAgents/com.orchael.bridgectl.plist 2>/dev/null || true; \
		echo "LaunchAgent installed. Run 'launchctl start com.orchael.bridgectl' to start now."; \
	elif [ "$$OS" = "Linux" ]; then \
		mkdir -p ~/.config/systemd/user; \
		cp packaging/bridge.user.service ~/.config/systemd/user/bridge.service; \
		systemctl --user daemon-reload; \
		echo "Run 'systemctl --user enable --now bridge' to start the service"; \
	else \
		echo "Unsupported OS: $$OS"; exit 1; \
	fi

check-deps:
	./scripts/check-deps.sh

setup-node:
	./scripts/setup-node.sh
