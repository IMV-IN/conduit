SHELL := /bin/bash
VERSION ?= dev
K6 ?= k6
RESULTS ?= test-results
GO ?= go

.PHONY: build test test-short lint vuln bench mock up down obs \
	k6-all k6-overhead k6-accuracy k6-chaos k6-learning k6-soak verify verify-full \
	validate doctor datasets e2e goreleaser-snapshot

build:
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags "-s -w -X main.version=$(VERSION)" -o bin/conduit ./cmd/conduit
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags "-s -w" -o bin/mockprovider ./test/mockprovider

test:
	$(GO) test -race -count=1 ./...

# Fast PR gate (unit + classifier fixture, no race).
test-short:
	$(GO) test -count=1 ./internal/... ./pkg/...

lint:
	$(GO) vet ./...
	command -v staticcheck >/dev/null && staticcheck ./... || echo "staticcheck not installed; run: go install honnef.co/go/tools/cmd/staticcheck@latest"

vuln:
	command -v govulncheck >/dev/null && govulncheck ./... || echo "govulncheck not installed; run: go install golang.org/x/vuln/cmd/govulncheck@latest"

bench:
	$(GO) test -run xxx -bench . -benchmem ./internal/router/... ./internal/stats/... ./internal/classify/...

mock:
	$(GO) run ./test/mockprovider -addr :9001

validate:
	$(GO) run ./cmd/conduit validate --config conduit.example.yaml

doctor:
	$(GO) run ./cmd/conduit doctor --config conduit.example.yaml

up:
	docker compose -f deploy/docker-compose.yml up -d --build

down:
	docker compose -f deploy/docker-compose.yml down -v

obs:
	docker compose -f deploy/docker-compose.yml --profile obs up -d

# ---- datasets (self-provisioning: installs python deps if missing) ----
datasets:
	@python3 -c "import datasets" 2>/dev/null || pip install -r tools/datasets/requirements.txt
	python3 tools/datasets/fetch.py --out test/datasets --max-per-source 200

# ---- k6 suites (need Conduit + mock; `make up` first) ----
$(RESULTS):
	mkdir -p $(RESULTS)

k6-overhead: $(RESULTS)
	$(K6) run test/k6/01_overhead.js

k6-accuracy: $(RESULTS)
	$(K6) run test/k6/02_accuracy.js

k6-chaos: $(RESULTS)
	FAULT=$(FAULT) $(K6) run test/k6/03_chaos.js

k6-learning: $(RESULTS)
	$(K6) run test/k6/04_learning.js
	@echo "--- verdict ---"; jq '.verdict' $(RESULTS)/learning.json

k6-soak: $(RESULTS)
	$(K6) run test/k6/05_soak.js

k6-all: k6-overhead k6-accuracy k6-chaos k6-learning

# Short CI variant of the perf suites (see docs/TESTING.md §6).
e2e: $(RESULTS)
	RATE=300 DUR=15s $(K6) run test/k6/01_overhead.js
	$(K6) run test/k6/02_accuracy.js
	FAULT=errors $(K6) run test/k6/03_chaos.js

# Gate an already-produced results file (CI runs k6 via docker first, then
# calls this; it must NOT re-run k6 because the runner has no k6 binary).
verify:
	@test -f $(RESULTS)/overhead.json || (echo "missing $(RESULTS)/overhead.json; run 'make k6-overhead' first"; exit 1)
	@jq -e --argjson p50 "$${VERIFY_P50:-1.5}" --argjson p99 "$${VERIFY_P99:-5}" \
	  '.overhead_p99_ms < $$p99 and .overhead_p50_ms < $$p50' $(RESULTS)/overhead.json >/dev/null \
	  && echo "overhead OK" || (echo "overhead budget exceeded"; jq . $(RESULTS)/overhead.json; exit 1)

# Local convenience: run the suite, then gate it.
verify-full: k6-overhead verify

goreleaser-snapshot:
	command -v goreleaser >/dev/null && goreleaser release --snapshot --clean || echo "goreleaser not installed"
