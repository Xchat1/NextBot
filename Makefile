.PHONY: build test web

build: web
	go build -trimpath -o bin/nextbot ./cmd/nextbot

web:
	npm --prefix web ci
	npm --prefix web run build

test:
	go test -race ./...
