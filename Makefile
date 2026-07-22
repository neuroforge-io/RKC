SHELL := /bin/sh
VERSION := $(shell tr -d '\n' < VERSION)
LDFLAGS := -s -w -X main.version=$(VERSION)

.PHONY: all build format-check vet test python-test test-race contracts docs-check plugins smoke reproducibility smoke-api smoke-mcp smoke-git benchmark verify release-verify demo release-binaries complete-package clean package

all: verify build

build:
	mkdir -p bin
	CGO_ENABLED=0 go build -trimpath -ldflags="$(LDFLAGS)" -o bin/rkc ./cmd/rkc
	CGO_ENABLED=0 go build -trimpath -ldflags="$(LDFLAGS)" -o bin/rkc-mcp ./cmd/rkc-mcp

format-check:
	@test -z "$$(gofmt -l cmd internal pkg)" || { echo "Go files require gofmt:"; gofmt -l cmd internal pkg; exit 1; }

vet:
	go vet ./...

test:
	go test ./...

python-test:
	python3 -m unittest discover -s plugins/python-ast -p 'test_*.py' -v

test-race:
	go test -race ./...

contracts:
	python3 scripts/validate-contracts.py


docs-check:
	python3 scripts/validate-docs.py

plugins: build
	./bin/rkc plugins validate --root plugins
	./bin/rkc plugins verify --root plugins --lock plugins/plugins.lock.json

smoke: build
	rm -rf .rkc-smoke .rkc-state-smoke
	./bin/rkc scan --out .rkc-smoke --state-dir .rkc-state-smoke --force examples
	./bin/rkc check --coverage .rkc-smoke/coverage.json --min-inventory-accounting 1 --min-symbol-evidence 1 --min-edge-resolution 0.5 --max-errors 0 --max-high-confidence-secrets 0
	./bin/rkc query --dir .rkc-smoke --limit 5 Login
	./bin/rkc synthesize --dir .rkc-smoke --repo-root examples --out .rkc-smoke/derived-test --packet-only --query Login --limit 1 --force

reproducibility: build
	sh scripts/reproducibility.sh

smoke-api: build
	sh scripts/smoke-api.sh

smoke-mcp: build
	sh scripts/smoke-mcp.sh

smoke-git: build
	sh scripts/smoke-git-acquisition.sh

benchmark: build
	sh scripts/benchmark-reference.sh

verify: format-check vet test python-test contracts docs-check build plugins smoke reproducibility smoke-api smoke-mcp smoke-git

release-verify:
	sh scripts/verify-release.sh

demo: build
	sh scripts/generate-demo.sh

release-binaries:
	rm -rf dist/binaries
	mkdir -p dist/binaries/linux-amd64 dist/binaries/linux-arm64
	GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -ldflags="$(LDFLAGS)" -o dist/binaries/linux-amd64/rkc ./cmd/rkc
	GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -ldflags="$(LDFLAGS)" -o dist/binaries/linux-amd64/rkc-mcp ./cmd/rkc-mcp
	GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -trimpath -ldflags="$(LDFLAGS)" -o dist/binaries/linux-arm64/rkc ./cmd/rkc
	GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -trimpath -ldflags="$(LDFLAGS)" -o dist/binaries/linux-arm64/rkc-mcp ./cmd/rkc-mcp

complete-package: release-verify demo release-binaries
	python3 scripts/package-complete.py --output dist/repository-knowledge-compiler-complete.zip

package: complete-package

clean:
	rm -rf bin dist .rkc .rkc-smoke .rkc-state-smoke .rkc-repro-* .rkc-api-smoke .rkc-mcp-smoke .rkc-git-smoke .rkc-benchmark
