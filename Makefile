BINARY := bin/carteiro
VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)

.PHONY: build run test vet fmt clean install

build:
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
