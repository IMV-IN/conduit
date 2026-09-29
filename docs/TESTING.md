# Conduit — Testing Harness

The harness answers three different questions. Keep them separate when you read results.

| Question | Suite | Type of evidence |
|---|---|---|
| **Is it fast?** (performance) | `01_overhead`, `05_soak` | Latency added by the gateway, throughput knee, leaks |
| **Is it right?** (accuracy) | `02_accuracy` | Task classification accuracy, hard-constraint safety, objective sanity |
| **Is it reliable?** (resilience) | `03_chaos` | Failure rate and recovery under injected faults |
| **Is it useful?** (value) | `04_learning` | Cost and quality versus static routing baselines |

## 1. Components

```
test/
├── mockprovider/main.go   Fault-injecting OpenAI-compatible mock
├── datasets/tasks.jsonl   Labeled prompts (30 to start; grow to 500+ before trusting F1)
└── k6/
    ├── lib.js             helpers (chat, feedback, mock control, admin)
    ├── 01_overhead.js     direct vs pinned vs auto-routed latency
    ├── 02_accuracy.js     classification + constraint safety + objective sanity
    ├── 03_chaos.js        errors | down | slow on the preferred model
    ├── 04_learning.js     policy comparison with simulated ground truth
    └── 05_soak.js         ramp to MAX_RATE, hold, cool down, sample heap
```

### Mock provider control API

| Endpoint | Effect |
|---|---|
| `POST /__control/profile?model=premium` body `{"error_rate":0.6}` | Merge-patch a model profile |
| `POST /__control/reset` | Restore default profiles |
| `GET /__control/profiles` | Inspect current profiles |

Profile fields: `ttft_median_ms`, `ttft_sigma` (lognormal), `tokens_per_sec`, `output_tokens`, `error_rate` (HTTP 500), `rate_limit_rate` (HTTP 429), `down` (HTTP 503), `context_tokens`, `tools`, `quality{task: P(correct)}`.

**Simulated correctness.** Replies end in `ANSWER:OK` with probability `quality[task]`, else `ANSWER:WRONG`. The task label travels in `metadata.eval_task`, which Conduit passes through but never uses for routing. This gives k6 a ground-truth signal through any gateway.

**Strictness.** The mock returns HTTP 400 if a request exceeds a model's context or uses tools on a model without them, so a routing-constraint bug surfaces as a hard failure rather than a silent mis-route.

### What the gateway must expose for the tests

Response headers `X-Conduit-Model`, `-Task`, `-Decision-Id`, `-Attempts`, `-Cost-Usd`; `POST /v1/feedback`; `GET /admin/v1/runtime` returning `{heap_alloc_mb, goroutines}`; policies `baseline-best`, `baseline-cheap`, `round-robin`, `default` (all in `conduit.example.yaml`).

## 2. Running

```bash
make up                 # Conduit + mock via Docker Compose
make k6-all             # 01–04 sequentially (about 10 minutes)
make k6-soak            # 12 minutes, run separately
make verify             # overhead gate for CI
docker compose -f deploy/docker-compose.yml --profile test run --rm k6 run /test/k6/03_chaos.js
FAULT=down make k6-chaos
```

k6 v0.55+ is required for the `k6/execution` API used by `03` and `04`. Results land in `test-results/*.json`.

## 3. Pass criteria

Targets are *initial* budgets. Tighten them once you have measurements on your hardware.

| Suite | Metric | Target |
|---|---|---|
| 01 | Added latency, auto-routed, p50 / p99 | < 1.5 ms / < 5 ms at 500 rps |
| 01 | Routing decision alone (auto − pinned), p50 | < 0.3 ms |
| 02 | `task_class_match` | > 0.85 (macro-F1 ≥ 0.85 offline via `conduit eval`) |
| 02 | `constraint_violation` | **exactly 0** across thousands of adversarial requests |
| 02 | `best_prefers_premium_for_code` / `cheap_avoids_premium` | > 0.85 / > 0.90 |
| 03 | Failure rate in fault window | < 0.5% (all three fault modes) |
| 03 | Premium share: pre / fault / recovered | > 0.7 / < 0.5 / > 0.6 |
| 03 | p95 latency in fault window | < 3 s (errors, down), < 4 s (slow, with hedging) |
| 04 | `default` late accuracy vs best | ≥ 0.95× |
| 04 | `default` late cost per request vs best | ≤ 0.60× |
| 04 | `default` late accuracy vs early | not worse than −1 pt |
| 05 | Error rate / p95 through the ramp | < 1% / < 2 s |
| 05 | Heap after cool-down | returns to within 20% of the pre-load level |

## 4. Reading results honestly

- **Warm up first.** Run 01 twice; discard the first run (JIT of connection pools, page cache).
- **Isolate the load generator.** k6 saturating the same CPU as the gateway inflates overhead. Use separate machines or pin CPUs (`taskset`).
- **The mock is not the internet.** 01 measures *gateway* cost with near-zero upstream latency; real TTFT is dominated by the provider. Report both.
- **Simulated quality is a model, not reality.** 04 proves the *learning machinery* converges when a signal exists. It does not prove your real reward signal is good; validate that with the Replay Lab on real traffic and with human-labeled samples.
- **Small dataset warning.** 30 labeled prompts is a smoke test. A classifier F1 from 30 items has a wide confidence interval; grow the set before publishing numbers.
- **Report hardware and versions** (CPU, RAM, Go, k6, kernel, Conduit commit) next to every published number, and commit the raw JSON.

## 5. Other layers (Go tests, not k6)

| Layer | Focus |
|---|---|
| Property (`rapid`) | Hard constraints never violated over 10⁶ random requests; selection probabilities sum to 1 and every feasible model has p ≥ ε/n; Beta merge is commutative/associative |
| Simulator regret | Seeded bandit environment; cumulative regret is sublinear and drops after a quality shift with decay enabled |
| Replay estimators | Synthetic logs with known ground truth: DR within 3% and CI coverage ≥ 90% |
| Golden fixtures | Recorded provider payloads (incl. streams, tool calls) → canonical → back |
| Fuzz | SSE parser, JSON scanner, config loader |
| Race and leak | `go test -race`, `goleak` after 10k cancelled streams and hedged requests |
| UI | Playwright: config edit → dry-run diff → apply; decision explorer; live feed |

## 6. CI wiring (suggested)

1. PR: unit, property, race, `staticcheck`, `govulncheck`, `benchstat` on `internal/router`.
2. PR (Compose): `01` (short: `DUR=15s RATE=300`), `02`, `03` with `FAULT=errors`.
3. Nightly: full `01`–`04`, publish JSON as build artifacts, alert on regression > 20%.
4. Weekly: `05` soak.

## 7. What has and has not been verified in this repo

- Verified (v0 gateway, Go 1.22):
  - `go build ./...`, `go vet ./...` clean; `go test ./internal/... ./pkg/...`
    passes, including `-race` on router/stats/health/classify.
  - Classifier fixture: 32/32 on `test/datasets/tasks.jsonl`
    (`TestFixtureAccuracy`, gate ≥ 0.85).
  - Router property test: 20k randomized requests, hard capability/context
    filters never violated; guarded-selection propensities exact (ε-floor,
    determinism, starvation checks).
  - Live smoke (mock + gateway on localhost): `auto:cheap`/`auto:best`/
    pinned/`policy:baseline-best` all 200 with correct `X-Conduit-*` headers;
    tools request avoided `mock/notools`; 20k-token prompt avoided `mock/tiny`;
    streaming SSE passthrough; `/v1/route`, `/v1/feedback`,
    `/admin/v1/runtime` all OK.
  - Mock provider compiles (`go vet`) and behaves as documented; all five k6
    scripts parse under k6 v0.55; scripts `01` and `04` previously ran
    end-to-end against a throwaway stand-in, which exposed and fixed two
    harness bugs (a vacuous PASS when all requests fail, and double-counted
    samples).
- Not verified: full k6 suites `01`–`05` against this gateway binary at the
  target rates (needs two machines or pinned CPUs per §4); every number in
  the targets table remains a goal until a measured run with published
  hardware details and raw JSON says otherwise. Suites `02`, `03`, `05`
  have only been syntax-checked plus covered by the equivalent Go tests.
