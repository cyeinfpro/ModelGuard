VERSION := $(shell cat VERSION)
SIGNING_KEY ?=

.PHONY: test build package verify-package

test:
	go test -race ./...
	go vet ./...
	npm test

build:
	mkdir -p build
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags='-s -w' -o build/guard-linux-amd64 ./cmd/guard
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -ldflags='-s -w' -o build/guard-linux-arm64 ./cmd/guard
	go build -trimpath -o build/guard-native ./cmd/guard
	go run ./cmd/verify

package: build
	@test -n "$(SIGNING_KEY)" || (echo 'Set SIGNING_KEY to an external Ed25519 seed file'; exit 1)
	go run ./cmd/package -key "$(SIGNING_KEY)"

verify-package:
	go run ./cmd/verify-package
