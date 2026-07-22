SHELL := /bin/sh
VERSION := $(shell tr -d '\n' < VERSION)
LDFLAGS := -s -w -X main.version=$(VERSION)
PYTHON ?= $(if $(wildcard .venv/bin/python),.venv/bin/python,python3)
MODEL_RUNTIME ?=
MODEL_QUALIFICATION_OUTPUT ?=

.PHONY: all build safe-build format-check vet test python-test coverage safe-coverage test-race contracts docs-check licenses model-lock-check model-runtime-portable model-runtime-native model-fetch-generation model-fetch-embedding model-qualify plugins smoke reproducibility smoke-api smoke-mcp smoke-git benchmark verify safe-verify safe-test safe-test-race release-verify safe-release-verify self-catalogue demo release-binaries complete-package clean package

all: verify build

build:
	mkdir -p bin
	CGO_ENABLED=0 go build -trimpath -ldflags="$(LDFLAGS)" -o bin/rkc ./cmd/rkc
	CGO_ENABLED=0 go build -trimpath -ldflags="$(LDFLAGS)" -o bin/rkc-mcp ./cmd/rkc-mcp

safe-build:
	sh scripts/with-rkc-limits.sh $(MAKE) build

format-check:
	@test -z "$$(gofmt -l cmd internal pkg)" || { echo "Go files require gofmt:"; gofmt -l cmd internal pkg; exit 1; }

vet:
	go vet ./...

test:
	go test -p=1 ./...

python-test:
	$(PYTHON) -m unittest discover -s plugins/python-ast -p 'test_*.py' -v
	$(PYTHON) -m unittest discover -s scripts -p 'test_*.py' -v

coverage:
	$(PYTHON) scripts/coverage_gate.py

safe-coverage:
	sh scripts/with-rkc-limits.sh $(PYTHON) scripts/coverage_gate.py

test-race:
	go test -p=1 -race ./...

# Local development guards deliberately yield CPU/I/O to higher-priority work
# and fail closed if the user cgroup controller is unavailable.
safe-test:
	sh scripts/with-rkc-limits.sh go test -p=1 ./...

safe-test-race:
	sh scripts/with-rkc-limits.sh go test -p=1 -race ./...

contracts:
	$(PYTHON) scripts/validate-contracts.py


docs-check:
	$(PYTHON) scripts/validate-docs.py

licenses:
	$(PYTHON) scripts/validate-licenses.py

model-lock-check:
	$(PYTHON) scripts/model_assets.py validate-lock

# Every source build, weight download, and real-model run enters the same
# subordinate cgroup used by RKC's other expensive local validation.
model-runtime-portable:
	sh scripts/with-rkc-limits.sh $(PYTHON) scripts/bootstrap_llama_cpp.py --profile portable

model-runtime-native:
	sh scripts/with-rkc-limits.sh $(PYTHON) scripts/bootstrap_llama_cpp.py --profile native

model-fetch-generation:
	sh scripts/with-rkc-limits.sh $(PYTHON) scripts/model_assets.py fetch --asset qwen3.5-2b-q4-k-m-candidate --cache-root .rkc-models --accept-license Apache-2.0

model-fetch-embedding:
	sh scripts/with-rkc-limits.sh $(PYTHON) scripts/model_assets.py fetch --asset qwen3-embedding-0.6b-q8-0-candidate --cache-root .rkc-models --accept-license Apache-2.0

model-qualify:
	@test -n "$(MODEL_RUNTIME)" || { echo "MODEL_RUNTIME is required" >&2; exit 2; }
	@test -n "$(MODEL_QUALIFICATION_OUTPUT)" || { echo "MODEL_QUALIFICATION_OUTPUT is required" >&2; exit 2; }
	sh scripts/with-rkc-limits.sh $(PYTHON) scripts/qualify_models.py --runtime "$(MODEL_RUNTIME)" --output "$(MODEL_QUALIFICATION_OUTPUT)"

plugins: build
	./bin/rkc plugins validate --root plugins
	./bin/rkc plugins verify --root plugins --lock plugins/plugins.lock.json

smoke: build
	sh scripts/smoke-reference.sh

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

verify: format-check vet coverage contracts docs-check licenses model-lock-check build plugins smoke reproducibility smoke-api smoke-mcp smoke-git

safe-verify:
	sh scripts/with-rkc-limits.sh $(MAKE) verify

release-verify:
	sh scripts/verify-release.sh

safe-release-verify:
	sh scripts/with-rkc-limits.sh $(MAKE) release-verify

# The wrapper builds and scans inside one subordinate cgroup and refuses dirty
# source, unsafe output, generated-input recursion, links, and model weights.
self-catalogue:
	sh scripts/with-rkc-limits.sh bash scripts/self-catalogue.sh

demo: build
	sh scripts/generate-demo.sh

release-binaries:
	sh scripts/build-release-binaries.sh

complete-package: release-verify demo release-binaries
	$(PYTHON) scripts/package-complete.py --output dist/repository-knowledge-compiler-complete.zip --force

package: complete-package

clean:
	rm -f bin/rkc bin/rkc-mcp
	@rmdir bin 2>/dev/null || true
	@echo "Retained generated atlases, snapshot stores, and dist artifacts; RKC replaces owned outputs through marker-aware publication."
