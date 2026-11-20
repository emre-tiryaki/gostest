VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)

.PHONY: build install test test-short lint clean

build:
	go build -ldflags "$(LDFLAGS)" -o gostest .

install:
	go install -ldflags "$(LDFLAGS)" .

test:
	go test -race ./...

test-short:
	go test -race -short ./...

lint:
	@test -z "$$(gofmt -l .)" || { echo "gofmt needed:"; gofmt -l .; exit 1; }
	go vet ./...

clean:
	rm -f gostest
