// 01_overhead.js — How much latency does Conduit add?
//
// The mock provider is set to answer in ~1 ms with no token delay, so what we measure is
// (almost) pure gateway cost. Three phases run sequentially at the same arrival rate:
//   direct : client -> mock                         (baseline)
//   pinned : client -> Conduit (pinned model)       (transport + auth + parsing + translate)
//   auto   : client -> Conduit (model=auto:fast)    (the above + classify + route + ledger)
//
// overhead = gateway - direct. Written to test-results/overhead.json for CI gating (`make verify`).
//
// Run:  k6 run test/k6/01_overhead.js   (env: RATE=500 DUR=30s)
import http from 'k6/http';
import { check } from 'k6';
import { Trend } from 'k6/metrics';
import { BASE, MOCK, headers, mockProfile, mockReset } from './lib.js';

const RATE = Number(__ENV.RATE || 500);
const DUR = __ENV.DUR || '30s';
const GAP = 5; // seconds between phases

const direct = new Trend('direct_ms', true);
const pinned = new Trend('pinned_ms', true);
const auto = new Trend('auto_ms', true);

function phase(name, startSec, fn) {
  return {
    executor: 'constant-arrival-rate',
    rate: RATE,
    timeUnit: '1s',
    duration: DUR,
    preAllocatedVUs: Math.max(20, Math.ceil(RATE / 10)),
    maxVUs: Math.max(200, RATE),
    startTime: `${startSec}s`,
    exec: fn,
    tags: { phase: name },
  };
}

const durSec = parseInt(DUR, 10);

export const options = {
  scenarios: {
    direct: phase('direct', 0, 'runDirect'),
    pinned: phase('pinned', durSec + GAP, 'runPinned'),
    auto: phase('auto', 2 * (durSec + GAP), 'runAuto'),
  },
  thresholds: {
    // Absolute sanity thresholds (laptop-class hardware). Overhead gating is done via overhead.json.
    pinned_ms: ['p(99)<50'],
    auto_ms: ['p(99)<50'],
    'http_req_failed{phase:pinned}': ['rate<0.001'],
    'http_req_failed{phase:auto}': ['rate<0.001'],
  },
  summaryTrendStats: ['min', 'med', 'p(90)', 'p(95)', 'p(99)', 'max', 'avg'],
};

export function setup() {
  const r = mockProfile('fast', {
    ttft_median_ms: 1, ttft_sigma: 0, tokens_per_sec: 0, output_tokens: 16, error_rate: 0,
  });
  check(r, { 'mock configured': (x) => x.status === 204 });
}

export function teardown() {
  mockReset();
}

const body = (model) =>
  JSON.stringify({
    model,
    messages: [{ role: 'user', content: 'Summarize: the quick brown fox jumps over the lazy dog.' }],
    max_tokens: 16,
  });

export function runDirect() {
  const r = http.post(`${MOCK}/v1/chat/completions`, body('fast'), {
    headers: { 'Content-Type': 'application/json' },
    tags: { phase: 'direct' },
  });
  check(r, { 'direct 200': (x) => x.status === 200 });
  direct.add(r.timings.duration);
}

export function runPinned() {
  const r = http.post(`${BASE}/v1/chat/completions`, body('mock/fast'), {
    headers: headers({ 'x-conduit-pin': 'strict' }),
    tags: { phase: 'pinned' },
  });
  check(r, { 'pinned 200': (x) => x.status === 200 });
  pinned.add(r.timings.duration);
}

export function runAuto() {
  const r = http.post(`${BASE}/v1/chat/completions`, body('auto:fast'), {
    headers: headers(),
    tags: { phase: 'auto' },
  });
  check(r, {
    'auto 200': (x) => x.status === 200,
    'has decision id': (x) => !!x.headers['X-Conduit-Decision-Id'],
  });
  auto.add(r.timings.duration);
}

export function handleSummary(data) {
  const v = (m, k) => (data.metrics[m] && data.metrics[m].values[k]) || NaN;
  const out = {
    rate_rps: RATE,
    direct: { p50: v('direct_ms', 'med'), p95: v('direct_ms', 'p(95)'), p99: v('direct_ms', 'p(99)') },
    pinned: { p50: v('pinned_ms', 'med'), p95: v('pinned_ms', 'p(95)'), p99: v('pinned_ms', 'p(99)') },
    auto: { p50: v('auto_ms', 'med'), p95: v('auto_ms', 'p(95)'), p99: v('auto_ms', 'p(99)') },
  };
  out.pinned_overhead_p50_ms = out.pinned.p50 - out.direct.p50;
  out.pinned_overhead_p99_ms = out.pinned.p99 - out.direct.p99;
  out.overhead_p50_ms = out.auto.p50 - out.direct.p50;
  out.overhead_p99_ms = out.auto.p99 - out.direct.p99;
  out.routing_only_p50_ms = out.auto.p50 - out.pinned.p50;

  const f = (x) => (Number.isFinite(x) ? x.toFixed(2) : 'n/a');
  const text = [
    '',
    `Overhead @ ${RATE} rps (ms)         p50      p99`,
    `  direct (baseline)            ${f(out.direct.p50).padStart(7)}  ${f(out.direct.p99).padStart(7)}`,
    `  pinned via Conduit           ${f(out.pinned.p50).padStart(7)}  ${f(out.pinned.p99).padStart(7)}`,
    `  auto   via Conduit           ${f(out.auto.p50).padStart(7)}  ${f(out.auto.p99).padStart(7)}`,
    `  => added by Conduit (auto)   ${f(out.overhead_p50_ms).padStart(7)}  ${f(out.overhead_p99_ms).padStart(7)}`,
    `  => routing decision alone    ${f(out.routing_only_p50_ms).padStart(7)}`,
    '',
  ].join('\n');

  // RESULTS_DIR lets container runs (compose k6 service, CI) redirect file
  // output to the mounted /results volume. Local runs default to test-results/.
  const outPath = `${__ENV.RESULTS_DIR || 'test-results'}/overhead.json`;
  return { stdout: text, [outPath]: JSON.stringify(out, null, 2) };
}
