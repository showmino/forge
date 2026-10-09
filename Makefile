.DEFAULT_GOAL := build
BIN := bin

.PHONY: build test lint chart-test generate update-golden tools dex api api-dev image clean

build: ## Build forge and forge-api into ./bin
	CGO_ENABLED=0 go build -trimpath -o $(BIN)/ ./cmd/...

test: ## Run all tests with the race detector
	go test -race ./...

HELM ?= go run helm.sh/helm/v3/cmd/helm@v3.21.4

chart-test: ## Render deploy/argocd/chart and compare it with its golden
	$(HELM) template payments deploy/argocd/chart --namespace payments-staging \
	  -f deploy/argocd/chart/testdata/staging.yaml | diff -u deploy/argocd/chart/testdata/staging.golden -

lint: ## Lint Go code and protos
	golangci-lint run ./...
	buf lint

generate: ## Regenerate gen/ from proto/
	buf generate

update-golden: ## Rewrite golden files after an intended output change
	go test ./internal/output ./internal/template ./internal/cli -update

tools: ## Install buf and the protoc plugins at the versions CI uses
	go install github.com/bufbuild/buf/cmd/buf@v1.47.2
	go install google.golang.org/protobuf/cmd/protoc-gen-go@v1.36.5
	go install connectrpc.com/connect/cmd/protoc-gen-connect-go@v1.18.1

dex: ## Start the local Dex identity provider on :5556
	docker compose -f deploy/dex/compose.yaml up -d

api: build ## Run forge-api against local Dex
	$(BIN)/forge-api --oidc-issuer http://localhost:5556/dex

api-dev: build ## Run forge-api without an identity provider (token "dev")
	$(BIN)/forge-api --insecure-dev-token

image: ## Build the forge-api image for k3s (then: docker save forge-api:dev | sudo k3s ctr images import -)
	docker build -t forge-api:dev .

clean:
	rm -rf $(BIN) dist
