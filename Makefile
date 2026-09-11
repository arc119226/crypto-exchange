# crypto-exchange — developer entry points. Run `make help`.
SHELL := /bin/bash
.DEFAULT_GOAL := help

MODULE        := github.com/arc119226/crypto-exchange
TOOLS_MOD     := tools/go.mod
COMPOSE_FILE  := deploy/compose/compose.yaml
ENV_FILE      ?= .env
COMPOSE       := docker compose -f $(COMPOSE_FILE) --env-file $(ENV_FILE)
SEPOLIA_FILE  := deploy/compose/compose.sepolia.yaml
COMPOSE_SEP   := docker compose -f $(COMPOSE_FILE) -f $(SEPOLIA_FILE) --env-file $(ENV_FILE)
PROD_FILE     := deploy/compose/compose.prod.yaml
ENV_PROD      ?= .env.prod
COMPOSE_PROD  := docker compose -f $(COMPOSE_FILE) -f $(SEPOLIA_FILE) -f $(PROD_FILE) --env-file $(ENV_PROD)
PROD_PROFILES := --profile infra --profile app --profile observability --profile backup
PROD_SERVICE  ?= exchange-api
OBS           ?= 1
SERVICE       ?= exchange-all
TAIL          ?= 100
OBS_PROFILE   := $(if $(filter 1,$(OBS)),--profile observability,)
VERSION       ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT        ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
DATE          ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS       := -s -w -X main.version=$(VERSION) -X main.commit=$(COMMIT) -X main.date=$(DATE)
IMAGE         ?= ghcr.io/arc119226/crypto-exchange:$(VERSION)
IMAGE_EDGE    ?= ghcr.io/arc119226/crypto-exchange-edge:$(VERSION)
IMAGE_BACKUP  ?= ghcr.io/arc119226/crypto-exchange-backup:$(VERSION)
FUZZ_TIME     ?= 30s
GOTOOL        := go tool -modfile=$(TOOLS_MOD)
FOUNDRY_TAG   ?= $(shell sed -n 's/^FOUNDRY_TAG=//p' .env.example)
FOUNDRY_IMAGE := ghcr.io/foundry-rs/foundry:$(FOUNDRY_TAG)
ALL_PROFILES  := --profile infra --profile observability --profile app --profile single --profile backup --profile web
BACKUP        ?= 0
BACKUP_PROFILE := $(if $(filter 1,$(BACKUP)),--profile backup,)
WEB           ?= 1
WEB_PROFILE   := $(if $(filter 1,$(WEB)),--profile web,)
# The container that carries exchangectl and the admin role in the running
# stack: the all-in-one one when `make up-single` is up, else the admin role.
STACK_SERVICE  = $(if $(shell $(COMPOSE) $(ALL_PROFILES) ps -q exchange-all 2>/dev/null),exchange-all,exchange-admin)
ACCOUNT       ?=
ASSET         ?= USDC
AMOUNT        ?= 10000
EMAIL         ?=
LATEST_MIGRATION := $(shell ls migrations | sed -n 's/^0*\([0-9]*\)_.*\.sql$$/\1/p' | sort -n | tail -1)
HELM          ?= helm
HELM_CHART    := deploy/helm/exchange
KIND          ?= kind
KIND_CLUSTER  ?= exchange
KIND_IMAGE    := crypto-exchange:ci

.PHONY: help tools gen gen-check fmt tidy tidy-check lint scripts-syntax check secrets-scan test docs-test test-fuzz test-integration e2e cover-money build image \
	    up up-single up-sepolia down down-sepolia logs logs-sepolia ps ps-sepolia reset infra-up run migrate seed artifacts compose-config contracts-test \
	    gen-dev-secrets faucet totp-enroll trace loadgen web-gen web-check web-build web-e2e \
	    helm-lint helm-template kind-up helm-e2e kind-down backup-drill \
	    image-edge image-backup images up-prod down-prod logs-prod ps-prod gen-prod-secrets release-check

help: ## Show this help
	@# The character class needs 0-9: without it this silently dropped e2e,
	@# helm-e2e and web-e2e -- the whole end-to-end path -- from the default
	@# goal, which is the only map a newcomer has.
	@grep -E '^[a-zA-Z0-9_-]+:.*?## .*$$' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-18s\033[0m %s\n", $$1, $$2}'

tools: ## Show pinned tool versions (tools/go.mod)
	$(GOTOOL) oapi-codegen -version
	$(GOTOOL) sqlc version
	$(GOTOOL) golangci-lint version
	@# gitleaks under `go tool` has no version stamped in, so read the pin.
	@printf 'gitleaks %s\n' "$$(sed -n 's|.*zricethezav/gitleaks/v8 \(v[0-9.]*\).*|\1|p' $(TOOLS_MOD) | head -1)"
	@printf 'kubeconform %s\n' "$$(sed -n 's|.*yannh/kubeconform \(v[0-9.]*\).*|\1|p' $(TOOLS_MOD) | head -1)"

GEN_DIRS := internal/api/gen internal/admin/gen cmd/exchangectl/internal/apiclient cmd/exchangectl/internal/adminclient \
            internal/registry/sqlcgen internal/ledger/sqlcgen internal/audit/sqlcgen \
            internal/trading/sqlcgen internal/eventbus/sqlcgen internal/auth/sqlcgen \
            internal/chain/sqlcgen internal/webhook/sqlcgen internal/marketdata/sqlcgen internal/admin/sqlcgen

# Editing api/public/v1/openapi.yaml? This target does not regenerate the front
# end's TypeScript types -- `web-gen` does, and it needs Node. `checks` runs both,
# so a stale web/trade/src/api/schema.d.ts fails CI rather than reaching anyone.
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

lint: ## go vet + golangci-lint + gitleaks (all pinned in tools/go.mod)
	go vet ./...
	$(GOTOOL) golangci-lint run ./...
	$(MAKE) --no-print-directory secrets-scan

tidy-check: ## Fail if go.mod / go.sum are not tidy (what CI asserts)
	$(MAKE) --no-print-directory tidy
	git diff --exit-code -- go.mod go.sum $(TOOLS_MOD) tools/go.sum

# Same list the checks job parses, for the same reason: this takes
# milliseconds, and a syntax error found twelve minutes into a container run is
# twelve minutes wasted.
scripts-syntax: ## bash -n over every shell script CI parses
	@for f in scripts/gen-dev-secrets.sh scripts/e2e.sh scripts/e2e-web.sh scripts/kind-secrets.sh \
	          scripts/helm-e2e.sh scripts/backup.sh scripts/restore-drill.sh scripts/gen-prod-secrets.sh \
	          deploy/vm/bootstrap.sh; do \
	  bash -n "$$f" || exit 1; \
	done
	@echo "scripts parse"

# One command for "am I ready to open a pull request". Before this existed the
# nearest thing was `make lint && make test`, which is two of the checks job's
# twelve steps, and the rest were written down only in docs/README.md, in
# Traditional Chinese.
#
# Two honest gaps, because a target that claims more than it does is worse than
# no target. It does NOT run the Docker-backed suites -- integration, e2e,
# helm-e2e, backup-drill -- which is where most of this project's evidence
# actually lives; run `make test-integration` and `make e2e` separately, and
# budget about twelve minutes. And deploy/helm/helm_test.go skips the chart
# lint and the kubeconform render when helm is not on PATH, while CI fails on
# it, so a green run here can still meet a red `checks`: set HELM or install
# helm to close that one.
check: lint tidy-check scripts-syntax gen-check docs-test test cover-money compose-config web-check contracts-test ## Everything a PR must pass that runs without the Docker suites
	@echo "check: all green"

# Scans git history, not the working tree. A --no-git scan walks everything on
# disk, which means a developer's own .env and secrets/jwt/ed25519.pem -- real
# keys, correctly gitignored, that can never reach the repo. Reporting those on
# every run is how a scanner teaches people to ignore it. What is in git is what
# CI enforces and what a push can leak, so that is what this scans.
#
# The whole history, not a range: no branch assumptions, nothing to get wrong,
# and go-re2 does 116 commits in under a second. If that stops being true, add
# a range here rather than reaching for --no-git.
secrets-scan: ## gitleaks over the git history (.gitleaks.toml)
	$(GOTOOL) gitleaks detect --source . --config .gitleaks.toml --redact --no-banner

test: ## Unit + property tests with the race detector
	go test -race -short -count=1 ./...

# The subset of `test` that reads the documentation rather than the code:
# links, backticked repo paths, the four-part runbook shape, the CLI
# subcommands and metric names runbooks tell an operator to use. Broken out
# because a documentation-only pull request skips ci.yml entirely, so
# .github/workflows/docs.yml runs this and nothing else.
docs-test: ## The test/docs suite alone (what a documentation-only PR runs)
	go test -count=1 ./test/docs/...

# This used to discover targets by running `go test -list 'Fuzz.*'` over every
# package, which reads as self-maintaining -- add a Fuzz* anywhere and CI picks
# it up -- and measured at four billable minutes to find the one that exists.
# Listing tests links a test binary per package, so the cost was 43 links to
# learn what one grep answers.
#
# internal/matching's FuzzApply is the only Fuzz* target in the repository.
# Naming it here means a new target added elsewhere would be skipped silently,
# so the checks job in .github/workflows/ci.yml greps for `func Fuzz` and fails
# if this is no longer the whole list. Forgetting is a red check, not silence.
test-fuzz: ## Run the Fuzz* targets in internal/matching for FUZZ_TIME each
	go test -run '^$$' -fuzz 'Fuzz' -fuzztime $(FUZZ_TIME) ./internal/matching/

# ./internal/... used to be listed here too. It ran nothing the unit job had
# not already run: no package outside ./test/integration carries the integration
# build tag, so the tag added no files to it (152 tests listed either way), and
# the only place -short changes anything is internal/matching, where rapid
# divides its check count by five -- which the unit job already re-runs at full
# strength with `-run TestProperty`. What it did cost was -p 1: twelve packages
# forced through one at a time, measured at 3.3x the two-way-parallel time on
# the two cores a runner has.
#
# -p 1 stays. With one package it is a no-op today; it is here so that a second
# integration package added later does not race this one for Docker.
test-integration: ## Integration tests (testcontainers; needs Docker)
	go test -race -count=1 -p 1 -tags integration ./test/integration/...

cover-money: ## Enforce >= 95% coverage on internal/money
	scripts/covercheck.sh 95 ./internal/money

build: ## Build exchange and exchangectl into bin/
	CGO_ENABLED=0 go build -trimpath -ldflags '$(LDFLAGS)' -o bin/ ./cmd/exchange ./cmd/exchangectl

image: ## Build the container image
	docker build -f build/Dockerfile --build-arg VERSION=$(VERSION) --build-arg COMMIT=$(COMMIT) --build-arg DATE=$(DATE) -t $(IMAGE) .

image-edge: ## Build the edge image (Caddy + web/trade; deploy/compose/compose.prod.yaml)
	docker build -f build/edge/Dockerfile -t $(IMAGE_EDGE) .

image-backup: ## Build the backup sidecar image (postgres client tools + mc)
	docker build -f build/backup/Dockerfile -t $(IMAGE_BACKUP) .

images: image image-edge image-backup ## Build all three images

up: ## Start infra + all roles as separate containers (+observability unless OBS=0, +the front end on :8088 unless WEB=0)
	$(COMPOSE) --profile infra $(OBS_PROFILE) $(WEB_PROFILE) --profile app up -d --build --wait

up-single: ## Start infra + single all-in-one container (+observability unless OBS=0, +the front end on :8088 unless WEB=0)
	$(COMPOSE) --profile infra $(OBS_PROFILE) $(WEB_PROFILE) --profile single up -d --build --wait

up-sepolia: ## Start the all-in-one container against Sepolia (see docs/guides/sepolia.md)
	@test -f deploy/compose/sepolia/sepolia-addresses.json || 	  (echo "missing deploy/compose/sepolia/sepolia-addresses.json — see deploy/compose/sepolia/README.md" && exit 2)
	@test -f deploy/compose/sepolia/seed-params.json || 	  (echo "missing deploy/compose/sepolia/seed-params.json — see deploy/compose/sepolia/README.md" && exit 2)
	$(COMPOSE_SEP) --profile infra $(OBS_PROFILE) --profile single up -d --build --wait

down-sepolia: ## Stop the Sepolia stack (keeps volumes; a separate project from the anvil one)
	$(COMPOSE_SEP) $(ALL_PROFILES) down

# Both of these exist because the profiles are not optional: naming a service
# on the command line activates that service but not the ones it depends on,
# so `logs exchange-all` without --profile infra fails with "no such service:
# nats", and a bare `ps` resolves to an empty model and reports nothing running
# while the stack is up. Neither failure points at the missing flag.
logs-sepolia: ## Read the Sepolia stack's logs (SERVICE=exchange-all TAIL=100; FOLLOW=1 to keep watching)
	$(COMPOSE_SEP) $(ALL_PROFILES) logs --tail $(TAIL) $(if $(FOLLOW),-f,) $(SERVICE)

ps-sepolia: ## Show what is running in the Sepolia stack
	$(COMPOSE_SEP) $(ALL_PROFILES) ps

# --- the beta VM (deploy/compose/compose.prod.yaml over the Sepolia overlay; docs/runbooks/beta-deploy.md)
up-prod: ## Pull the released images and start the beta stack (needs .env.prod and secrets/prod/)
	@test -f $(ENV_PROD) || (echo "missing $(ENV_PROD): run sudo scripts/gen-prod-secrets.sh, then fill it in" && exit 2)
	@test -d secrets/prod || (echo "missing secrets/prod/: run sudo scripts/gen-prod-secrets.sh" && exit 2)
	@test -f deploy/compose/sepolia/sepolia-addresses.json || (echo "missing deploy/compose/sepolia/sepolia-addresses.json — see docs/guides/sepolia.md" && exit 2)
	$(COMPOSE_PROD) $(PROD_PROFILES) pull --quiet
	$(COMPOSE_PROD) $(PROD_PROFILES) up -d --wait

down-prod: ## Stop the beta stack (keeps volumes)
	$(COMPOSE_PROD) $(PROD_PROFILES) --profile backup-local down

logs-prod: ## Read the beta stack's logs (PROD_SERVICE=exchange-api TAIL=100; FOLLOW=1 to keep watching; PROD_SERVICE= for all)
	$(COMPOSE_PROD) $(PROD_PROFILES) --profile backup-local logs --tail $(TAIL) $(if $(FOLLOW),-f,) $(PROD_SERVICE)

ps-prod: ## Show what is running in the beta stack
	$(COMPOSE_PROD) $(PROD_PROFILES) --profile backup-local ps

gen-prod-secrets: ## Create .env.prod and secrets/prod/ (run with sudo; idempotent, FORCE=1 to regenerate)
	scripts/gen-prod-secrets.sh

# See logs-sepolia for why every profile is named here.
logs: ## Read the dev stack's logs (SERVICE=exchange-all TAIL=100; FOLLOW=1 to keep watching)
	$(COMPOSE) $(ALL_PROFILES) logs --tail $(TAIL) $(if $(FOLLOW),-f,) $(SERVICE)

ps: ## Show what is running in the dev stack
	$(COMPOSE) $(ALL_PROFILES) ps

faucet: ## Credit a dev account from inside the stack (make faucet ACCOUNT=<id> ASSET=USDC AMOUNT=10000); the admin key stays in the container
	@test -n "$(ACCOUNT)" || (echo "usage: make faucet ACCOUNT=<account id from the wallet page> [ASSET=USDC] [AMOUNT=10000]" && exit 2)
	$(COMPOSE) $(ALL_PROFILES) exec -e EXCHANGE_ADMIN_URL=http://127.0.0.1:8082 -e EXCHANGE_ADMIN_API_KEY="$$(sed -n 's/^ADMIN_API_KEY=//p' $(ENV_FILE))" \
	  $(STACK_SERVICE) /exchangectl admin fund --account "$(ACCOUNT)" --asset "$(ASSET)" --amount "$(AMOUNT)" --reason "dev faucet"

totp-enroll: ## Issue the back-office authenticator secret for an administrator (make totp-enroll EMAIL=admin@example.com); printed once
	@test -n "$(EMAIL)" || (echo "usage: make totp-enroll EMAIL=<administrator email>" && exit 2)
	$(COMPOSE) $(ALL_PROFILES) exec $(STACK_SERVICE) /exchange admin totp enroll --email "$(EMAIL)"

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
	       JWT_JWKS_URL=http://127.0.0.1:8080/.well-known/jwks.json; \
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

backup-drill: ## Take a backup into the compose MinIO and restore it into a throwaway database, printing the RTO (needs a running stack; docs/runbooks/backup-restore.md)
	$(COMPOSE) --profile infra --profile backup build backup
	$(COMPOSE) --profile infra --profile backup up -d --wait minio
	$(COMPOSE) --profile infra --profile backup run --rm backup once
	$(COMPOSE) --profile infra --profile backup run --rm --entrypoint /usr/local/bin/restore-drill.sh \
	    -e BACKUP_STORE=s3 -e EXPECTED_MIGRATION=$(LATEST_MIGRATION) \
	    -e DRILL_ADMIN_URL="postgres://exchange:$$(sed -n 's/^POSTGRES_PASSWORD=//p' $(ENV_FILE))@postgres:5432/postgres?sslmode=disable" \
	    -e SOURCE_DATABASE_URL="postgres://ex_backup:$$(sed -n 's/^POSTGRES_PASSWORD=//p' $(ENV_FILE))@postgres:5432/exchange?sslmode=disable" \
	    backup

compose-config: ## Validate both compose files with every profile (no daemon needed)
	docker compose -f $(COMPOSE_FILE) --env-file .env.example $(ALL_PROFILES) config -q
	@echo "compose.yaml OK"
	ETH_RPC_URL=https://example.invalid ETH_SCAN_START_BLOCK=1 \
	  docker compose -f $(COMPOSE_FILE) -f $(SEPOLIA_FILE) --env-file .env.example $(ALL_PROFILES) config -q
	@echo "compose.sepolia.yaml OK"
	ETH_RPC_URL=https://example.invalid ETH_SCAN_START_BLOCK=1 BACKUP_S3_ENDPOINT=https://example.invalid \
	  docker compose -f $(COMPOSE_FILE) -f $(SEPOLIA_FILE) -f $(PROD_FILE) --env-file .env.prod.example $(ALL_PROFILES) --profile backup-local config -q
	@echo "compose.prod.yaml OK"

# --- Helm chart (deploy/helm/exchange; verified in CI on kind, docs/plan-v1.0.md §12 Phase 7)
helm-lint: ## helm lint + render the chart with the default and the kind values through kubeconform (no cluster needed)
	$(HELM) lint $(HELM_CHART)
	$(HELM) template exchange $(HELM_CHART) | $(GOTOOL) kubeconform -strict -summary -ignore-missing-schemas -kubernetes-version 1.31.0
	$(HELM) template exchange $(HELM_CHART) -f $(HELM_CHART)/values-kind.yaml | $(GOTOOL) kubeconform -strict -summary -ignore-missing-schemas -kubernetes-version 1.31.0

helm-template: ## Print the rendered manifests (VALUES=path to add a values file)
	$(HELM) template exchange $(HELM_CHART) $(if $(VALUES),-f $(VALUES),)

kind-up: image ## Create the kind cluster, load the image and create the chart's secrets (needs kind + kubectl + docker)
	$(KIND) get clusters 2>/dev/null | grep -qx $(KIND_CLUSTER) || $(KIND) create cluster --name $(KIND_CLUSTER)
	docker tag $(IMAGE) $(KIND_IMAGE)
	$(KIND) load docker-image $(KIND_IMAGE) --name $(KIND_CLUSTER)
	scripts/kind-secrets.sh exchange

helm-e2e: ## Install the chart into the current cluster and run the in-cluster e2e (after kind-up; KEEP=1 keeps the release)
	bash scripts/helm-e2e.sh

kind-down: ## Delete the kind cluster
	$(KIND) delete cluster --name $(KIND_CLUSTER)

release-check: ## Dry run of the release assertions for TAG=vX.Y.Z: chart packages with that appVersion and the binary reports it (docs/release.md; no Docker)
	@test -n "$(TAG)" || (echo "usage: make release-check TAG=vX.Y.Z" && exit 2)
	@case "$(TAG)" in v[0-9]*.[0-9]*.[0-9]*) ;; *) echo "TAG must look like vX.Y.Z"; exit 2;; esac
	@test -z "$$(git status --porcelain)" || (echo "the working tree is not clean; a release is a commit on main" && exit 2)
	$(HELM) lint $(HELM_CHART) >/dev/null
	rm -rf dist/release && mkdir -p dist/release
	$(HELM) package $(HELM_CHART) --version $(patsubst v%,%,$(TAG)) --app-version $(TAG) --destination dist/release >/dev/null
	@test "$$($(HELM) show chart dist/release/exchange-$(patsubst v%,%,$(TAG)).tgz | sed -n 's/^appVersion: *//p')" = "$(TAG)" || (echo "chart appVersion does not equal $(TAG)" && exit 1)
	go build -trimpath -ldflags "-s -w -X main.version=$(TAG) -X main.commit=$(COMMIT) -X main.date=$(DATE)" -o dist/release/exchange ./cmd/exchange
	@test "$$(dist/release/exchange version --json | jq -r .version)" = "$(TAG)" || (echo "the binary does not report $(TAG)" && exit 1)
	@echo "release-check $(TAG) OK: dist/release/exchange-$(patsubst v%,%,$(TAG)).tgz reports $(TAG), and so does the binary"

contracts-test: ## forge build + test inside the pinned foundry image (no local foundry needed)
	docker run --rm -v $(CURDIR)/infra/contracts:/contracts:ro --entrypoint sh $(FOUNDRY_IMAGE) \
	  -c 'cp -r /contracts /tmp/work && cd /tmp/work && forge build && forge test -vv'

gen-dev-secrets: ## Create .env and dev secrets (idempotent; FORCE=1 to regenerate)
	scripts/gen-dev-secrets.sh

screenshots: ## Screenshot every back-office page into docs/screenshots (needs a running admin role; ADMIN_URL ADMIN_EMAIL ADMIN_PASSWORD ADMIN_TOTP_SECRET)
	NODE_PATH=$$(npm root -g) node scripts/screenshots.mjs

trace: ## Grep all container logs for a correlation id (make trace ID=...)
	@test -n "$(ID)" || (echo "usage: make trace ID=<correlation_id>" && exit 2)
	$(COMPOSE) logs --no-color 2>/dev/null | grep -- "$(ID)"

# --- web/trade (the reference front end; Node 22, docs/plan-v1.0.md §12 Phase 6)
WEB_DIR := web/trade

web-gen: ## Regenerate web/trade/src/api/schema.d.ts from the public OpenAPI
	cd $(WEB_DIR) && npm ci --no-fund --no-audit && npm run gen

web-check: ## Front-end fast checks: schema.d.ts fresh, tsc, vite build
	cd $(WEB_DIR) && npm ci --no-fund --no-audit && npm run gen && git diff --exit-code -- src/api/schema.d.ts && npm run build

web-build: ## Build web/trade/dist
	cd $(WEB_DIR) && npm ci --no-fund --no-audit && npm run build

web-e2e: ## Playwright smoke against a running stack (API_URL / WS_URL / ADMIN_URL / ADMIN_API_KEY; after `KEEP=1 make e2e` in CI)
	bash scripts/e2e-web.sh

loadgen: ## Run the load generator against a running stack (docs/loadtest.md)
	go run ./cmd/exchangectl loadgen --market ETH-USDC --rate 1000 --duration 60s --accounts 100 \
	    --ws-clients 20 --private-clients 10 --idle-connections 1000
