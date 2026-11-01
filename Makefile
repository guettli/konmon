# The checks of CI. `make check` before a pull request.
# Tool versions: tools/go.mod and package.json (updated by Dependabot).
GO_TOOL := go tool -modfile=tools/go.mod
GOLANGCI_LINT := $(GO_TOOL) golangci-lint
ACTIONLINT := $(GO_TOOL) actionlint
GOVULNCHECK := $(GO_TOOL) govulncheck
TSC := npx --no-install tsc
PRETTIER := npx --no-install prettier

.PHONY: check test build-all lint typescript fmt docs e2e

check: test build-all lint typescript

test:
	go test -race ./...
	go run . checks test --checks examples

# The targets of the release.
build-all:
	for target in linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64; do \
		CGO_ENABLED=0 GOOS=$${target%/*} GOARCH=$${target#*/} go build -trimpath -o /dev/null . || exit 1; \
	done

lint:
	$(GOLANGCI_LINT) run
	$(ACTIONLINT)
	$(GOVULNCHECK) ./...
	go mod tidy && git diff --exit-code go.mod go.sum
	cd tools && go mod tidy && git diff --exit-code go.mod go.sum
	shellcheck hack/*.sh
	$(MAKE) docs && test -z "$$(git status --porcelain -- docs)" || (git status --short -- docs; echo 'docs are out of date: run make docs'; exit 1)

# Generated docs: the CLI reference (docs/cli) and the table of core checks.
docs:
	go run ./internal/docgen

# End-to-end test on a kind cluster (needs kind and docker).
e2e:
	hack/e2e.sh

node_modules: package.json package-lock.json
	npm ci --ignore-scripts --no-audit --no-fund

typescript: node_modules
	cd checks && $(TSC) -p .
	cd examples && $(TSC) -p .
	$(PRETTIER) --check 'checks/*.ts' 'examples/*.ts'

# Fix formatting of Go and TypeScript.
fmt: node_modules
	$(GOLANGCI_LINT) fmt
	$(PRETTIER) --write 'checks/*.ts' 'examples/*.ts'
