.PHONY: build install uninstall test vet fmt clean public-check

BIN := zmina
PREFIX ?= $(HOME)/.local
PKG := github.com/iamnikolie/zmina/cmd

# Version stamped into the binary. Falls back to the short commit when the tree
# has no tag yet, so a local build is still identifiable.
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X $(PKG).version=$(VERSION)

build:
	go build -ldflags "$(LDFLAGS)" -o $(BIN) .

install: build
	mkdir -p $(PREFIX)/bin
	ln -sf $(CURDIR)/$(BIN) $(PREFIX)/bin/$(BIN)
	@echo "linked $(PREFIX)/bin/$(BIN) -> $(CURDIR)/$(BIN)"

uninstall:
	rm -f $(PREFIX)/bin/$(BIN)

test: public-check
	go test ./...

# Unpushed commits must not carry this machine's crew names, node and ssh host names, live task ids, IPs or
# private paths into the public repository (tools/publiccheck).
public-check:
	go run ./tools/publiccheck

vet:
	go vet ./...

fmt:
	gofmt -l -w .

clean:
	rm -f $(BIN)
	rm -rf dist
