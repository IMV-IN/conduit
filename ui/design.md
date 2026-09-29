# Conduit Console — Frontend Design (`design.md`)

The Console is the embedded web UI for **inspecting** what Conduit is doing and **configuring** it. It is a thin client over the Admin API (`:8081`). Anything the UI can do, `curl` can do; the UI never has private endpoints.

---

## 1. Purpose and principles

| # | Principle | Consequence |
|---|---|---|
| 1 | **Explain, don't just show.** | Every request, every routing choice, every breaker trip links to *why*. |
| 2 | **Safe by construction.** | Config edits are dry-run, diffed, validated, versioned and reversible before they touch traffic. |
| 3 | **API-first parity.** | UI = Admin API. Each screen shows the equivalent `curl` / YAML for what you're viewing or changing. |
| 4 | **Live but calm.** | Streaming data, but never jumpy: pausable, rate-limited redraws, stable ordering. |
| 5 | **Operator-dense, newcomer-readable.** | Dense tables and keyboard shortcuts for operators; empty states and inline docs for first-timers. |
| 6 | **Zero-install.** | Static assets embedded with `go:embed`, served at `/console`. No CDN, no telemetry, works air-gapped. |

### Users

| Persona | Needs | Primary screens |
|---|---|---|
| **App developer** | "Why did my request go to that model? Why is it slow/expensive?" | Playground, Decision Explorer |
| **Platform/SRE** | Health, incidents, capacity, safe rollouts | Overview, Health, Routing, Experiments |
| **FinOps / owner** | Spend, savings, budgets, per-key usage | Cost & Budgets, Keys |
| **ML/quality owner** | Is routing actually good? Compare policies. | Quality, Replay Lab, Experiments |

---

## 2. Information architecture

```
Console
├── Overview                 (home: live health, traffic, cost, savings)
├── Traffic
│   ├── Live                 (real-time request stream)
│   └── Decisions            (search + Decision Explorer)
├── Routing
│   ├── Policies             (list, editor, simulate)
│   ├── Models & Providers   (catalog, prices, capabilities, priors)
│   └── Task Classes         (classifier stats, misclassification review)
├── Reliability
│   ├── Health               (breakers, capacity, incidents)
│   └── Experiments          (shadow, canary, A/B)
├── Quality
│   ├── Posteriors           (per task × model)
│   ├── Feedback & Judge     (signal sources, judge config)
│   └── Replay Lab           (offline policy evaluation)
├── Cost & Budgets
├── Access
│   ├── Virtual Keys
│   └── Admin Users & Audit
├── Playground               (send a request, see full explanation)
└── Settings                 (config versions, storage, retention, about)
```

Global chrome: left nav (collapsible), top bar (time range, environment badge, config version chip, `⌘K` command palette, theme toggle), toast area, "Unsaved changes" banner.

---

## 3. Screens

Each screen lists: **purpose**, **layout**, **data source**, **key interactions**, **states**.

### 3.1 Overview

**Purpose:** answer "is everything OK and is it saving money?" in 5 seconds.

```
┌──────────────────────────────────────────────────────────────────────────────┐
│ Overview                       [Last 1h ▾]   ● Live         config v42 ▾     │
├─────────────┬─────────────┬─────────────┬─────────────┬──────────────────────┤
│ Requests/s  │ Success     │ p95 latency │ Spend today │ Saved vs "best"      │
│ 412  ▁▂▅▇▆  │ 99.87% ▁▁▁▂ │ 1.9 s ▂▃▂▂  │ $84.20      │ $131.60 (61%)  ▁▃▅▇  │
├─────────────┴─────────────┴─────────────┴─────────────┴──────────────────────┤
│ Traffic by model (stacked area)                │ Provider health              │
│  ████████████ premium / balanced / fast        │  openai     ● healthy  99.9% │
│                                                │  anthropic  ◐ degraded 96.1% │
│                                                │  local      ● healthy 100%   │
├────────────────────────────────────────────────┼──────────────────────────────┤
│ Routing mix by task class (heatmap)            │ Active incidents & events    │
│  code · math · chat · rag · …  × models        │  10:41 breaker OPEN anthropic│
│                                                │  10:39 hedge rate ↑ 8%       │
└────────────────────────────────────────────────┴──────────────────────────────┘
```

- **Data:** `GET /admin/v1/stats/summary?range=1h`, SSE `/admin/v1/stream/events` for the live badge and event list.
- **Interactions:** click any KPI → drill to Traffic with filters applied; click a provider → Health; click a heatmap cell → Decisions filtered by (task, model).
- **"Saved vs best"** is a *counterfactual estimate*; the tooltip states the method (same tokens priced on the `best` baseline model) and links to Replay Lab.
- **States:** no traffic → onboarding card with a copy-paste `curl` and SDK snippet; degraded → banner with direct link to the offending breaker.

### 3.2 Traffic → Live

Real-time table (virtualized), newest first, pausable.

| Column | Notes |
|---|---|
| Time, Key, Policy | Policy chip colored by objective |
| Task (conf) | e.g. `code 0.93`; amber below threshold |
| Chosen model | Provider icon + name; `↺` if failed over, `⚡` if hedged, `🧪` if explored, `💾` cache hit |
| TTFT / Total | Color scaled vs the model's p50 |
| Tokens in/out, Cost | |
| Status | 2xx / relaxed / error class |

Filters: key, policy, model, task, status, explored-only, failover-only, latency > X. Row click → Decision Drawer (§3.3). Pause button freezes the view while the buffer keeps filling; "N new" pill returns to live.

### 3.3 Decision Explorer (the signature feature)

Opened as a right-side drawer from any request or as a full page at `/console/decisions/:id` (shareable link).

```
Decision d_9f2c…   2026-09-29 10:41:07   key: dev   policy: default (v42)
──────────────────────────────────────────────────────────────────────────
Request      task=code (0.93)  in≈1,840 tok  tools=no  stream=yes  max_out=512
Outcome      → premium   TTFT 812 ms   total 3.1 s   cost $0.0231   ✓ 200
Attempts     1. premium ✓            (no failover, no hedge)

Why this model?                                   [Table] [Pareto] [Raw JSON]
┌───────────┬──────┬────────┬───────┬──────┬───────┬──────────┬───────────────┐
│ Model     │  Q̂   │ Cost   │ Lat   │ Risk │   U   │ p(select)│ Status        │
├───────────┼──────┼────────┼───────┼──────┼───────┼──────────┼───────────────┤
│▶ premium  │ 0.94 │ $0.023 │ 3.0 s │ 0.02 │ 0.412 │  0.81    │ CHOSEN ★      │
│  balanced │ 0.86 │ $0.004 │ 2.1 s │ 0.02 │ 0.388 │  0.15    │ in guard set  │
│  fast     │ 0.58 │ $0.001 │ 1.2 s │ 0.03 │ 0.201 │  0.02    │ ε-floor only  │
│  tiny     │  –   │   –    │  –    │  –   │  –    │  –       │ ✗ context 4k  │
│  notools  │  –   │   –    │  –    │  –   │  –    │  –       │ ✗ open breaker│
└───────────┴──────┴────────┴───────┴──────┴───────┴──────────┴───────────────┘
Score breakdown (chosen):  +0.47 quality  −0.10 cost  −0.03 latency  −0.002 risk
Reward       1.0 (feedback)   sources: feedback ✓  validator –  judge –
[ Replay this decision with… ▾ ]   [ Open in Playground ]   [ Copy as curl ]
```

Components:
- **Candidate table** (default): sortable; rejected models greyed with a reason chip; hover on any cell shows its estimator inputs (e.g. "TTFT p50 640 ms from 1,204 samples, shrinkage 0.96").
- **Pareto view:** scatter of Cost (x, log) vs Quality (y) with bubble size = latency; frontier line; chosen model ringed; dominated models faded.
- **Score waterfall:** stacked bar showing each term's contribution for chosen vs runner-up.
- **What-if:** change objective/weights/constraints in a side form → recompute the ranking client-side from the logged estimates (the same math as the server), showing which model would win. No traffic is sent.
- **Timeline** of attempts with timings (connect, TTFT, stream), hedge launch markers, breaker verdicts.
- **Determinism badge:** "Reproducible (seed d_9f2c)" — the server can re-run selection exactly.

Data: `GET /admin/v1/decisions/{id}` (full ledger record incl. all candidates).

### 3.4 Traffic → Decisions (search)

Faceted search over the ledger with saved views: task, model, policy, key, time, cost > X, latency > X, relaxed, explored, failover, reward < X. Result table mirrors Live plus reward and "misroute suspicion" (high-cost model chosen for a `classification_short` task, low-confidence classification, etc.). Export: `CSV`, `JSONL` (`GET /admin/v1/decisions:export`).

### 3.5 Routing → Policies

Two-pane editor.

- **Left:** policy list (name, match rule, objective, traffic share last 24h, config version touched).
- **Right:** tabs — **Form** and **YAML**, always in sync.
  - Form: objective preset selector with live weight sliders (four sliders constrained to sum 1, shown as a stacked bar), constraint inputs (`min_quality`, `max_latency_ms`, `max_cost_usd`), allow/deny model pickers (glob-aware with matched-models preview), regions, exploration (ε, δ, τ with plain-language helper: "How far from the best score may exploration stray?"), hedging, reliability, budget behavior, reward sources.
  - YAML: CodeMirror 6 with JSON-Schema-powered autocomplete, inline validation, CEL expression highlighting and a **CEL tester** (paste a sample request JSON → shows match true/false and the evaluated sub-expressions).
- **Simulate panel** (bottom): pick a sample set (last 500 real requests, or a saved test set) → shows how the *edited* policy would route them: distribution by model, estimated cost delta, latency delta, constraint violations (must be zero), infeasible count. Backed by `POST /admin/v1/config:simulate`.
- **Apply flow:** `Validate` → `Simulate` → **Diff** (side-by-side YAML, semantic summary "objective balanced → cheap; +1 deny glob") → choose rollout: *Apply now*, *Canary N% for M minutes with auto-rollback on SLO breach*, or *Shadow only*. Every apply creates a config version with author + comment.

### 3.6 Routing → Models & Providers

Table of models: provider, id, context, output cap, capabilities (chips), price in/out, latency prior, live p50/p95 TTFT, TPS, error rate, breaker state, key headroom bar.

Detail drawer: edit price/priors; **Test model** button (sends a tiny request via the provider adapter, shows latency and response); provider key pool status (masked keys, remaining tokens/requests from headers, cool-down timers); "Prior vs learned" sparkline per task class so users see when evidence overtook the prior.

### 3.7 Routing → Task Classes

Confusion-style matrix built from ledger items that have a trusted label (explicit task header or validator/feedback-corrected). Lists low-confidence prompts (redacted per `content_logging`) with a "relabel" action that writes a labeled example for `conduit-train`. Shows tier hit rates (explicit / rules / model / plug-in) and classifier latency.

### 3.8 Reliability → Health

- **Breaker board:** one card per model/provider key: state (`Closed/Open/HalfOpen`), trip count, last error class, next probe time, rolling error rate sparkline, latency ejection status. Manual actions (RBAC `operator`): *Force open*, *Force close*, *Drain*, *Probe now*.
- **Capacity:** stacked bars of RPM/TPM headroom per provider key, with 429 counters.
- **Incident timeline:** breaker transitions, hedge spikes, budget events, config changes, on one time axis so causes and effects line up.
- **Failure taxonomy chart:** retryable / context / auth / client / refusal by model.

### 3.9 Reliability → Experiments

Create shadow, canary or A/B experiments comparing policy A vs B.

- Setup wizard: choose policy, traffic %, target filter (key/task), duration, guardrail SLOs (error rate, p95, cost per request), auto-rollback toggle.
- Results view: side-by-side metrics with confidence intervals; verdict banner ("B is 31% cheaper, quality indistinguishable — 87% probability") and an explicit "insufficient evidence" state.
- One-click *Promote* creates a config version.

### 3.10 Quality → Posteriors

Grid: rows = task classes, columns = models; each cell shows mean quality with a colored uncertainty band and sample count; cell hover shows Beta(α, β), prior contribution, last update. Toggle *Prior / Learned / Blend*. Drift alerts (posterior mean dropped > X in 24h) are pinned at top with a link to the affected decisions.

### 3.11 Quality → Feedback & Judge

- Signal mix donut (feedback / validators / implicit / judge / canary) per task class.
- Judge configuration: model, sample rate, rubric per task, monthly cap, judge spend; a **calibration chart** (judge score vs human feedback where both exist) with correlation and a warning if weak.
- Gold canary prompts editor (prompt, expected-answer matcher, schedule).
- Feedback API quick-start snippet.

### 3.12 Quality → Replay Lab

```
1  Choose logs      [Last 7 days ▾] [task: any ▾] [key: any ▾]        n = 184,203
2  Choose policy    [ current (v42) ]  vs  [ candidate: cheap-v2 ▾ ]  (edit YAML…)
3  Estimator        (●) Doubly robust  ( ) SNIPS  ( ) IPS     clip weights ≤ [20]
                                                             [ Run replay ]
Results
              current        candidate       Δ (95% CI)
 Reward       0.912          0.905           −0.007 (−0.021, +0.008)
 Cost / req   $0.0052        $0.0031         −40.4% (−41.0, −39.8)   ← exact
 p95 latency  2.4 s          2.1 s           −12%   (−17, −7)
 Effective sample size 9,420 · support coverage 100% · clipped weights 0.3%
 Verdict: candidate is cheaper; quality difference not significant.
```

The UI **must** show ESS and coverage and switch the verdict to "Not enough evidence" when ESS is below a threshold; it never presents a point estimate without its interval. Results can be saved, compared, and turned into an Experiment or an applied config in one click.

### 3.13 Cost & Budgets

Spend over time by key/team/model/task; **Savings ledger** (cache savings, routing savings vs `best`, hedge waste as a negative); budget bars with soft/hard thresholds and projected exhaustion date; edit budgets inline; "what happens at soft limit" preview showing the downgrade policy.

### 3.14 Access → Virtual Keys

Table (id, tenant, scopes, default policy, RPM/TPM, budget, last used, status). Create-key dialog shows the secret **once** with a copy button and a ready-made SDK snippet. Rotate (with overlap window), revoke, set expiry. Keys are never displayed again; only prefix + last 4.

### 3.15 Access → Admin Users & Audit

Admin keys/SSO mapping (OIDC optional), roles (`viewer`, `operator`, `admin`), and an immutable audit log (who changed what, config version diff link, source IP).

### 3.16 Playground

Left: request builder (message editor, model/policy selector, objective override, constraints, tools JSON, stream toggle, explore off/on, cache allow). Right: streamed response plus a live **Explanation panel** (same components as Decision Explorer) that fills in as headers and decision data arrive. Buttons: *Copy as curl / Python / TS*, *Compare policies* (fires the same prompt through 2–4 policies in parallel and shows outputs, cost, and latency side by side; clearly labeled as spending real money).

### 3.17 Settings

Config versions (list, diff any two, rollback), storage and retention, content-logging mode with a plain-language privacy statement, cluster status (nodes, state-sync lag), About (version, build, Go version, links), theme/density.

---

## 4. Cross-cutting UX patterns

### 4.1 Safe configuration lifecycle

```
Edit ──► Validate (schema+CEL) ──► Simulate (on real traffic sample)
      ──► Diff ──► Rollout choice (now / canary / shadow) ──► Version created
      ──► Watch (auto-rollback guardrails) ──► Rollback anytime (1 click)
```

- Unsaved changes indicator; navigation guard.
- Dangerous changes (deleting a policy in use, allow-list that leaves zero feasible models for live traffic) require typing the policy name to confirm and show the blast radius ("affects 61% of traffic in the last hour").
- Optimistic concurrency: config carries an `etag`; a conflicting edit shows a three-way merge view.

### 4.2 Equivalent-API panel

Every screen has an "API" toggle showing the exact request(s) it uses (`GET /admin/v1/...`) as curl and the YAML fragment for config screens. This is the parity guarantee and also the best documentation.

### 4.3 Live data

- One shared SSE connection (`/admin/v1/stream/events`) multiplexed to components through a client-side event bus; auto-reconnect with `Last-Event-ID`; visible connection state chip.
- UI redraw throttled to ≤ 4 Hz; charts use fixed windows; tables freeze on hover.
- Server-side sampling above N events/s ("showing 1 of 10 · click for exact stats").

### 4.4 Empty, loading, error states

| State | Treatment |
|---|---|
| First run | Checklist: add provider → add model → create key → send test request; each step deep-links |
| Loading | Skeletons matching final layout; never spinners alone |
| No data in range | Explains why (no traffic / filters / retention) and offers the fix |
| API error | Inline error with request ID and "copy debug info"; retry with backoff |
| Permission | Feature visible-but-disabled with tooltip naming the required role |

### 4.5 Keyboard and command palette

`⌘/Ctrl-K` palette (jump to screen, search decision by ID, search model/policy/key); `g o` overview, `g t` traffic, `g p` policies, `g h` health; `/` focus search; `j/k` move in tables; `Enter` open drawer; `Esc` close; `?` cheatsheet.

### 4.6 Privacy in the UI

Prompt/response content is shown only if the server's `content_logging` allows; otherwise the UI shows "content not stored (metadata mode)" instead of hiding the panel. Client never persists content in `localStorage`. A "Redact" toggle blurs content for screen-sharing.

---

## 5. Design system

### 5.1 Visual language

Calm, technical, high-contrast. Dark theme default (operators live in it), light theme available; follows `prefers-color-scheme`.

**Color tokens** (CSS variables; semantic first)

| Token | Dark | Light | Use |
|---|---|---|---|
| `--bg` | `#0B0F14` | `#F7F8FA` | App background |
| `--surface` | `#121821` | `#FFFFFF` | Cards, tables |
| `--surface-2` | `#1A2230` | `#F0F2F6` | Hover, subtle panels |
| `--border` | `#263043` | `#DCE1E9` | Dividers |
| `--text` | `#E6EAF0` | `#141A22` | Primary text |
| `--text-muted` | `#93A0B4` | `#5B6778` | Secondary text |
| `--accent` | `#5B8CFF` | `#2F5BE0` | Primary actions, selection |
| `--ok` | `#3DDC97` | `#12805A` | Healthy, success |
| `--warn` | `#F5B841` | `#9A6700` | Degraded, low confidence |
| `--danger` | `#FF6B6B` | `#C62828` | Errors, open breakers |
| `--info` | `#4CC9F0` | `#0B7BA8` | Explored/experimental markers |

Categorical palette for models/providers: 8 colorblind-safe hues (Okabe-Ito based), assigned deterministically by model ID hash and consistent across all charts. **Never rely on color alone**: state chips include an icon and text.

**Typography**

- UI: `Inter`, system fallback stack (bundled locally, no CDN).
- Numbers/IDs/code: `JetBrains Mono`, tabular figures on all metrics.
- Scale: 12 / 13 / 14 (base) / 16 / 20 / 24 / 32. Line-height 1.4–1.5. Dense tables use 13px.

**Spacing and shape:** 4px grid; radii 6 (controls), 10 (cards); 1px borders over shadows; elevation only for drawers and popovers.

**Motion:** 120–180 ms ease-out for drawers and popovers; charts animate on first load only; honor `prefers-reduced-motion`.

### 5.2 Core components

`AppShell`, `NavRail`, `TopBar`, `TimeRangePicker`, `KpiCard` (value + sparkline + delta), `DataTable` (virtualized, sortable, column chooser, sticky header, row density), `FilterBar` (typed chips, saved views), `Drawer`, `StateChip` (`Closed/Open/HalfOpen`, `Healthy/Degraded`), `ModelBadge` (provider icon + name + color), `TaskBadge` (with confidence), `CandidateTable`, `ParetoPlot`, `ScoreWaterfall`, `Heatmap`, `PosteriorCell`, `WeightSliders` (sum-to-one stacked), `YamlEditor` (CodeMirror 6 + schema), `CelTester`, `DiffViewer`, `ConfirmByTyping`, `CopyableCode` (with curl/Python/TS tabs), `Toast`, `EmptyState`, `Skeleton`, `CommandPalette`, `EventBusProvider`.

### 5.3 Data visualization rules

- Time-series: line for rates/latency, stacked area for shares, sparkline in KPIs; shared crosshair across charts on a page.
- Log scale toggle for cost and latency axes (cost spans decades).
- Always label units; latency axes show p50/p95/p99 series distinctly (dash patterns, not just color).
- Uncertainty shown as bands or error bars wherever the value is an estimate (posteriors, replay results).
- Chart data tables are available via a "View data" toggle (accessibility and copy-out).

---

## 6. Technical architecture

| Concern | Decision |
|---|---|
| Framework | React 18 + TypeScript, Vite build |
| Routing | React Router (data routers); URL is the source of truth for filters/time range so every view is shareable |
| Server state | TanStack Query (caching, retries, background refetch) |
| Client/live state | Zustand for UI prefs; a small typed event-bus over one SSE connection |
| Styling | Tailwind CSS with the CSS-variable tokens above; Radix UI primitives for accessible dialogs/popovers/menus |
| Charts | `uPlot` for high-frequency time-series (performance); `visx` for Pareto/heatmap/waterfall |
| Tables | TanStack Table + `@tanstack/react-virtual` |
| Editor | CodeMirror 6 (YAML + JSON Schema completion, CEL grammar) |
| Diff | `diff` + custom side-by-side renderer (YAML-semantic summary computed from parsed docs) |
| API types | Generated from the server's OpenAPI 3.1 (`openapi-typescript`) + typed client; no hand-written DTOs |
| Validation | Server is authoritative; client uses generated JSON Schema (Ajv) for instant feedback |
| Auth | Admin key or OIDC session cookie (SameSite=Strict, HttpOnly); RBAC role from `GET /admin/v1/me` gates controls |
| i18n | English first; strings externalised (`react-intl`-compatible) |
| Delivery | `vite build` → `ui/dist` → `go:embed` → served at `/console` with immutable hashed assets and SPA fallback; CSP: `default-src 'self'`, no inline script, no external hosts |
| Bundle budget | < 350 KB gzip initial route; Monaco *not* used (CodeMirror is smaller); heavy views lazy-loaded |
| Dev | `npm run dev` proxies to a local Conduit at `:8081`; MSW mocks for offline UI development |

### 6.1 Data contract per screen

| Screen | Endpoints / streams |
|---|---|
| Overview | `GET /admin/v1/stats/summary`, `GET /admin/v1/health`, SSE events |
| Live | SSE `/admin/v1/stream/events?types=request` |
| Decisions | `GET /admin/v1/decisions?…`, `GET /admin/v1/decisions/{id}`, `:export` |
| Policies | `GET/PUT /admin/v1/config`, `POST /config:validate`, `POST /config:simulate`, `GET /config/versions` |
| Models & Providers | `GET /admin/v1/models`, `GET /admin/v1/providers`, `POST /admin/v1/models/{id}:test` |
| Task Classes | `GET /admin/v1/stats/classifier` |
| Health | `GET /admin/v1/health`, `POST /admin/v1/breakers/{id}:force-open|force-close|probe` |
| Experiments | `GET/POST /admin/v1/experiments`, `GET /experiments/{id}/results` |
| Posteriors | `GET /admin/v1/stats/quality?task=…` |
| Replay Lab | `POST /admin/v1/replay`, `GET /admin/v1/replay/{id}` |
| Budgets | `GET/PUT /admin/v1/budgets`, `GET /admin/v1/stats/cost` |
| Keys | `GET/POST/DELETE /admin/v1/keys`, `POST /keys/{id}:rotate` |
| Playground | `POST /v1/chat/completions` (data plane) + `GET /admin/v1/decisions/{id}` |

### 6.2 Performance requirements

- First contentful paint < 1.2 s on a mid laptop over localhost/LAN.
- Live table sustains 200 events/s with virtualization and 4 Hz batching; UI thread stays < 50 ms per frame.
- Decision drawer opens < 150 ms (prefetch on row hover).
- Decisions search returns < 1 s for 1 M ledger rows with indexed facets (server responsibility; UI shows progress and allows cancel).

---

## 7. Accessibility and quality bar

- WCAG 2.2 AA: contrast ≥ 4.5:1 for text (tokens above chosen accordingly), visible focus rings, full keyboard operation, ARIA roles for tables, tabs, dialogs, live regions for toasts and connection state.
- Charts have text alternatives and a data-table view; no information encoded by color only.
- Responsive: desktop-first (≥ 1024 px); tablet collapses the nav and drawers become full-screen; phone gets a read-only Overview + Health + Decision view (editing disabled with an explanation).
- Testing: Playwright end-to-end for critical flows (first run, create key, edit policy → simulate → apply → rollback, decision explanation, kill a provider and watch Health), axe-core checks in CI, visual regression on the design-system storybook, contract tests against the generated OpenAPI client.

---

## 8. Build order (aligned with `IMPLEMENTATION_PLAN.md`)

| Priority | Deliverable | Why first |
|---|---|---|
| **P0** | App shell, auth, generated API client, Overview (KPIs + provider health) | Proves the loop end-to-end |
| **P0** | Decisions search + **Decision Explorer** (table, waterfall) | The core differentiator; also the best debugging tool |
| **P0** | Live traffic table | Demo value: kill a provider, watch traffic shift |
| **P0** | Models & Providers (read-only) + Health board | Operators need visibility before control |
| **P1** | Policy editor (YAML + form) with validate → simulate → diff → apply, config versions, rollback | Safe configuration |
| **P1** | Playground, Virtual Keys, Budgets | Day-2 essentials |
| **P2** | Pareto view, What-if, Posteriors, Feedback & Judge | Quality story |
| **P2** | Replay Lab, Experiments | Advanced differentiator |
| **P3** | Task Classes review, Audit/OIDC, i18n, mobile read-only | Polish |

### MVP screen cut (2–3 week build)

Overview · Live · Decision Explorer · Health · Policies (YAML only, validate + apply) · Playground.

---

## 9. Open questions for the design review

1. Should canary auto-rollback SLOs be defined per experiment or inherited from policy constraints?
2. How much prompt content should the Decision Explorer show by default when `content_logging: hashed`? (Current answer: none; show length and hash.)
3. Do we expose the Replay Lab to `operator` role or keep it `admin` only, given it can read historical (possibly sensitive) records?
4. Is a multi-environment switcher (dev/stage/prod Conduit instances behind one Console) in scope for v1? (Current answer: no; one Console per deployment.)
