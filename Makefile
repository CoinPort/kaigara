APPS       := kaigara kaisave kaidump kaidel kaitail kaienv
GO         ?= go
OUT_DIR    ?= bin
VERSION    ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
GO_LDFLAGS := -w -s -X main.version=$(VERSION)

# Integration tests need Vault, MySQL and PostgreSQL. `make test-env-up`
# starts them; `make test` assumes they are reachable at these addresses.
export KAIGARA_VAULT_ADDR ?= http://127.0.0.1:8200
export KAIGARA_VAULT_TOKEN ?= changeme
export DATABASE_HOST ?= 127.0.0.1
export DATABASE_PORT ?= 3306

.PHONY: all build release clean test test-unit test-env-up test-env-down \
        fmt fmt-check vet lint vulncheck check help

all: build

## build: compile every binary for the host platform into bin/
build:
	@mkdir -p $(OUT_DIR)
	@for app in $(APPS); do \
		echo "  building $$app"; \
		CGO_ENABLED=0 $(GO) build -tags netgo -trimpath \
			-ldflags '$(GO_LDFLAGS)' -o $(OUT_DIR)/$$app ./cmd/$$app || exit 1; \
	done

## release: cross-compile every binary for every supported platform
release:
	@chmod +x ./scripts/build.sh
	@VERSION=$(VERSION) OUT_DIR=$(OUT_DIR) ./scripts/build.sh

## clean: remove build artifacts
clean:
	rm -rf $(OUT_DIR)/*

## test: run the whole suite (requires Vault, MySQL and PostgreSQL)
test:
	$(GO) test -race ./...

## test-unit: run only the packages that need no external services
test-unit:
	$(GO) test -race ./cmd/kaigara/ ./pkg/encryptor/aes/ ./pkg/encryptor/plaintext/ ./pkg/ika/

## test-env-up: start the Vault, MySQL and PostgreSQL the tests need
test-env-up:
	docker compose -f etc/backend.yml up -d
	@echo "waiting for services..."
	@sleep 15
	@docker compose -f etc/backend.yml exec -T vault \
		sh -c 'VAULT_ADDR=http://127.0.0.1:8200 VAULT_TOKEN=$(KAIGARA_VAULT_TOKEN) vault secrets enable transit' \
		2>/dev/null || echo "  transit already enabled"

## test-env-down: stop the test services and delete their volumes
test-env-down:
	docker compose -f etc/backend.yml down -v

## fmt: rewrite all sources with gofmt
fmt:
	gofmt -w $(shell find . -name '*.go' -not -path './vendor/*')

## fmt-check: fail if anything is not gofmt-clean
fmt-check:
	@unformatted=$$(gofmt -l $$(find . -name '*.go' -not -path './vendor/*')); \
	if [ -n "$$unformatted" ]; then \
		echo "not gofmt-clean:"; echo "$$unformatted"; exit 1; \
	fi

## vet: run go vet
vet:
	$(GO) vet ./...

## lint: run golangci-lint (install it if absent)
lint:
	@command -v golangci-lint >/dev/null 2>&1 || { \
		echo "golangci-lint not found; see https://golangci-lint.run/usage/install/"; exit 1; }
	golangci-lint run

## vulncheck: report known vulnerabilities reachable from this code
vulncheck:
	@command -v govulncheck >/dev/null 2>&1 || $(GO) install golang.org/x/vuln/cmd/govulncheck@latest
	govulncheck ./...

## check: everything CI runs bar the integration tests
check: fmt-check vet build test-unit

## help: list targets
help:
	@grep -hE '^## ' $(MAKEFILE_LIST) | sed 's/## //' | awk -F': ' '{printf "  \033[36m%-14s\033[0m %s\n", $$1, $$2}'
