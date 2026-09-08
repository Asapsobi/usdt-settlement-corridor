.PHONY: build test test-integration migrate-up migrate-down lint

build:
	go build ./...

# Unit tests only. Integration-tagged files (build tag "integration") are
# excluded automatically -- they need a real Postgres instance.
test:
	go test ./...

# Requires DISPATCHER_TEST_DATABASE_URL pointing at a real, reachable
# Postgres 16 instance, e.g.:
#   export DISPATCHER_TEST_DATABASE_URL=postgres://dispatcher:dispatcher@localhost:5436/dispatcher_test
test-integration:
	go test -tags=integration ./...

# Requires DISPATCHER_DATABASE_URL. Uses our own cmd/migrate rather than
# goose's CLI binary -- see cmd/migrate's own doc comment.
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
