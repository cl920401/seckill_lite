SHELL := /bin/bash

.PHONY: run test fmt vet migrate loadtest

run:
	go run ./cmd/api

test:
	go test -race ./...

fmt:
	gofmt -w cmd internal

vet:
	go vet ./...

migrate:
	./scripts/setup_db.sh

loadtest:
	go run ./cmd/loadtest -n 1000 -c 200 -item 1 -activity 1 -mode unique
