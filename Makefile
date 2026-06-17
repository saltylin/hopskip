# hopskip — a single Go binary that embeds the Vue shell via embed.FS.
# The frontend is pre-built static assets under web/public, so there is no
# separate frontend build step: `go build` bakes them into the binary.

BINARY := hopskip
GO     ?= go
PKG    := ./...
PREFIX ?= /usr/local        # `make install` -> $(PREFIX)/bin ; override e.g. PREFIX=$HOME/.local

.DEFAULT_GOAL := help

.PHONY: help build run test vet fmt tidy check clean install uninstall

help: ## Show this help
	@grep -hE '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) | \
		awk 'BEGIN{FS=":.*?## "}{printf "  \033[36m%-10s\033[0m %s\n", $$1, $$2}'

build: ## Build the hopskip binary
	$(GO) build -o $(BINARY) .

run: build ## Build, then run (http://127.0.0.1:8765)
	./$(BINARY)

test: ## Run the Go test suite
	$(GO) test $(PKG)

vet: ## Run go vet
	$(GO) vet $(PKG)

fmt: ## Format the Go sources
	$(GO) fmt $(PKG)

tidy: ## Tidy go.mod / go.sum
	$(GO) mod tidy

check: vet test ## vet + test (pre-commit gate)

clean: ## Remove the built binary
	rm -f $(BINARY)

install: build ## Install the binary into $(PREFIX)/bin (may need sudo)
	@mkdir -p $(PREFIX)/bin
	install -m 0755 $(BINARY) $(PREFIX)/bin/$(BINARY)
	@echo "installed $(PREFIX)/bin/$(BINARY)"

uninstall: ## Remove the installed binary
	rm -f $(PREFIX)/bin/$(BINARY)
