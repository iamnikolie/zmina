.PHONY: build install uninstall test test-fast vet fmt fmt-check clean public-check

BIN := zmina
PREFIX ?= $(HOME)/.local
PKG := github.com/iamnikolie/zmina/cmd

# Version stamped into the binary. Falls back to the short commit when the tree
# has no tag yet, so a local build is still identifiable.
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X $(PKG).version=$(VERSION)

# Built next to the old binary and renamed over it: a role's hook or a tick that starts during the build runs the
# old zmina or the new one, never a half-written file.
build:
	go build -ldflags "$(LDFLAGS)" -o $(BIN).new .
	mv -f $(BIN).new $(BIN)

install: build
	mkdir -p $(PREFIX)/bin
	ln -sf $(CURDIR)/$(BIN) $(PREFIX)/bin/$(BIN)
	@echo "linked $(PREFIX)/bin/$(BIN) -> $(CURDIR)/$(BIN)"

uninstall:
	rm -f $(PREFIX)/bin/$(BIN)

# The gate before a push, as CI runs it: the race detector on, nothing taken from the test cache.
test: public-check fmt-check
	go test -race -count=1 ./...

# A quicker loop while working: cached results, no race detector.
test-fast:
	go test ./...

# CI fails on unformatted files; so does make test, before a push.
fmt-check:
	@out=$$(gofmt -l .); if [ -n "$$out" ]; then echo "gofmt needed (make fmt):"; echo "$$out"; exit 1; fi

# Unpushed commits must not carry this machine's crew names, node and ssh host names, live task ids, IPs or
# private paths into the public repository (tools/publiccheck).
public-check:
	go run ./tools/publiccheck

vet:
	go vet ./...

fmt:
	gofmt -l -w .

clean:
	rm -f $(BIN) $(BIN).new
	rm -rf dist
