# Detect operating system
ifeq ($(OS),Windows_NT)
    # Windows
    UNAME_S = Windows_NT
else
    # Unix-like systems (Linux, macOS, etc.)
    UNAME_S := $(shell uname -s)
endif

# Set variables based on OS
ifeq ($(UNAME_S),Windows_NT)
    OS_TYPE = Windows
    BINARY_NAME := gopilot.exe
    BINARY_RUN := .\gopilot.exe
    RM = -cmd /c del /q $(BINARY_NAME) $(BINARY_NAME:.exe=) 2>nul
    BUILD_TIME := $(shell powershell -Command "Get-Date -Format 'o'")
else ifeq ($(UNAME_S),Darwin)
    OS_TYPE = macOS
    BINARY_NAME := gopilot
    BINARY_RUN := ./gopilot
    RM = rm -f $(BINARY_NAME)
    BUILD_TIME := $(shell date -u +'%Y-%m-%dT%H:%M:%SZ')
else
    OS_TYPE = Linux
    BINARY_NAME := gopilot
    BINARY_RUN := ./gopilot
    RM = rm -f $(BINARY_NAME)
    BUILD_TIME := $(shell date -u +'%Y-%m-%dT%H:%M:%SZ')
endif

GO ?= go
MODULE := $(shell $(GO) list -m)
VERSION ?= 1.0.0
GIT_COMMIT := $(shell git rev-parse --short HEAD 2>/dev/null || echo "none")

LDFLAGS := -ldflags "-X '$(MODULE)/pkg/version.Version=$(VERSION)' -X '$(MODULE)/pkg/version.BuildTime=$(BUILD_TIME)' -X '$(MODULE)/pkg/version.GitCommit=$(GIT_COMMIT)'"

.PHONY: build clean run test info

build:
	$(GO) build $(LDFLAGS) -o $(BINARY_NAME) .

run: build
	$(BINARY_RUN)

test:
	$(GO) test ./...

clean:
	$(RM)

info:
	@echo "Detected OS: $(OS_TYPE)"
	@echo "Binary name: $(BINARY_NAME)"
	@echo "Go: $(GO)"
	@echo "Module: $(MODULE)"
	@echo "Version: $(VERSION)"
	@echo "Build time: $(BUILD_TIME)"
	@echo "Git commit: $(GIT_COMMIT)"
