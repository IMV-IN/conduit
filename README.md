# Conduit

**A single-binary Go AI gateway that picks the right model for every request, learns from outcomes, fails over safely, and can explain and replay every decision.**

You say *what you want* (`auto:cheap`, `auto:fast`, `auto:best`, or your own policy). Conduit decides *which model*, using latency, cost, context length, availability, capabilities, and task type.

> **Status: v0 gateway + full harness.** This repo now contains a working
> gateway (proxy, routing brain v0, learning loop, failover/hedging,
> ledger, admin API) plus the design docs, k6 suites, mock provider, dataset
> tooling and release pipeline. See `docs/DESIGN.md` for the full vision and
> `docs/IMPLEMENTATION_PLAN.md` for what is implemented vs. phased.

## Why another gateway?

Most gateways route with static fallback lists and weights. Conduit adds:

1. **Intent-based routing.** Hard constraints (context, capabilities, residency, budget) → utility scoring → guarded exploration.
2. **Closed-loop learning.** Per-(task, model) quality posteriors updated from feedback, validators, and sampled judging.
3. **Streaming-aware reliability.** First-token gating, safe failover, hedging, rate-limit-aware capacity governor.
4. **Decision Ledger + Replay Lab.** Every decision is logged with an exact propensity, so you can evaluate a *new* policy on *old* traffic offline.
5. **Three ways to adopt.** Full proxy, Go library (`pkg/router`), or decision-only sidecar (`POST /v1/route`).

## Quick start

```bash
docker compose -f deploy/docker-compose.yml up -d --build
# data plane :8080, admin :8081, mock :9001
curl localhost:8080/healthz
```

```python
from openai import OpenAI
client = OpenAI(base_url="http://localhost:8080/v1", api_key="sk-conduit-dev")
r = client.chat.completions.create(model="auto:cheap",
        messages=[{"role": "user", "content": "Summarize this ticket…"}])
print(r.choices[0].message.content)
```

Local build without Docker:

```bash
go build -o bin/conduit ./cmd/conduit
go run ./test/mockprovider -addr :9001 &
CONDUIT_ADMIN_KEY=admin-dev CONDUIT_DEV_KEY=sk-conduit-dev \
  ./bin/conduit serve --config conduit.example.yaml
```

More examples: `examples/` (Python, TypeScript, curl, decision-only sidecar).

## Repo layout

```
conduit/
├── cmd/conduit/        serve | validate | simulate | doctor | version | healthcheck
├── pkg/router/         public Go library (adoption mode 2)
├── internal/           server, router, classify, stats, health, provider,
│                       ledger, config, telemetry (+ Phase 3–5 extension points:
│                       budget, cache, replay, learn, admin, policy, catalog, exec)
├── ui/                 console (design.md now; React app in Phase 5)
├── deploy/             Dockerfile, compose, Helm stub, Prometheus, Grafana dashboard
├── test/k6/            5 suites (overhead, accuracy, chaos, learning, soak)
├── test/mockprovider/  fault-injecting OpenAI-compatible mock
├── test/datasets/      seed tasks.jsonl + fetcher for public samples
├── tools/datasets/     HF/Kaggle fetch script (see README there)
├── docs/               DESIGN.md, IMPLEMENTATION_PLAN.md, TESTING.md
├── examples/           python / ts / curl
└── conduit.example.yaml
```

## Testing

| Layer | Command | What it proves |
|---|---|---|
| Unit + property | `make test` | Scorer math, guarded selection, Beta decay/merge, breakers, classifier fixture |
| Fast gate | `make test-short` | Same without race detector (PR default in CI) |
| Perf/accuracy/chaos | `make e2e` | k6 01 (short) + 02 + 03(errors) against compose stack |
| Full matrix | `make k6-all`, `make k6-soak` | Nightly; publishes raw JSON artifacts |
| Overhead gate | `make verify` | Fails CI if added p99 ≥ 5 ms / p50 ≥ 1.5 ms |

Full guide, pass criteria and honest-reading notes: `docs/TESTING.md`.
What is verified vs. targeted: `docs/TESTING.md §7`.

### Datasets

Seed set `test/datasets/tasks.jsonl` (32 rows) ships in-repo. Grow it with:

```bash
pip install -r tools/datasets/requirements.txt
python3 tools/datasets/fetch.py --out test/datasets --max-per-source 200
```

Needs nothing by default; `HF_TOKEN` (https://huggingface.co/settings/tokens)
and `KAGGLE_USERNAME`/`KAGGLE_KEY` (https://www.kaggle.com/settings/account)
unlock more rows. Details: `tools/datasets/README.md`. Copy `.env.example`
to `.env` for local secrets; CI uses repository secrets of the same names.

## Configuration

`conduit validate --config conduit.example.yaml` checks your file.
Prices and quality priors in the example are **illustrative** — fill them from
your providers. Env expansion: `${env:VAR}`, `${file:/run/secrets/x}`.

## Releases

Tag `v*.*.*` → GoReleaser builds static binaries (linux/darwin, amd64/arm64)
+ SBOMs, and pushes multi-arch images to Docker Hub and GHCR.
Setup (one time): set repository secrets `DOCKERHUB_USERNAME`,
`DOCKERHUB_TOKEN`, and variable `DOCKERHUB_REPO` (see `release.yml`).
Run locally: `make goreleaser-snapshot`.

## Documents

| File | Contents |
|---|---|
| `docs/DESIGN.md` | Architecture, routing math, reliability, learning, replay, API, security, scaling |
| `docs/IMPLEMENTATION_PLAN.md` | Repo layout, code skeletons, phased plan, MVP cut |
| `docs/TESTING.md` | Harness guide, pass criteria, verified-vs-targeted |
| `ui/design.md` | Console design: screens, components, system, build order |
| `conduit.example.yaml` | Config matching the test harness |

## License

Apache-2.0 (see LICENSE).
