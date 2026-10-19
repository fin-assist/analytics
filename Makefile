# Common commands; CI runs the same steps (.github/workflows/ci.yml).
GOLANGCI_LINT_VERSION := v2.15.0
BIN := bin/analytics
export GOPRIVATE := github.com/fin-assist/*

.PHONY: build test cover lint fmt run migrate image up down

build: ## build the binary into bin/
	go build -trimpath -o $(BIN) ./cmd/analytics

test: ## go.mod tidy, unit and integration tests (Docker or TEST_DATABASE_URL)
	go mod tidy -diff
	go vet ./...
	go test -race -count=1 ./...

cover: ## tests with a coverage profile, then coverage of the lines changed against origin/main
	go test -race -count=1 -coverpkg=./... -coverprofile=cover.out ./...
	python3 scripts/coverage-diff.py --base origin/main

lint: ## golangci-lint of the pinned version
	go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION) run ./...

fmt:
	go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION) fmt ./...

image: ## the service image
	docker build -t analytics:dev .

up: ## PostgreSQL, migrations and the service in Docker
	docker compose up --build

down:
	docker compose down

run: build ## the service on the host against the compose PostgreSQL
	SERVICE_NAME=analytics ENV=dev AUTH_MODE=insecure-dev LOG_FORMAT=text \
	DB_PRIMARY_DSN='postgres://analytics:analytics@localhost:5432/analytics?sslmode=disable' \
	$(BIN) serve

migrate: build ## migrate up against the compose PostgreSQL
	DB_PRIMARY_DSN='postgres://analytics:analytics@localhost:5432/analytics?sslmode=disable' $(BIN) migrate up
