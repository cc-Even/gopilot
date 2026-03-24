BINARY_NAME := gopilot
GO ?= /usr/local/go/bin/go
MODULE := $(shell $(GO) list -m)
VERSION ?= 1.0.0
BUILD_TIME := $(shell date +%FT%T%z)
GIT_COMMIT := $(shell git rev-parse --short HEAD 2>/dev/null || echo "none")

LDFLAGS := -ldflags "-X '$(MODULE)/pkg/version.Version=$(VERSION)' \
                     -X '$(MODULE)/pkg/version.BuildTime=$(BUILD_TIME)' \
                     -X '$(MODULE)/pkg/version.GitCommit=$(GIT_COMMIT)'"

.PHONY: build clean run test

build:
	$(GO) build $(LDFLAGS) -o $(BINARY_NAME) .

run: build
	./$(BINARY_NAME)

test:
	$(GO) test ./...

clean:
	rm -f $(BINARY_NAME) $(BINARY_NAME).exe
