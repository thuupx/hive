GO ?= go
BIN := bin/hive

.PHONY: all build test vet fmt tidy run clean ci

all: build

build:
	$(GO) build -o $(BIN) ./cmd/hive

test:
	$(GO) test ./...

vet:
	$(GO) vet ./...

fmt:
	$(GO) fmt ./...

tidy:
	$(GO) mod tidy

run: build
	$(BIN) serve

clean:
	rm -rf bin

ci: vet test build
