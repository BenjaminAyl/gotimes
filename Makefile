.PHONY: dev build test lint image

dev:
	air -c .air.toml

build:
	go build -o bin/rss ./cmd/rss

test:
	go test ./...

lint:
	golangci-lint run

image:
	docker build -t rss .
