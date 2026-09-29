// 05_soak.js — Find the throughput knee and check for leaks.
//
// Ramps arrival rate up to MAX_RATE, holds, then ramps down. A side scenario samples the
// gateway's heap and goroutine count from the admin runtime endpoint every 10 s so a leak
// shows up as a monotonic climb after load drops.
//
// Mix: 70% non-streaming, 30% streaming (TTFB ≈ TTFT because Conduit defers headers until the
// first upstream chunk). Streaming TTFB is tracked separately.
//
// Run: k6 run test/k6/05_soak.js   (env: MAX_RATE=3000 HOLD=5m)
import { check, sleep } from 'k6';
import { Trend, Gauge } from 'k6/metrics';
import { chat, adminGet } from './lib.js';

const MAX_RATE = Number(__ENV.MAX_RATE || 2000);
const HOLD = __ENV.HOLD || '3m';

const ttft = new Trend('stream_ttfb_ms', true);
const heap = new Gauge('gateway_heap_mb');
const goroutines = new Gauge('gateway_goroutines');

export const options = {
  scenarios: {
    load: {
      executor: 'ramping-arrival-rate',
      startRate: 50, timeUnit: '1s',
      preAllocatedVUs: 200, maxVUs: 5000,
      stages: [
        { target: Math.floor(MAX_RATE / 4), duration: '1m' },
        { target: Math.floor(MAX_RATE / 2), duration: '1m' },
        { target: MAX_RATE, duration: '1m' },
        { target: MAX_RATE, duration: HOLD },
        { target: 50, duration: '1m' },
        { target: 0, duration: '2m' }, // cool-down: watch heap here
      ],
      exec: 'load',
    },
    sampler: {
      executor: 'constant-vus', vus: 1, duration: '12m',
      exec: 'sampler',
    },
  },
  thresholds: {
    http_req_failed: ['rate<0.01'],
    http_req_duration: ['p(95)<2000'],
    stream_ttfb_ms: ['p(95)<1500'],
  },
};

export function load() {
  const stream = Math.random() < 0.3;
  const res = chat('auto', 'Give me one tip for writing readable code.', {
    stream, task: 'chat', max_tokens: 32, tags: { name: stream ? 'soak_stream' : 'soak_json' },
  });
  check(res, { 'status 200': (r) => r.status === 200 });
  if (stream) ttft.add(res.timings.waiting);
}

export function sampler() {
  const r = adminGet('/admin/v1/runtime');
  if (r.status === 200) {
    heap.add(r.json('heap_alloc_mb'));
    goroutines.add(r.json('goroutines'));
  }
  sleep(10);
}
