GO ?= go
GOFMT ?= gofmt
BINARY ?= tell

.PHONY: all build install format fmt-check tidy vet test race repeat modules diff-check check reproducible

all: build

build:
	CGO_ENABLED=0 $(GO) build -trimpath -buildvcs=false -ldflags="-buildid=" -o $(BINARY) ./cmd/tell

install:
	CGO_ENABLED=0 $(GO) install -trimpath -buildvcs=false -ldflags="-buildid=" ./cmd/tell

format:
	command -v $(GOFMT) >/dev/null
	find . -type f -name '*.go' -exec $(GOFMT) -w {} +

fmt-check:
	command -v $(GOFMT) >/dev/null
	test -z "$$(find . -type f -name '*.go' -exec $(GOFMT) -l {} +)"

tidy:
	$(GO) mod tidy

vet:
	$(GO) vet ./...

test:
	CGO_ENABLED=0 $(GO) test -count=1 ./...

race:
	$(GO) test -race -count=1 ./...

repeat:
	$(GO) test -count=3 ./...

modules:
	$(GO) list -m all

diff-check:
	git diff --check

check: fmt-check tidy vet test race repeat modules diff-check

reproducible:
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 $(GO) build -trimpath -buildvcs=false -ldflags="-buildid=" -o /tmp/tell-a ./cmd/tell
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 $(GO) build -trimpath -buildvcs=false -ldflags="-buildid=" -o /tmp/tell-b ./cmd/tell
	cmp /tmp/tell-a /tmp/tell-b
	/tmp/tell-a version
