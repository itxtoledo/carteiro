BINARY := bin/carteiro
VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)

# The UI is embedded into the binary (go:embed), so every release build runs
# `web` first. The Docker build and the CI release workflow do the same.
.PHONY: web web-dev build run test vet fmt clean install

web:
	cd web && npm ci && npm run build

web-dev:
	cd web && npm run dev

build: web
	go build -trimpath -ldflags "-s -w -X main.version=$(VERSION)" -o $(BINARY) ./cmd/carteiro

run: build
	./$(BINARY)

test:
	go test ./...

vet:
	go vet ./...

fmt:
	gofmt -l -w cmd internal

clean:
	rm -rf bin

# Installs as a local daemon (linux/macos): binary + config and data folders
install: build
	install -d /usr/local/bin
	install -m 0755 $(BINARY) /usr/local/bin/carteiro
	install -d "$(HOME)/.config/carteiro" "$(HOME)/.local/share/carteiro"
	@echo "carteiro installed. Copy a config to $(HOME)/.config/carteiro/config.yaml (see config.example.yaml)"
