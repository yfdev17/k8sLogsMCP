BINARY  := bin/k8s-logs-mcp
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)

.PHONY: build test lint clean

build:
	go build -ldflags "-s -w -X main.version=$(VERSION)" -o $(BINARY) ./cmd/k8s-logs-mcp

test:
	go test -race ./...

lint:
	go vet ./...
	golangci-lint run ./...

clean:
	rm -rf bin
