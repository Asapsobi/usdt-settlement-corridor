.PHONY: build test test-integration migrate-up migrate-down lint gate

build:
	go build ./...

# Unit tests only. Integration-tagged files (build tag "integration") are
# excluded automatically -- they need a real Postgres instance.
test:
	go test ./...

# Requires LEDGER_TEST_DATABASE_URL pointing at a real, reachable
# Postgres 16 instance, e.g.:
#   export LEDGER_TEST_DATABASE_URL=postgres://ledger:ledger@localhost:5432/ledger_test
test-integration:
	go test -tags=integration ./...

# Requires LEDGER_DATABASE_URL. Uses our own cmd/migrate rather than
# goose's CLI binary, which imports a driver for every database goose
# supports and would drag their modules into go.sum unnecessarily.
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

# The C1 ship gate. Provisions its own throwaway database (ledger_gate,
# derived from LEDGER_TEST_DATABASE_URL's connection details) rather than
# running against ledger_dev or ledger_test, because several FINAL
# ASSERTIONS (exact journal_entries count, whole-database
# VerifyBalances/TrialBalance) are only meaningful against a database this
# run had entirely to itself -- see internal/replay's package doc. The
# throwaway database is dropped whether the gate passes or fails, but left
# in place if provisioning itself fails, so it's inspectable.
#
# Override scale for a quick smoke run, e.g.:
#   make gate GATE_ARGS="-orders=200 -workers=4"
GATE_ARGS ?=
gate:
	@set -e; \
	if [ -z "$$LEDGER_TEST_DATABASE_URL" ]; then \
		echo "LEDGER_TEST_DATABASE_URL is not set (used to derive the throwaway gate database's connection details)" >&2; \
		exit 1; \
	fi; \
	MAINT_URL=$$(echo "$$LEDGER_TEST_DATABASE_URL" | sed 's#/ledger_test#/postgres#'); \
	GATE_URL=$$(echo "$$LEDGER_TEST_DATABASE_URL" | sed 's#/ledger_test#/ledger_gate#'); \
	echo "==> provisioning throwaway database ledger_gate"; \
	psql "$$MAINT_URL" -v ON_ERROR_STOP=1 -c "DROP DATABASE IF EXISTS ledger_gate WITH (FORCE)"; \
	psql "$$MAINT_URL" -v ON_ERROR_STOP=1 -c "CREATE DATABASE ledger_gate"; \
	echo "==> running migrations"; \
	LEDGER_DATABASE_URL="$$GATE_URL" go run ./cmd/migrate up; \
	echo "==> running replay harness"; \
	STATUS=0; \
	LEDGER_DATABASE_URL="$$GATE_URL" go run ./cmd/replay $(GATE_ARGS) || STATUS=$$?; \
	echo "==> dropping throwaway database ledger_gate"; \
	psql "$$MAINT_URL" -v ON_ERROR_STOP=1 -c "DROP DATABASE IF EXISTS ledger_gate WITH (FORCE)"; \
	exit $$STATUS
