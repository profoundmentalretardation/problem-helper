.PHONY: build test lint run

build:
	go build -o helper ./cmd/helper

test:
	go test ./...

lint:
	golangci-lint run

run: build
	./helper
