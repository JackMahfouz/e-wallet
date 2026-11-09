MODULE  := github.com/JackMahfouz/e-wallet/libs/auth
PROTO   := proto/auth/v1/auth.proto
# Where the generated PHP classes land (Identity's app/ directory).
PHP_OUT ?= ../../services/identity/app

.PHONY: proto proto-go proto-php test

proto: proto-go proto-php

# Go messages + gRPC stubs into gen/ (committed so other Go services can import them).
proto-go:
	protoc -I proto \
		--go_out=. --go_opt=module=$(MODULE) \
		--go-grpc_out=. --go-grpc_opt=module=$(MODULE) \
		$(PROTO)

# PHP message classes for Identity. protoc writes App/... from the PHP namespace,
# but PSR-4 maps App\ to app/, so generate into a temp dir and copy under $(PHP_OUT).
proto-php:
	@tmp=$$(mktemp -d) && \
	protoc -I proto --php_out=$$tmp $(PROTO) && \
	mkdir -p $(PHP_OUT)/Grpc && cp -R $$tmp/App/Grpc/. $(PHP_OUT)/Grpc/; \
	status=$$?; rm -rf $$tmp; exit $$status

test:
	go vet ./...
	go test -race -count=1 ./...
