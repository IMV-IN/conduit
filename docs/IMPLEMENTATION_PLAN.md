# Conduit — Implementation Plan

Companion to `DESIGN.md`. This document says **what to build, in what order, with what acceptance criteria**, and includes the core code skeletons so the hard parts are unambiguous.

---

## 1. Repository layout

```
conduit/
├── cmd/
│   ├── conduit/            # main binary: serve|validate|simulate|doctor|keys|ledger|replay|eval
│   └── conduit-train/      # trains the task-classifier weights from labeled JSONL
├── pkg/
│   └── router/             # public Go library: Route(ctx, Request) (Plan, error)
├── internal/
│   ├── server/             # HTTP ingress, middleware, auth
│   ├── canonical/          # request/response/chunk types, lazy JSON scanning
│   ├── classify/           # features, rules, linear model, plug-in hook
│   ├── catalog/            # models, prices, capabilities, priors
│   ├── policy/             # policy structs, CEL matching, resolution
│   ├── router/             # filters, estimators, scorer, selector, planner
│   ├── stats/              # EWMA, DDSketch, Wilson bound, Beta counts, snapshots
│   ├── health/             # breakers, outlier ejection, capacity governor
│   ├── exec/               # executor: first-token gate, retry, hedge
│   ├── provider/           # openai/, anthropic/, gemini/, bedrock/, ollama(=openai)
│   ├── budget/             # hierarchical budgets, reserve/reconcile
│   ├── cache/              # exact cache, semantic interfaces
│   ├── ledger/             # sqlite + postgres writers, ring buffer
│   ├── learn/              # reward, judge sampler, feedback intake
│   ├── replay/             # IPS / SNIPS / DR, policy simulation
│   ├── admin/              # REST + SSE + embedded UI
│   ├── config/             # load, validate (JSON Schema), versioning, hot reload
│   └── telemetry/          # prometheus, otel, slog
├── ui/                     # React + Vite + TS; built assets embedded via go:embed
│   └── design.md
├── deploy/                 # Dockerfile, compose, helm/, prometheus.yml, grafana dashboards
├── test/
│   ├── mockprovider/       # fault-injecting OpenAI-compatible mock
│   ├── k6/                 # performance, accuracy, chaos, learning, soak
│   └── datasets/           # labeled tasks for accuracy tests
├── docs/                   # DESIGN.md, IMPLEMENTATION_PLAN.md, TESTING.md
├── examples/               # python, ts, curl, langchain
├── conduit.example.yaml
├── Makefile
└── go.mod
```

### Dependencies (keep the list short)

| Purpose | Choice |
|---|---|
| HTTP | `net/http` (Go 1.22+ mux) |
| JSON scanning | `github.com/tidwall/gjson` + `sjson` for splicing |
| SQLite | `modernc.org/sqlite` (pure Go) |
| Postgres | `github.com/jackc/pgx/v5` |
| CEL | `github.com/google/cel-go` |
| Sketches | `github.com/DataDog/sketches-go` (DDSketch) |
| Metrics | `github.com/prometheus/client_golang` |
| Tracing | `go.opentelemetry.io/otel` |
| Config validation | `github.com/santhosh-tekuri/jsonschema/v6` |
| YAML | `gopkg.in/yaml.v3` |
| CLI | `github.com/spf13/cobra` |
| Tests | stdlib `testing`, `testify`, `pgregory.net/rapid` (property tests) |

---

## 2. Core code skeletons

These are the parts where a wrong design choice is costly. Everything else is conventional plumbing.

### 2.1 Canonical types and provider interface

```go
// internal/canonical/types.go
package canonical

type Message struct {
    Role    string
    Content []Part // text, image_url, tool_call, tool_result
}

type Request struct {
    ID          string
    Tenant      string
    KeyID       string
    Requested   string // "auto", "auto:cheap", "policy:x", "openai/gpt-..."
    Messages    []Message
    Tools       bool
    Vision      bool
    JSONMode    bool
    Stream      bool
    MaxOutput   int
    InTokensEst int
    Metadata    map[string]string
    Raw         []byte // original body for zero-copy passthrough
    Task        string
    TaskConf    float64
}

type Chunk struct {
    Delta        string
    ToolDelta    []byte
    FinishReason string
    Usage        *Usage // set on final chunk if provider reports
}

type Usage struct{ In, Out, CachedIn int }
```

```go
// internal/provider/provider.go
package provider

type Stream interface {
    Recv() (canonical.Chunk, error) // io.EOF at end
    Usage() canonical.Usage
    Close() error
}

type Provider interface {
    Name() string
    Chat(ctx context.Context, m *catalog.Model, r *canonical.Request) (Stream, error)
    Embed(ctx context.Context, m *catalog.Model, r *canonical.EmbedRequest) (*canonical.EmbedResponse, error)
}

// Errors carry a class so the executor doesn't parse strings.
type Error struct {
    Class      ErrClass // Retryable, ContextExceeded, Auth, Client, Refusal
    HTTPStatus int
    RetryAfter time.Duration
    Err        error
}
```

### 2.2 Scorer (pure, table-testable)

```go
// internal/router/score.go
package router

import ("math"; "sort")

type Weights struct{ Quality, Cost, Latency, Risk float64 }

type Candidate struct {
    Model      *catalog.Model
    Q, C, L, R float64 // quality [0,1], USD, ms, risk [0,1]
    cn, ln     float64 // normalised
    U          float64
    Reject     string  // non-empty => filtered out (kept for the ledger)
    Frontier   bool
}

func Score(all []*Candidate, w Weights) (ranked []*Candidate) {
    for _, c := range all {
        if c.Reject == "" { ranked = append(ranked, c) }
    }
    if len(ranked) == 0 { return nil }

    cLo, cHi := math.Inf(1), math.Inf(-1)
    lLo, lHi := math.Inf(1), math.Inf(-1)
    for _, c := range ranked {
        lc := math.Log(c.C + 1e-6) // floor so free/local models don't distort the scale
        cLo, cHi = math.Min(cLo, lc), math.Max(cHi, lc)
        lLo, lHi = math.Min(lLo, c.L), math.Max(lHi, c.L)
    }
    norm := func(x, lo, hi float64) float64 {
        if hi-lo < 1e-12 { return 0 }
        return (x - lo) / (hi - lo)
    }
    for _, c := range ranked {
        c.cn = norm(math.Log(c.C+1e-6), cLo, cHi)
        c.ln = norm(c.L, lLo, lHi)
        c.U = w.Quality*c.Q - w.Cost*c.cn - w.Latency*c.ln - w.Risk*c.R
    }
    sort.Slice(ranked, func(i, j int) bool { return ranked[i].U > ranked[j].U })
    markFrontier(ranked) // non-dominated in (Q up, C down, L down)
    return ranked
}
```

### 2.3 Guarded selection with exact propensity

```go
// internal/router/select.go
type Explore struct{ Epsilon, Delta, Tau float64 }

// Select draws from  p(a) = (1-eps)*guardedSoftmax(a) + eps/n  and returns the exact p(a).
// u must be uniform in [0,1) from a PRNG seeded with hash(decisionID) => reproducible.
func Select(ranked []*Candidate, ex Explore, u float64) (idx int, propensity float64) {
    n := len(ranked)
    best := ranked[0].U
    w := make([]float64, n) // small; use a pooled fixed-size array in production
    var z float64
    for i, c := range ranked {
        if best-c.U <= ex.Delta {
            w[i] = math.Exp((c.U - best) / ex.Tau)
            z += w[i]
        }
    }
    acc := 0.0
    for i := range ranked {
        p := (1-ex.Epsilon)*w[i]/z + ex.Epsilon/float64(n)
        acc += p
        if u < acc { return i, p }
    }
    last := n - 1
    return last, (1-ex.Epsilon)*w[last]/z + ex.Epsilon/float64(n)
}
```

Property tests (`rapid`) for `Select`: probabilities sum to 1 (±1e-9); every feasible candidate has `p ≥ ε/n`; `ε = 0` and `δ = 0` collapses to argmax; empirical frequencies over 10⁶ draws match `p` within tolerance.

### 2.4 Beta posterior with lazy decay

```go
// internal/stats/beta.go
type Beta struct {
    A, B   float64
    Stamp  time.Time
}

func (b *Beta) decay(now time.Time, halfLife time.Duration) {
    if b.Stamp.IsZero() { b.Stamp = now; return }
    g := math.Pow(0.5, float64(now.Sub(b.Stamp))/float64(halfLife))
    b.A, b.B, b.Stamp = b.A*g, b.B*g, now
}

func (b *Beta) Update(r float64, now time.Time, halfLife time.Duration) {
    b.decay(now, halfLife)
    b.A += r
    b.B += 1 - r
}

func (b *Beta) MeanSD() (mean, sd float64) {
    s := b.A + b.B
    mean = b.A / s
    sd = math.Sqrt(b.A * b.B / (s * s * (s + 1)))
    return
}

// Merge is commutative and associative once both sides are decayed to the same instant.
func (b *Beta) Merge(o Beta, now time.Time, halfLife time.Duration) {
    b.decay(now, halfLife); o.decay(now, halfLife)
    b.A += o.A; b.B += o.B
}
```

### 2.5 Circuit breaker

```go
// internal/health/breaker.go
type State int32
const (Closed State = iota; Open; HalfOpen)

type BreakerCfg struct {
    ConsecFails   int           // e.g. 5
    MinRequests   int           // e.g. 20
    ErrRate       float64       // e.g. 0.5
    OpenFor       time.Duration // base, e.g. 10s (exponential backoff + jitter)
    HalfOpenProbes int          // e.g. 3
}

type Breaker struct {
    mu       sync.Mutex
    cfg      BreakerCfg
    state    State
    fails    int
    win      *ringWindow // rolling outcomes over last N seconds
    openedAt time.Time
    trips    int
    probes   int
}

func (b *Breaker) Allow(now time.Time) bool {
    b.mu.Lock(); defer b.mu.Unlock()
    switch b.state {
    case Closed:
        return true
    case Open:
        if now.Sub(b.openedAt) >= b.backoff() {
            b.state, b.probes = HalfOpen, 0
        } else { return false }
        fallthrough
    case HalfOpen:
        if b.probes < b.cfg.HalfOpenProbes { b.probes++; return true }
        return false
    }
    return false
}

func (b *Breaker) Report(now time.Time, ok bool) {
    b.mu.Lock(); defer b.mu.Unlock()
    b.win.Add(now, ok)
    if ok {
        b.fails = 0
        if b.state == HalfOpen { b.state, b.trips = Closed, 0 }
        return
    }
    b.fails++
    if b.state == HalfOpen || b.fails >= b.cfg.ConsecFails ||
        (b.win.N() >= b.cfg.MinRequests && b.win.ErrRate() >= b.cfg.ErrRate) {
        b.state, b.openedAt = Open, now
        b.trips++
    }
}
```

### 2.6 Executor: first-token gate + retry + hedge

```go
// internal/exec/run.go
// tryUntilFirstToken returns a Result whose stream has already yielded its first chunk
// (buffered inside Result) or a classified error. Nothing is written to the client yet.
func (e *Executor) Run(ctx context.Context, p *router.Plan) (*Result, error) {
    type out struct{ i int; r *Result; err error }
    ch := make(chan out, len(p.Attempts))       // buffered: late losers never leak goroutines
    cancels := make([]context.CancelFunc, len(p.Attempts))

    launch := func(i int) {
        actx, cancel := context.WithCancel(ctx)
        cancels[i] = cancel
        go func() { r, err := e.tryUntilFirstToken(actx, p, i); ch <- out{i, r, err} }()
    }

    next, inflight := 0, 0
    launch(next); next++; inflight++

    // A zero HedgeDelay means "no hedging": use a timer that will never fire in practice.
    d := p.HedgeDelay
    if d <= 0 { d = 24 * time.Hour }
    hedge := time.NewTimer(d)
    defer hedge.Stop()

    var lastErr error
    for inflight > 0 {
        select {
        case <-hedge.C:
            if p.HedgeDelay > 0 && next < len(p.Attempts) && e.hedgeBudget.Allow() {
                launch(next); next++; inflight++
                e.metrics.Hedge.Inc()
            }
        case o := <-ch:
            inflight--
            if o.err == nil {
                for j, c := range cancels { if c != nil && j != o.i { c() } } // cancel losers
                go e.drainLosers(ch, inflight)                                  // close late winners' streams
                o.r.OnClose(cancels[o.i])
                return o.r, nil
            }
            lastErr = o.err
            e.health.Report(p.Attempts[o.i], o.err)
            if provider.IsRetryable(o.err) && next < len(p.Attempts) {
                launch(next); next++; inflight++
            }
        case <-ctx.Done():
            return nil, ctx.Err()
        }
    }
    return nil, lastErr
}
```

Rules to encode in tests: no bytes to the client before `Run` returns; only one attempt's stream is ever forwarded; every non-winning stream is closed; total attempts ≤ `max_attempts`; hedges ≤ budget.

### 2.7 Task classifier (tiered)

```go
// internal/classify/classify.go
func (c *Classifier) Classify(r *canonical.Request) (class string, conf float64) {
    if t := r.Metadata["task"]; t != "" && c.known(t) { return t, 1 }   // tier 0
    f := extract(r)                                                     // structural + hashed n-grams
    if cls, ok := c.rules.Match(f); ok { return cls, 0.95 }             // tier 1
    probs := c.model.Predict(f)                                         // tier 2: softmax over linear scores
    cls, conf := argmax(probs, c.classes)
    if conf < c.cfg.MinConf && c.hook != nil {                          // tier 3 (optional)
        if cls2, conf2, err := c.hook.Classify(r, 5*time.Millisecond); err == nil { return cls2, conf2 }
    }
    if conf < c.cfg.MinConf { return "chat", conf }                     // generalist fallback
    return cls, conf
}
```

---

## 3. Phased plan

Estimates assume one focused developer (≈ 15–20 h/week). Halve them with two people. Every phase ends in a **demoable, tested** state.

### Phase 0 — Foundations (week 1)

**Tasks**
- `go mod init`, repo layout, `Makefile`, CI (test, `staticcheck`, `govulncheck`, race detector).
- Canonical types and the `Provider` interface.
- Config loader + JSON Schema validation + `conduit validate`.
- **`test/mockprovider`** (needed by everything after; build it first).
- Telemetry skeleton: slog, `/metrics`, `/healthz`.

**Done when:** `make test` passes in CI; mock provider serves streamed and non-streamed completions with configurable faults.

### Phase 1 — Transparent proxy (weeks 1–3)

**Tasks**
- Ingress `/v1/chat/completions` with lazy parsing and `model` splice.
- OpenAI-compatible adapter (covers OpenAI, Azure, vLLM, Ollama, Groq, Together) and Anthropic adapter (request/response/stream mapping).
- SSE passthrough with the **first-token gate** and deferred headers.
- Virtual keys (hashed), admin-key auth, per-key RPM limit.
- Static routing: pinned `provider/model`, plus simple ordered fallback.
- Response headers (`x-conduit-*`).
- Golden-file tests for adapters using recorded fixtures; fuzz tests for the SSE parser.

**Acceptance criteria**
- Official OpenAI Python & JS SDKs work unmodified (streaming + non-streaming) against Conduit → mock.
- `test/k6/01_overhead.js` shows gateway added latency p99 < 5 ms at 1 000 rps on a laptop.
- No goroutine leaks (`goleak`) after 10 000 cancelled streams.

### Phase 2 — Reliability (weeks 3–5)

**Tasks**
- Stats package: EWMA, DDSketch, Wilson bound; snapshot publisher (`atomic.Pointer`).
- Circuit breakers, outlier ejection, provider-level breaker.
- Capacity governor (token buckets corrected by response headers, key pools).
- Executor: retry policy, error taxonomy, provider-diverse fallback, hedging with budget.
- Graceful shutdown that drains streams.

**Acceptance criteria**
- `test/k6/03_chaos.js`: with the preferred model at 60% errors for 60 s, request failure rate in the fault window < 0.5%; traffic returns to the preferred model within 30 s of healing.
- Hedging cuts p95 under injected tail latency (lognormal σ ≥ 0.8) by ≥ 25% at ≤ 6% extra requests.

### Phase 3 — Routing brain v1 (weeks 5–7)

**Tasks**
- Catalog (prices, context, capabilities, priors).
- Hard filters with recorded reject reasons; relaxation logic.
- Estimators (`Q̂`, `Ĉ`, `L̂`, `R̂`), `E[out]` per task class.
- Scorer, objective presets, Pareto flagging.
- Policy resolution (headers, aliases, CEL matching).
- Task classifier tiers 0–2 and `cmd/conduit-train`.
- `POST /v1/route` (decision-only) and `x-conduit-*` explain headers.
- `conduit simulate` CLI.

**Acceptance criteria**
- Property tests: hard constraints are **never** violated across 10⁶ randomized requests.
- Classifier macro-F1 ≥ 0.85 on `test/datasets` (grow the dataset to ≥ 500 items before relying on this number).
- `internal/router` benchmark: decision < 200 µs for 50 candidates.
- `test/k6/02_accuracy.js` passes.

### Phase 4 — Learning loop and Replay Lab (weeks 7–9)

**Tasks**
- Ledger writers (SQLite, async batch, ring buffer), retention.
- Guarded selection with propensity logging, seeded PRNG.
- Reward computation, `/v1/feedback`, validators, judge sampler (bounded worker pool), gold canaries.
- Beta posteriors with decay; checkpoint/restore.
- Replay Lab: IPS, SNIPS, DR; bootstrap CIs; ESS and coverage; CLI `conduit replay` and admin job API.
- Shadow policies (evaluate live, act nothing).

**Acceptance criteria**
- `test/k6/04_learning.js`: Conduit `balanced` reaches ≥ 95% of the accuracy of the always-best baseline at ≤ 60% of its cost after warm-up, in the simulated environment.
- Replay Lab unit test: on synthetic logs with known ground truth, DR estimate is within 3% of the true value with 95% CI coverage ≥ 90%.
- Regret in the simulator decreases over time (asserted in a Go test with fixed seed).

### Phase 5 — Control plane and UI (weeks 9–11)

**Tasks**
- Admin REST API + OpenAPI generation, RBAC, audit log, config versioning/rollback.
- SSE live event stream.
- Budgets (reserve/reconcile), soft/hard limits, downgrade behavior.
- Exact cache (+ semantic interfaces).
- UI per `ui/design.md`; embed via `go:embed`.
- Playwright smoke tests for critical UI flows.

**Acceptance criteria**
- Config change from UI takes effect < 1 s with no dropped requests (k6 running during change).
- Rollback restores previous behavior exactly (checked by decision-replay).

### Phase 6 — Packaging and release (weeks 11–12)

**Tasks**
- Dockerfile (distroless), Compose bundle (Conduit + mock + Prometheus + Grafana), Helm chart, goreleaser, SBOM, cosign.
- `conduit doctor`, `examples/`, quick-start and integration docs.
- Publish benchmark report (hardware, commands, raw JSON).
- Multi-node: Postgres config store, Redis budgets, stats delta merge (can slip to v1.1).

**Acceptance criteria**
- `docker compose up` gives a working system with dashboards in < 60 s.
- A new user integrates using only the README in < 10 minutes.

### MVP cut-line (if you have 2–3 weeks, e.g. for a hackathon)

Keep: Phase 1 (OpenAI adapter + one more), Phase 2 breakers + failover, Phase 3 filters + scorer + presets + rules-only classifier, ledger with explain, minimal UI (Overview, Decision Explorer, Routes), mock provider and k6 `01`–`03`.
Defer: Anthropic mid-stream resume, learning loop, Replay Lab, budgets, semantic cache, multi-node.
Demo story: *"Kill a provider live; watch traffic shift; click any request to see why."*

---

## 4. Testing strategy (summary; full detail in `TESTING.md`)

| Layer | Tooling | What it proves |
|---|---|---|
| Unit | `testing`, table tests | Scorer math, filters, estimators, breaker transitions |
| Property | `rapid` | Constraints never violated; propensities valid; Beta merge commutative |
| Golden/fixtures | Recorded provider payloads | Adapter mapping correct, incl. streams and tool calls |
| Fuzz | `go test -fuzz` | SSE parser, JSON scanner, config loader |
| Race/leak | `-race`, `goleak` | Concurrency safety of executor |
| Integration | Compose + mock provider | End-to-end behavior incl. auth, budgets, cache |
| Performance | k6 | Overhead, throughput, TTFT, memory |
| Accuracy | k6 + `conduit eval` | Task classification, constraint adherence, routing optimality |
| Chaos | k6 + mock control API | Failover, recovery, hedging |
| Usefulness | k6 (04) + Replay Lab | Cost/quality vs baselines |

## 5. Definition of done (per feature)

1. Unit + property tests; race detector clean.
2. Metrics and ledger fields added; documented in `DESIGN.md`.
3. Config schema updated and validated; example config updated.
4. Benchmarks unchanged or improved (CI compares `benchstat`).
5. Failure mode documented (what happens when this dependency fails).

## 6. Risk register (execution)

| Risk | Likelihood | Impact | Mitigation |
|---|---|---|---|
| Streaming edge cases across providers | High | High | Golden fixtures per provider; fuzzing; ship OpenAI-compatible + Anthropic first |
| Reward signal too noisy to learn | Medium | High | Ship routing value without learning (Phase 3); learning is additive |
| Overhead budget missed | Medium | Medium | Snapshot architecture from day one; benchmarks in CI |
| Scope creep (UI) | High | Medium | Build UI last, against a stable API; follow `ui/design.md` P0 list first |
| Provider API drift | High | Medium | Adapter contract tests using recorded fixtures + `conduit doctor` |
