GO ?= go
BINDIR := bin
DISTDIR := dist
HIVE := $(BINDIR)/hive
PLUGIN_ACP := $(BINDIR)/hive-plugin-acp
PLUGIN_SLACK := $(BINDIR)/hive-plugin-slack
EXAMPLES := $(BINDIR)/example-client $(BINDIR)/plugin-echo

# VERSION is stamped into the binary as `hive version` reports it. The release
# workflow passes the tag without its leading v; a local run gets the default.
VERSION ?= 0.0.0-dev

.PHONY: all build examples test test-race vet fmt tidy run clean ci release dist

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

test-race:
	$(GO) test -race ./...

vet:
	$(GO) vet ./...

fmt:
	$(GO) fmt ./...

tidy:
	$(GO) mod tidy

run: build
	$(HIVE) serve

clean:
	rm -rf $(BINDIR) $(DISTDIR)

# Release builds every binary on the host it runs on.
#
# CGO cannot be cross-compiled practically, and the libSQL driver ships prebuilt
# native libraries for linux and darwin on amd64 and arm64 only. So a release is
# built on a native runner per target rather than cross-compiled from one host.
# See docs/adr/0001-storage-libsql.md.
release: build examples
	@echo "built for $$(go env GOOS)/$$(go env GOARCH) with CGO_ENABLED=$$(go env CGO_ENABLED)"

# dist packages this host's binaries as a release publishes them: one tarball
# per platform, holding hive and both plugins side by side, which is where the
# daemon looks for them.
#
# It is the same layout the install script expects, so `make dist` and a real
# release can be tested the same way. Run it on a native runner per target; see
# .github/workflows/release.yml.
dist:
	@mkdir -p $(DISTDIR)
	$(GO) build -ldflags "-X main.Version=$(VERSION)" -o $(HIVE) ./cmd/hive
	$(GO) build -o $(PLUGIN_ACP) ./cmd/hive-plugin-acp
	$(GO) build -o $(PLUGIN_SLACK) ./cmd/hive-plugin-slack
	tar -czf $(DISTDIR)/hive_$(VERSION)_$$($(GO) env GOOS)_$$($(GO) env GOARCH).tar.gz \
		-C $(BINDIR) hive hive-plugin-acp hive-plugin-slack
	@echo "$(DISTDIR)/hive_$(VERSION)_$$($(GO) env GOOS)_$$($(GO) env GOARCH).tar.gz"

ci: vet test build examples
