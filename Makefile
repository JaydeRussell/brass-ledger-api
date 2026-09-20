.PHONY: help build run test test-integration latency latency-gate regress lint fmt tidy clean docker-up docker-up-d docker-down docker-down-v docker-build docker-reset logs logs-tail db-shell smoke

help: ## Show this help
	@grep -E '^[a-zA-Z_-]+:.*## ' $(MAKEFILE_LIST) | sort | awk 'BEGIN {FS = ":.*## "}; {printf "  %-10s %s\n", $$1, $$2}'

build: ## Compile the server binary to bin/server
	go build -o bin/server ./cmd/server

run: ## Run the server locally (reads .env via godotenv — see .env.example)
	go run ./cmd/server

test: ## Run the test suite
	go test ./...

test-integration: ## Run the Postgres-backed integration tests against the local docker-compose Postgres (must be up — see docker-up)
	DATABASE_URL="postgres://$${POSTGRES_USER:-brassledger}:$${POSTGRES_PASSWORD:-brassledger}@localhost:5432/$${POSTGRES_DB:-brass_ledger}?sslmode=disable" \
		go test -tags=integration ./internal/user/... ./internal/bcpcache/... ./internal/db/...

regress: ## Full local regression pass before a PR: build, vet, fmt, tests, then the latency map
	@$(MAKE) --no-print-directory build
	@$(MAKE) --no-print-directory lint
	@$(MAKE) --no-print-directory test
	@echo
	@echo "== latency =="
	@if curl -s -o /dev/null -m 3 http://localhost:8080/healthz; then \
		$(MAKE) --no-print-directory latency; \
		echo; \
		echo "Read the page table above. Anything over CRITICAL_MS is a bug by this repo's"; \
		echo "own rule (see CLAUDE.md) — and note local numbers are a floor, not a user"; \
		echo "experience, so a regression against the last run is the signal."; \
	else \
		echo "skipped — the local stack isn't up (make docker-up-d), so there was nothing to measure."; \
		echo "Run 'make latency' once it is, if this change touches request handling or caching."; \
	fi

lint: ## Vet the code and fail if anything isn't gofmt-formatted
	go vet ./...
	@unformatted="$$(gofmt -l .)"; \
	if [ -n "$$unformatted" ]; then \
		echo "The following files need gofmt:"; \
		echo "$$unformatted"; \
		exit 1; \
	fi

fmt: ## Reformat all Go source in place
	gofmt -w .

tidy: ## Sync go.mod/go.sum with actual imports (needs network)
	go mod tidy

clean: ## Remove build output
	rm -rf bin

docker-up: ## Build and start the whole stack (Postgres + backend + frontend)
	./run.sh

docker-up-d: ## Same as docker-up, but detached (runs in the background)
	./run.sh -d

docker-down: ## Stop the whole stack (keeps the Postgres data volume)
	./run.sh down

docker-down-v: ## Stop the whole stack and delete the Postgres data volume too
	./run.sh down -v

docker-build: ## Build every image in the stack without starting them
	docker compose build

docker-reset: ## Rebuild images from your latest code changes and restart the stack (detached)
	@mkdir -p logs ../brass-ledger-web/logs && chmod 777 logs ../brass-ledger-web/logs
	docker compose down
	docker compose up --build --force-recreate -d

db-shell: ## Open a psql shell against the local (docker compose) Postgres
	docker compose exec db psql -U $${POSTGRES_USER:-brassledger} -d $${POSTGRES_DB:-brass_ledger}

latency: ## Print a latency map of every endpoint (BASE=... SESSION=... RUNS=...)
	./scripts/latency-map.sh $${BASE:+--base $$BASE} $${SESSION:+--session $$SESSION} $${RUNS:+--runs $$RUNS}

latency-gate: ## Same, but exit non-zero if any page exceeds CRITICAL_MS (default 2000)
	./scripts/latency-map.sh --gate $${BASE:+--base $$BASE} $${SESSION:+--session $$SESSION} $${RUNS:+--runs $$RUNS}

smoke: ## Quick post-rebuild sanity check (healthz/readyz/auth/frontend) — no BCP calls
	./scripts/smoke-test.sh

logs: ## Show the backend container's logs so far
	docker compose logs backend

logs-tail: ## Follow the backend container's logs live (Ctrl-C to stop)
	docker compose logs -f backend
