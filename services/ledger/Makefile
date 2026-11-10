MODULE := github.com/JackMahfouz/e-wallet/services/ledger
PROTO  := proto/ledger/v1/ledger.proto

# A throwaway PostgreSQL for the integration tests (see test-db-up).
TEST_DB_URL ?= postgres://postgres:ledger@127.0.0.1:55432/ledger?sslmode=disable

.PHONY: proto test test-db-up test-db-down

# Go messages and gRPC stubs into gen/ (committed so other Go services can import them).
proto:
	protoc -I proto \
		--go_out=. --go_opt=module=$(MODULE) \
		--go-grpc_out=. --go-grpc_opt=module=$(MODULE) \
		$(PROTO)

# Needs a PostgreSQL: run `make test-db-up` first. Without LEDGER_TEST_DATABASE_URL the database
# tests are skipped; here they are required, so a missing database fails the run.
test:
	go vet ./...
	LEDGER_REQUIRE_TEST_DB=1 LEDGER_TEST_DATABASE_URL='$(TEST_DB_URL)' go test -race -count=1 ./...

test-db-up:
	docker run -d --name ledger-test-pg -e POSTGRES_PASSWORD=ledger -e POSTGRES_DB=ledger \
		-p 127.0.0.1:55432:5432 --tmpfs /var/lib/postgresql/data \
		postgres:16-alpine -c fsync=off -c synchronous_commit=off

test-db-down:
	docker rm -f ledger-test-pg
