# norca-deployer
GO      ?= go
BINARY  := norca-deployer
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -ldflags "-X main.version=$(VERSION)"

.PHONY: all build build-all test test-race lint lint-new vuln fmt tidy-check clean help

all: fmt lint test build

## build: Build bin/norca-deployer for this machine
build:
	$(GO) build $(LDFLAGS) -o bin/$(BINARY) ./cmd/norca-deployer

## build-all: Cross-build for the platforms the deployer runs on
build-all:
	GOOS=linux   GOARCH=amd64 $(GO) build $(LDFLAGS) -o bin/$(BINARY)-linux-amd64 ./cmd/norca-deployer
	GOOS=linux   GOARCH=arm64 $(GO) build $(LDFLAGS) -o bin/$(BINARY)-linux-arm64 ./cmd/norca-deployer
	GOOS=darwin  GOARCH=arm64 $(GO) build $(LDFLAGS) -o bin/$(BINARY)-darwin-arm64 ./cmd/norca-deployer
	GOOS=windows GOARCH=amd64 $(GO) build $(LDFLAGS) -o bin/$(BINARY)-windows-amd64.exe ./cmd/norca-deployer

## test: Run the tests
test:
	$(GO) test -count=1 ./...

## test-race: Run the tests with the race detector (needs cgo; CI runs it on Linux)
test-race:
	$(GO) test -race -shuffle=on -count=1 ./...

## lint: Run golangci-lint over everything (the code base has findings from before the config)
lint:
	golangci-lint run ./...

## lint-new: Run golangci-lint on what this branch adds to main (what CI checks)
lint-new:
	golangci-lint run --new-from-rev=main ./...

## vuln: Scan the module and the standard library for known vulnerabilities
vuln:
	govulncheck ./...

## fmt: Format and vet (on Windows with autocrlf gofmt reports CRLF files; CI checks on Linux)
fmt:
	$(GO) fmt ./...
	$(GO) vet ./...

## tidy-check: Fail when go.mod or go.sum is not tidy
tidy-check:
	$(GO) mod tidy
	git diff --exit-code go.mod go.sum

## clean: Remove build output
clean:
	rm -rf bin/ coverage.out

## help: Show this help
help:
	@sed -n 's/^##//p' $(MAKEFILE_LIST) | column -t -s ':'
