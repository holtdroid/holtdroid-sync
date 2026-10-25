GO      ?= go
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)

.PHONY: all build check test lint fmt image clean

all: build

build:
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags '$(LDFLAGS)' -o bin/ ./cmd/...

test:
	$(GO) test -race ./...

lint:
	golangci-lint run ./...

fmt:
	golangci-lint fmt ./...

check: lint test build
	for job in slack s3 github webview chromium; do helm lint deploy/helm/holtdroid-sync --set job=$$job || exit 1; done

image:
	docker buildx build --platform linux/amd64 --build-arg VERSION=$(VERSION) -t holtdroid-sync:$(VERSION) --load .

clean:
	rm -rf bin
