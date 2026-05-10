# container-init build
#
# Static binaries for linux/amd64 and linux/arm64. CGO disabled so the result
# is a single self-contained executable that runs as PID 1 in any rootfs.

PKG     := github.com/emrul/container-init
BIN_DIR := bin
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)
GOFLAGS := -trimpath
TAGS    := osusergo,netgo

.PHONY: all build build-amd64 build-arm64 test tidy clean

all: build

build: build-amd64 build-arm64

build-amd64:
	@mkdir -p $(BIN_DIR)
	GOOS=linux GOARCH=amd64 CGO_ENABLED=0 \
		go build $(GOFLAGS) -tags '$(TAGS)' -ldflags '$(LDFLAGS)' \
		-o $(BIN_DIR)/container-init.linux-amd64 ./cmd/container-init

build-arm64:
	@mkdir -p $(BIN_DIR)
	GOOS=linux GOARCH=arm64 CGO_ENABLED=0 \
		go build $(GOFLAGS) -tags '$(TAGS)' -ldflags '$(LDFLAGS)' \
		-o $(BIN_DIR)/container-init.linux-arm64 ./cmd/container-init

test:
	go test ./...

tidy:
	go mod tidy

clean:
	rm -rf $(BIN_DIR)
