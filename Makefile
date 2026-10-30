VERSION := $(shell cat VERSION)
SIGNING_KEY ?=
VERSION_FLAGS := -X github.com/cyeinfpro/ModelGuard/internal/runtime.Version=$(VERSION)

.PHONY: test build check-package package verify-package

test:
	go test -race ./...
	go vet ./...
	npm test
	python3 -m unittest discover -s tests -p '*_test.py'

build:
	mkdir -p build
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags='-s -w $(VERSION_FLAGS)' -o build/guard-linux-amd64 ./cmd/guard
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -ldflags='-s -w $(VERSION_FLAGS)' -o build/guard-linux-arm64 ./cmd/guard
	go build -trimpath -ldflags='$(VERSION_FLAGS)' -o build/guard-native ./cmd/guard
	go run ./cmd/verify

check-package: build
	python3 scripts/check-package.py

package: build
	go run ./cmd/package $(if $(strip $(SIGNING_KEY)),-key "$(SIGNING_KEY)")
	$(MAKE) verify-package
	python3 scripts/release-checksums.py

verify-package:
	go run ./cmd/verify-package -allow-unsigned
