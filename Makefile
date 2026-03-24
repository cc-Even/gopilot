BINARY_NAME=gopilot
VERSION=1.0.0
BUILD_TIME=$(shell date +%FT%T%z)
GIT_COMMIT=$(shell git rev-parse --short HEAD 2>/dev/null || echo "none")

LDFLAGS=-ldflags "-X 'gopilot/pkg/version.Version=${VERSION}' \
                  -X 'gopilot/pkg/version.BuildTime=${BUILD_TIME}' \
                  -X 'gopilot/pkg/version.GitCommit=${GIT_COMMIT}'"

.PHONY: build clean run

build:
	go build ${LDFLAGS} -o ${BINARY_NAME} main.go

run: build
	./${BINARY_NAME}

clean:
	rm -f ${BINARY_NAME}