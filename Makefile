.PHONY: help build pre-commit release

SHELL := /bin/bash

help:  ## Show this help
	@awk 'BEGIN {FS = ":.*##[[:space:]]*"} /^[[:alnum:]_.%\/-]+:.*##[[:space:]]*/ {print $$1 "\t" $$2}' $(MAKEFILE_LIST) \
		| sort -k1,1 \
		| awk 'BEGIN {FS = "\t"} {printf "\033[36m  %-20s\033[0m %s\n", $$1, $$2}'
	@echo ""
	@echo "Example (chalk-private, dry-run):"
	@echo "  BUILDKITE_API_TOKEN=<token> ./buildkite-sync-action \\"
	@echo "    -repo chalk-ai/chalk-private \\"
	@echo "    -cluster-id \$$BUILDKITE_CLUSTER_ID \\"
	@echo "    -work-dir ../chalk-private \\"
	@echo "    -dry-run"

build:  ## Build the binary (./buildkite-sync-action)
	go build -o buildkite-sync-action .

pre-commit:  ## Format, vet, fix, and test
	gofmt -l -w .
	go vet ./...
	go fix ./...
	go test -count=1 -shuffle=on ./...

release:  ## Force-move v1 tag to HEAD and push
	git tag -f v1
	git push origin v1 --force
