BIN     := bin/repoguard
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)

.DEFAULT_GOAL := help
.PHONY: help build test lint run ci docker

help: ## show targets
	@grep -E '^[a-z-]+:.*## ' $(MAKEFILE_LIST) | awk 'BEGIN{FS=":.*## "}{printf "  \033[36m%-8s\033[0m %s\n",$$1,$$2}'

build: ## build the binary
	CGO_ENABLED=0 go build -ldflags "-X main.version=$(VERSION)" -o $(BIN) ./cmd/repoguard

test: ## run tests with the race detector
	go test -race ./...

lint: ## gofmt + vet
	@test -z "$$(gofmt -l . | tee /dev/stderr)"
	go vet ./...

run: build ## run the service with deploy/.env
	set -a && . deploy/.env && set +a && $(BIN) serve

ci: build ## run `repoguard ci` on this repository
	$(BIN) ci --dir .

docker: ## build both images
	docker build -f deploy/Dockerfile -t repoguard:$(VERSION) --build-arg VERSION=$(VERSION) .
	docker build -f deploy/ci-runner.Dockerfile -t repoguard-ci:$(VERSION) --build-arg VERSION=$(VERSION) .
