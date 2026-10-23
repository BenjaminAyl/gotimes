.PHONY: dev build test lint up down logs

dev:
	air -c .air.toml

build:
	go build -o bin/gotimes ./cmd/gotimes

test:
	go test ./...

lint:
	golangci-lint run

up:
	./scripts/build.sh

down:
	docker compose down

logs:
	docker compose logs -f
