.PHONY: help build run test test-integration lint fmt tidy clean docker-up docker-up-d docker-down docker-down-v docker-build docker-reset logs logs-tail db-shell smoke

help: ## Show this help
	@grep -E '^[a-zA-Z_-]+:.*## ' $(MAKEFILE_LIST) | sort | awk 'BEGIN {FS = ":.*## "}; {printf "  %-10s %s\n", $$1, $$2}'

build: ## Compile the server binary to bin/server
	go build -o bin/server ./cmd/server

run: ## Run the server locally (reads .env via godotenv — see .env.example)
	go run ./cmd/server

test: ## Run the test suite
	go test ./...

test-integration: ## Run internal/user's integration tests against the local docker-compose Postgres (must be up — see docker-up)
	DATABASE_URL="postgres://$${POSTGRES_USER:-brassledger}:$${POSTGRES_PASSWORD:-brassledger}@localhost:5432/$${POSTGRES_DB:-brass_ledger}?sslmode=disable" \
		go test -tags=integration ./internal/user/...

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

smoke: ## Quick post-rebuild sanity check (healthz/readyz/auth/frontend) — no BCP calls
	./scripts/smoke-test.sh

logs: ## Show this service's log file so far (see LOG_FILE in .env.example)
	@f="$${LOG_FILE:-logs/backend.log}"; \
	if [ -f "$$f" ]; then cat "$$f"; else echo "No log file at $$f yet — has the server been started?"; fi

logs-tail: ## Follow this service's log file live (Ctrl-C to stop)
	@f="$${LOG_FILE:-logs/backend.log}"; \
	echo "Tailing $$f (Ctrl-C to stop)..."; \
	touch "$$f"; \
	tail -f "$$f"
