// 03_chaos.js — Does Conduit keep serving when the preferred model misbehaves?
//
// Timeline (150 s, steady 30 rps on model=auto:best, which prefers mock/premium):
//    0–30 s  healthy           (window=pre)
//   30–90 s  FAULT injected    (window=fault)
//   90–110 s healed            (window=heal, not asserted: breaker half-open probing)
//  110–150 s recovered         (window=recovered)
//
// FAULT modes (env FAULT=errors|down|slow, default errors):
//   errors : premium returns HTTP 500 for 60% of requests
//   down   : premium returns HTTP 503 for everything
//   slow   : premium TTFT jumps to ~6 s (tests hedging + latency ejection)
//
// Asserts: failure rate in fault window < 0.5%, latency stays bounded, traffic shifts away
// from premium during the fault and returns after healing.
//
// Run: k6 run test/k6/03_chaos.js      (FAULT=down k6 run ... for the outage variant)
import { check, sleep } from 'k6';
import exec from 'k6/execution';
import { Rate, Trend } from 'k6/metrics';
import { chat, mockProfile, mockReset } from './lib.js';

const FAULT = __ENV.FAULT || 'errors';
const RATE = Number(__ENV.RATE || 30);

const premiumShare = new Rate('premium_share');
const attempts = new Trend('attempts_per_request');

const FAULTS = {
  errors: { error_rate: 0.6 },
  down: { down: true },
  slow: { ttft_median_ms: 6000, ttft_sigma: 0.4 },
};
const HEAL = { error_rate: 0.003, down: false, ttft_median_ms: 900, ttft_sigma: 0.35 };

function windowOf(ms) {
  if (ms < 30000) return 'pre';
  if (ms < 90000) return 'fault';
  if (ms < 110000) return 'heal';
  return 'recovered';
}

export const options = {
  scenarios: {
    traffic: {
      executor: 'constant-arrival-rate',
      rate: RATE, timeUnit: '1s', duration: '150s',
      preAllocatedVUs: 60, maxVUs: 400,
      exec: 'traffic',
    },
    controller: {
      executor: 'per-vu-iterations',
      vus: 1, iterations: 1, maxDuration: '170s',
      exec: 'controller',
    },
  },
  thresholds: {
    'http_req_failed{window:pre}': ['rate<0.005'],
    'http_req_failed{window:fault}': ['rate<0.005'],
    'http_req_failed{window:recovered}': ['rate<0.005'],
    'http_req_duration{window:fault}': [`p(95)<${FAULT === 'slow' ? 4000 : 3000}`],
    'premium_share{window:pre}': ['rate>0.7'],
    'premium_share{window:fault}': ['rate<0.5'],
    'premium_share{window:recovered}': ['rate>0.6'],
  },
};

export function setup() {
  mockReset();
}

export function teardown() {
  mockReset();
}

export function controller() {
  sleep(30);
  console.log(`>>> injecting fault: ${FAULT}`);
  mockProfile('premium', FAULTS[FAULT]);
  sleep(60);
  console.log('>>> healing');
  mockProfile('premium', HEAL);
}

export function traffic() {
  const w = windowOf(exec.instance.currentTestRunDuration);
  const res = chat('auto:best', 'Explain what a mutex is in two sentences.', {
    task: 'chat',
    tags: { window: w, name: 'chaos_traffic' },
  });
  check(res, { 'status 200': (r) => r.status === 200 }, { window: w });
  premiumShare.add((res.headers['X-Conduit-Model'] || '') === 'mock/premium', { window: w });
  const a = Number(res.headers['X-Conduit-Attempts'] || 1);
  attempts.add(a, { window: w });
}
