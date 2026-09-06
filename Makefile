# crypto-exchange — developer entry points. Run `make help`.
SHELL := /bin/bash
.DEFAULT_GOAL := help

MODULE        := github.com/arc119226/crypto-exchange
TOOLS_MOD     := tools/go.mod
COMPOSE_FILE  := deploy/compose/compose.yaml
ENV_FILE      ?= .env
COMPOSE       := docker compose -f $(COMPOSE_FILE) --env-file $(ENV_FILE)
OBS           ?= 1
OBS_PROFILE   := $(if $(filter 1,$(OBS)),--profile observability,)
VERSION       ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT        ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
DATE          ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS       := -s -w -X main.version=$(VERSION) -X main.commit=$(COMMIT) -X main.date=$(DATE)
IMAGE         ?= ghcr.io/arc119226/crypto-exchange:$(VERSION)
FUZZ_TIME     ?= 30s
GOTOOL        := go tool -modfile=$(TOOLS_MOD)
FOUNDRY_TAG   ?= $(shell sed -n 's/^FOUNDRY_TAG=//p' .env.example)
FOUNDRY_IMAGE := ghcr.io/foundry-rs/foundry:$(FOUNDRY_TAG)
ALL_PROFILES  := --profile infra --profile observability --profile app --profile single

.PHONY: help tools gen gen-check fmt tidy lint test test-fuzz test-integration e2e cover-money build image \
	    up up-single down reset infra-up run migrate seed artifacts compose-config contracts-test \
	    gen-dev-secrets demo trace loadgen

help: ## Show this help
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-18s\033[0m %s\n", $$1, $$2}'

tools: ## Show pinned tool versions (tools/go.mod)
	$(GOTOOL) oapi-codegen -version
	$(GOTOOL) sqlc version
	$(GOTOOL) golangci-lint version

GEN_DIRS := internal/api/gen internal/admin/gen cmd/exchangectl/internal/apiclient cmd/exchangectl/internal/adminclient \
            internal/registry/sqlcgen internal/ledger/sqlcgen internal/audit/sqlcgen \
            internal/trading/sqlcgen internal/eventbus/sqlcgen internal/auth/sqlcgen

gen: ## Regenerate OpenAPI server/client and sqlc code (outputs are committed)
	$(GOTOOL) oapi-codegen -config internal/api/gen/oapi-codegen.yaml api/public/v1/openapi.yaml
	$(GOTOOL) oapi-codegen -config cmd/exchangectl/internal/apiclient/oapi-codegen.yaml api/public/v1/openapi.yaml
	$(GOTOOL) oapi-codegen -config internal/admin/gen/oapi-codegen.yaml api/admin/v1/openapi.yaml
	$(GOTOOL) oapi-codegen -config cmd/exchangectl/internal/adminclient/oapi-codegen.yaml api/admin/v1/openapi.yaml
	$(GOTOOL) sqlc generate -f sqlc.yaml
	gofmt -w $(GEN_DIRS)

gen-check: gen ## Fail if generated code is out of date
	git diff --exit-code -- $(GEN_DIRS)
	@test -z "$$(git status --porcelain -- $(GEN_DIRS))" || (git status --porcelain -- $(GEN_DIRS); echo "untracked generated files"; exit 1)

fmt: ## gofmt + goimports via golangci-lint formatters (pinned in tools/go.mod)
	$(GOTOOL) golangci-lint fmt ./...

tidy: ## go mod tidy for main and tools modules
	go mod tidy
	cd tools && go mod tidy

lint: ## go vet + golangci-lint (pinned in tools/go.mod)
	go vet ./...
	$(GOTOOL) golangci-lint run ./...

test: ## Unit + property tests with the race detector
	go test -race -short -count=1 ./...

test-fuzz: ## Run every Fuzz* target for FUZZ_TIME each
	@for pkg in $$(go list ./...); do \
	  for f in $$(go test -list 'Fuzz.*' $$pkg | grep '^Fuzz'); do \
	    echo "fuzz $$pkg $$f"; go test -run '^$$' -fuzz "^$$f$$" -fuzztime $(FUZZ_TIME) $$pkg || exit 1; \
	  done; done

test-integration: ## Integration tests (testcontainers; needs Docker)
	go test -race -count=1 -p 1 -tags integration ./test/integration/... ./internal/...

cover-money: ## Enforce >= 95% coverage on internal/money
	scripts/covercheck.sh 95 ./internal/money

build: ## Build exchange and exchangectl into bin/
	CGO_ENABLED=0 go build -trimpath -ldflags '$(LDFLAGS)' -o bin/ ./cmd/exchange ./cmd/exchangectl

image: ## Build the container image
	docker build -f build/Dockerfile --build-arg VERSION=$(VERSION) --build-arg COMMIT=$(COMMIT) --build-arg DATE=$(DATE) -t $(IMAGE) .

up: ## Start infra + all roles as separate containers (+observability unless OBS=0)
	$(COMPOSE) --profile infra $(OBS_PROFILE) --profile app up -d --build --wait

up-single: ## Start infra + single all-in-one container (+observability unless OBS=0)
	$(COMPOSE) --profile infra $(OBS_PROFILE) --profile single up -d --build --wait

down: ## Stop everything (keeps volumes)
	$(COMPOSE) $(ALL_PROFILES) down

reset: ## Stop and delete all volumes (postgres, nats, anvil state, contract artifacts)
	$(COMPOSE) $(ALL_PROFILES) down -v --remove-orphans
	rm -rf deploy/compose/artifacts

infra-up: ## Start only postgres/redis/nats/anvil so roles can run with `make run`
	$(COMPOSE) --profile infra up -d --wait

run: ## Run one role on the host against infra-up (make run ROLE=api)
	@test -n "$(ROLE)" || (echo "usage: make run ROLE=api|engine|chain|signer|stream|admin|worker|all" && exit 2)
	set -a; . ./$(ENV_FILE); set +a; \
	export DATABASE_URL="postgres://ex_all:$${POSTGRES_PASSWORD}@localhost:5432/exchange?sslmode=disable" \
	       NATS_URL=nats://localhost:4222 REDIS_ADDR=localhost:6379 ETH_RPC_URL=http://localhost:8545 \
	       JWT_JWKS_URL=http://127.0.0.1:8080/.well-known/jwks.json \
	       EXCHANGE_ADMIN_API_KEY="$${ADMIN_API_KEY}"; \
	go run -ldflags '$(LDFLAGS)' ./cmd/exchange serve --role=$(ROLE)

migrate: ## Apply migrations to the local Postgres started by infra-up
	set -a; . ./$(ENV_FILE); set +a; \
	DATABASE_URL="postgres://ex_migrate:$${POSTGRES_PASSWORD}@localhost:5432/exchange?sslmode=disable" go run ./cmd/exchange migrate up

seed: artifacts ## Seed registry fixtures into the local Postgres
	set -a; . ./$(ENV_FILE); set +a; \
	DATABASE_URL="postgres://ex_admin:$${POSTGRES_PASSWORD}@localhost:5432/exchange?sslmode=disable" \
	go run ./cmd/exchange seed --fixtures deploy/compose/artifacts/addresses.json

artifacts: ## Copy addresses.json out of the compose artifacts volume (for make seed / cast on the host)
	mkdir -p deploy/compose/artifacts
	$(COMPOSE) --profile infra run --rm --no-deps --entrypoint cat contracts-deployer /artifacts/addresses.json > deploy/compose/artifacts/addresses.json
	@cat deploy/compose/artifacts/addresses.json

e2e: ## Multi-container end-to-end test (compose app profile + exchangectl e2e; needs Docker)
	bash scripts/e2e.sh

compose-config: ## Validate the compose file with every profile (no daemon needed)
	docker compose -f $(COMPOSE_FILE) --env-file .env.example $(ALL_PROFILES) config -q
	@echo "compose.yaml OK"

contracts-test: ## forge build + test inside the pinned foundry image (no local foundry needed)
	docker run --rm -v $(CURDIR)/infra/contracts:/contracts:ro --entrypoint sh $(FOUNDRY_IMAGE) \
	  -c 'cp -r /contracts /tmp/work && cd /tmp/work && forge build && forge test -vv'

gen-dev-secrets: ## Create .env and dev secrets (idempotent; FORCE=1 to regenerate)
	scripts/gen-dev-secrets.sh

demo: ## Run the Phase demo script
	go run ./cmd/exchangectl demo

trace: ## Grep all container logs for a correlation id (make trace ID=...)
	@test -n "$(ID)" || (echo "usage: make trace ID=<correlation_id>" && exit 2)
	$(COMPOSE) logs --no-color 2>/dev/null | grep -- "$(ID)"

loadgen: ## Run the load generator against a running stack
	go run ./cmd/exchangectl loadgen --market ETH-USDC --rate 1000 --duration 60s --accounts 100
