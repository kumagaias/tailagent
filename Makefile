GO := mise exec go@1.26.3 -- go
export GOCACHE := $(CURDIR)/.cache/go-build
DATA_DIR ?= $(HOME)/.config/tailagent
PORT ?= 8787

.PHONY: build test run fmt

build:
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags="-s -w" -o bin/tailagent ./cmd/tailagent

test:
	$(GO) test ./...

run:
	$(GO) run ./cmd/tailagent -data-dir "$(DATA_DIR)" -port "$(PORT)" -open

fmt:
	$(GO) fmt ./...
