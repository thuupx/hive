GO ?= go
BINDIR := bin
HIVE := $(BINDIR)/hive
PLUGIN_ACP := $(BINDIR)/hive-plugin-acp
PLUGIN_SLACK := $(BINDIR)/hive-plugin-slack
EXAMPLES := $(BINDIR)/example-client $(BINDIR)/plugin-echo

.PHONY: all build examples test vet fmt tidy run clean ci release

all: build

# The plugin binary ships next to hive: the daemon resolves it from the
# executable directory unless HIVE_PLUGIN_DIR overrides it.
build:
	$(GO) build -o $(HIVE) ./cmd/hive
	$(GO) build -o $(PLUGIN_ACP) ./cmd/hive-plugin-acp
	$(GO) build -o $(PLUGIN_SLACK) ./cmd/hive-plugin-slack

examples:
	$(GO) build -o $(BINDIR)/example-client ./examples/client
	$(GO) build -o $(BINDIR)/plugin-echo ./examples/plugin-echo

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

# Release builds every binary on the host it runs on.
#
# CGO cannot be cross-compiled practically, and the libSQL driver ships prebuilt
# native libraries for linux and darwin on amd64 and arm64 only. So a release is
# built on a native runner per target rather than cross-compiled from one host.
# See docs/adr/0001-storage-libsql.md.
release: build examples
	@echo "built for $$(go env GOOS)/$$(go env GOARCH) with CGO_ENABLED=$$(go env CGO_ENABLED)"

ci: vet test build examples
