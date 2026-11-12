MODULE := github.com/JackMahfouz/e-wallet/services/wallet
PROTO  := proto/wallet/v1/wallet.proto

# A throwaway PostgreSQL for the integration tests (see test-db-up).
TEST_DB_URL ?= postgres://postgres:wallet@127.0.0.1:55433/wallet?sslmode=disable

.PHONY: proto test test-db-up test-db-down run-dev

# Go messages and gRPC stubs into gen/ (committed so other Go services can import them).
proto:
	protoc -I proto \
		--go_out=. --go_opt=module=$(MODULE) \
		--go-grpc_out=. --go-grpc_opt=module=$(MODULE) \
		$(PROTO)

# Needs a PostgreSQL: run `make test-db-up` first. Without WALLET_TEST_DATABASE_URL the database
# tests are skipped; here they are required, so a missing database fails the run.
test:
	go vet ./...
	WALLET_REQUIRE_TEST_DB=1 WALLET_TEST_DATABASE_URL='$(TEST_DB_URL)' go test -race -count=1 ./...

test-db-up:
	docker run -d --name wallet-test-pg -e POSTGRES_PASSWORD=wallet -e POSTGRES_DB=wallet \
		-p 127.0.0.1:55433:5432 --tmpfs /var/lib/postgresql/data \
		postgres:16-alpine -c fsync=off -c synchronous_commit=off

test-db-down:
	docker rm -f wallet-test-pg

# Local development: the static one-time code is 123456 and simulated funding is on. Neither is
# safe anywhere real. Needs the auth service, the ledger and a database (see docs/wallet-service.md).
run-dev:
	WALLET_DATABASE_URL='$(TEST_DB_URL)' \
	WALLET_OTP_MODE=static WALLET_OTP_STATIC_CODE=123456 \
	WALLET_FUNDING_MODE=simulated \
	go run ./cmd/server
