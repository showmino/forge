.DEFAULT_GOAL := build
BIN := bin

.PHONY: build test lint chart-test platform-chart-test platform-chart-golden generate update-golden tools dex db api api-dev image runner-image clean

build: ## Build forge and forge-api into ./bin
	CGO_ENABLED=0 go build -trimpath -o $(BIN)/ ./cmd/...

test: ## Run all tests with the race detector
	go test -race ./...

HELM ?= go run helm.sh/helm/v3/cmd/helm@v3.21.4

chart-test: ## Render deploy/argocd/chart and compare it with its goldens
	$(HELM) template payments deploy/argocd/chart --namespace payments-staging \
	  -f deploy/argocd/chart/testdata/staging.yaml | diff -u deploy/argocd/chart/testdata/staging.golden -
	$(HELM) template payments deploy/argocd/chart --namespace payments-production \
	  -f deploy/argocd/chart/testdata/production.yaml | diff -u deploy/argocd/chart/testdata/production.golden -
	$(HELM) template payments deploy/argocd/chart --namespace payments-staging \
	  -f deploy/argocd/chart/testdata/database.yaml | diff -u deploy/argocd/chart/testdata/database.golden -
	$(HELM) template payments deploy/argocd/chart --namespace payments-staging \
	  -f deploy/argocd/chart/testdata/public.yaml | diff -u deploy/argocd/chart/testdata/public.golden -
	$(HELM) template payments deploy/argocd/chart --namespace payments-production \
	  -f deploy/argocd/chart/testdata/options.yaml | diff -u deploy/argocd/chart/testdata/options.golden -
	$(HELM) template payments deploy/argocd/chart --namespace payments-staging \
	  -f deploy/argocd/chart/testdata/options-staging.yaml | diff -u deploy/argocd/chart/testdata/options-staging.golden -
	$(HELM) template payments deploy/argocd/chart --namespace payments-production \
	  -f deploy/argocd/chart/testdata/secret-backend-none.yaml | diff -u deploy/argocd/chart/testdata/secret-backend-none.golden -
	$(HELM) template payments deploy/argocd/chart --namespace payments-staging \
	  -f deploy/argocd/chart/testdata/ingress.yaml | diff -u deploy/argocd/chart/testdata/ingress.golden -

PLATFORM_CHART := deploy/platform/chart
PLATFORM_CASES := minimal gateway full ingress-deploys gateway-deploys platform

platform-chart-test: ## Lint deploy/platform/chart and compare its renders with their goldens
	$(HELM) lint $(PLATFORM_CHART) -f $(PLATFORM_CHART)/testdata/minimal.yaml
	for t in $(PLATFORM_CASES); do \
	  $(HELM) template forge $(PLATFORM_CHART) --namespace $$(case $$t in ingress-deploys) echo platform;; *) echo forge;; esac) -f $(PLATFORM_CHART)/testdata/$$t.yaml \
	    | diff -u $(PLATFORM_CHART)/testdata/$$t.golden - || exit 1; \
	done

platform-chart-golden: ## Rewrite deploy/platform/chart's goldens after an intended change
	for t in $(PLATFORM_CASES); do \
	  $(HELM) template forge $(PLATFORM_CHART) --namespace $$(case $$t in ingress-deploys) echo platform;; *) echo forge;; esac) -f $(PLATFORM_CHART)/testdata/$$t.yaml \
	    > $(PLATFORM_CHART)/testdata/$$t.golden || exit 1; \
	done

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

db: ## Start a local PostgreSQL on :55432 for the store tests (FORGE_TEST_DATABASE_URL)
	docker run -d --name forge-test-db -p 55432:5432 -e POSTGRES_PASSWORD=forge mirror.gcr.io/library/postgres:18

api: build ## Run forge-api against local Dex, accepting the web dashboard on localhost:3000
	$(BIN)/forge-api --oidc-issuer http://localhost:5556/dex --oidc-web-client-id forge-web --cors-origins http://localhost:3000

api-dev: build ## Run forge-api without an identity provider (token "dev")
	$(BIN)/forge-api --insecure-dev-token

image: ## Build the forge-api image for k3s (then: docker save forge-api:dev | sudo k3s ctr images import -)
	docker build -t forge-api:dev .

runner-image: ## Build the resource runner image (OpenTofu, the provider, the module, forge-runner)
	docker build -f deploy/runner/Dockerfile -t forge-runner:dev .

clean:
	rm -rf $(BIN) dist
