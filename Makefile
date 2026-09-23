GO ?= go
BINDIR := bin
HIVE := $(BINDIR)/hive
PLUGIN_ACP := $(BINDIR)/hive-plugin-acp
PLUGIN_SLACK := $(BINDIR)/hive-plugin-slack

.PHONY: all build test vet fmt tidy run clean ci

all: build

# The plugin binary ships next to hive: the daemon resolves it from the
# executable directory unless HIVE_PLUGIN_DIR overrides it.
build:
	$(GO) build -o $(HIVE) ./cmd/hive
	$(GO) build -o $(PLUGIN_ACP) ./cmd/hive-plugin-acp
	$(GO) build -o $(PLUGIN_SLACK) ./cmd/hive-plugin-slack

test:
	$(GO) test ./...

vet:
	$(GO) vet ./...

fmt:
	$(GO) fmt ./...

tidy:
	$(GO) mod tidy

run: build
	$(HIVE) serve

clean:
	rm -rf $(BINDIR)

ci: vet test build
