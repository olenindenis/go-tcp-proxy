BIN      := bin/tcp-proxy
COMPOSE  := docker compose -f deployments/docker-compose.yaml

.PHONY: build test test-e2e lint lint-fix run docker up down clean

build:
	CGO_ENABLED=0 go build -trimpath -o $(BIN) ./cmd/tcp-proxy

test:
	go test -race ./...

# Requires the demo stack: make up
test-e2e:
	go test -tags e2e -count=1 ./test/e2e/...

lint:
	docker run -t --rm -v $$(pwd):/app -w /app golangci/golangci-lint:v2.14.0 golangci-lint run

run: build
	$(BIN) -config configs/proxy.yaml

docker:
	docker build -f build/package/Dockerfile -t tcp-proxy:latest .

up:
	$(COMPOSE) up --build -d --wait

down:
	$(COMPOSE) down

clean:
	rm -rf bin
