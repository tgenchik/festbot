DB ?= postgres://festbot:festbot@localhost:5432/festbot?sslmode=disable

.PHONY: tidy build test test-integration up down migrate

tidy:            ## подтянуть зависимости и сгенерировать go.sum
	go mod tidy

build:
	go build ./...

test:
	go test ./...

test-integration: ## сквозной тест на реальном Postgres (схема будет пересоздана!)
	DATABASE_URL="$(DB)" go test -tags integration -count=1 -v ./test/integration/

up:
	docker compose up -d --build

down:
	docker compose down

migrate:
	DATABASE_URL="$(DB)" go run ./cmd/migrate up
