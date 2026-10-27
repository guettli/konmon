# The same checks as CI. `make check` before a pull request.
GOLANGCI_LINT := go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.12.2
ACTIONLINT := go run github.com/rhysd/actionlint/cmd/actionlint@v1.7.7
GOVULNCHECK := go run golang.org/x/vuln/cmd/govulncheck@v1.9.0
TSC := npx -y -p typescript@5.9.3 tsc
PRETTIER := npx -y prettier@3.6.2

.PHONY: check test lint typescript fmt

check: test lint typescript

test:
	go test -race ./...
	go run . checks test --checks examples

lint:
	$(GOLANGCI_LINT) run
	$(ACTIONLINT)
	$(GOVULNCHECK) ./...
	go mod tidy && git diff --exit-code go.mod go.sum

typescript:
	cd checks && $(TSC) -p .
	cd examples && $(TSC) -p .
	$(PRETTIER) --check 'checks/*.ts' 'examples/*.ts'

# Fix formatting of Go and TypeScript.
fmt:
	$(GOLANGCI_LINT) fmt
	$(PRETTIER) --write 'checks/*.ts' 'examples/*.ts'
