.PHONY: build test vet race updater-smoke run docker-up docker-down docker-smoke

build:
	go build -trimpath -o transcript-api ./cmd/transcript-api

test:
	go test ./...

vet:
	go vet ./...

race:
	go test -race ./...

updater-smoke:
	./tests/auto-update-smoke.sh

run:
	go run ./cmd/transcript-api

docker-up:
	docker compose up -d --build

docker-down:
	docker compose down

docker-smoke:
	./tests/docker-storage-smoke.sh
