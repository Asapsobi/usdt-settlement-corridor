.PHONY: build test test-integration migrate-up migrate-down lint

build:
	go build ./...

# Unit tests only. Integration-tagged files (build tag "integration") are
# excluded automatically -- they need a real Postgres instance.
test:
	go test ./...

# Requires WATCHER_TEST_DATABASE_URL pointing at a real, reachable
# Postgres 16 instance, e.g.:
#   export WATCHER_TEST_DATABASE_URL=postgres://watcher:watcher@localhost:5432/watcher_test
test-integration:
	go test -tags=integration ./...

# Requires WATCHER_DATABASE_URL. Uses our own cmd/migrate rather than
# goose's CLI binary, for the same reason as C1's: it would drag a driver
# for every database goose supports into go.sum unnecessarily.
migrate-up:
	go run ./cmd/migrate up

migrate-down:
	go run ./cmd/migrate down

# Falls back to gofmt + go vet when golangci-lint isn't installed, rather
# than hard-failing the target on a missing tool.
lint:
	@if command -v golangci-lint >/dev/null 2>&1; then \
		golangci-lint run ./...; \
	else \
		echo "golangci-lint not found, falling back to gofmt + go vet"; \
		test -z "$$(gofmt -l .)" || (gofmt -l . && exit 1); \
		go vet ./...; \
	fi
