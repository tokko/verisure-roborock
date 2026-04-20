.PHONY: build dev test lint clean build-linux build-linux-arm64

BINARY  = dist/verisure-roborock
CMD     = ./cmd/verisure-roborock
VERSION = $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS = -ldflags "-X main.version=$(VERSION) -s -w"

build:
	go build $(LDFLAGS) -o $(BINARY) $(CMD)

dev:
	@test -f .env || (echo "Copy .env.example to .env and fill in credentials"; exit 1)
	go run $(CMD)

test:
	go test ./...

test-verbose:
	go test -v ./...

lint:
	go vet ./...
	@which staticcheck >/dev/null 2>&1 && staticcheck ./... || echo "staticcheck not installed, skipping"

clean:
	rm -rf dist/

build-linux:
	GOOS=linux GOARCH=amd64 go build $(LDFLAGS) -o dist/verisure-roborock-linux-amd64 $(CMD)

build-linux-arm64:
	GOOS=linux GOARCH=arm64 go build $(LDFLAGS) -o dist/verisure-roborock-linux-arm64 $(CMD)
