// 04_learning.js — Is Conduit *useful*? Cost/quality versus static routing strategies.
//
// The mock provider simulates per-(model, task) correctness (see test/mockprovider). Four
// strategies are run one after another on the same task mix:
//   baseline-best   : always mock/premium          (max quality, max cost)
//   baseline-cheap  : always mock/fast             (low cost, weaker on code/math)
//   round-robin     : premium/balanced/fast in turn
//   default         : Conduit balanced policy WITH learning (feedback reward = correctness)
//
// After each response the script posts /v1/feedback with reward 1/0. Only `default` learns.
// We compare the *late* phase of `default` (after it has learned) with the baselines.
//
// PASS criteria (written to test-results/learning.json -> verdict):
//   late accuracy(default) >= 0.95 * accuracy(baseline-best)
//   cost per request(default, late) <= 0.60 * cost per request(baseline-best)
//   accuracy(default, late) >= accuracy(default, early)   (it actually improves or holds)
//   data_valid: enough samples and a sane baseline (prevents vacuous passes)
//
// Run: k6 run test/k6/04_learning.js   (env: RATE=40 BASE_DUR=40 LEARN_DUR=120)
import { SharedArray } from 'k6/data';
import exec from 'k6/execution';
import { Rate, Counter } from 'k6/metrics';
import { chat, feedback, answerOK, pick } from './lib.js';

const RATE = Number(__ENV.RATE || 40);
const BASE_DUR = Number(__ENV.BASE_DUR || 40);
const LEARN_DUR = Number(__ENV.LEARN_DUR || 120);
const GAP = 5;

const tasks = new SharedArray('tasks', () =>
  open('../datasets/tasks.jsonl')
    .split('\n')
    .filter((l) => l.trim())
    .map((l) => JSON.parse(l)),
);
const TOOLS = [{ type: 'function', function: { name: 'lookup', parameters: { type: 'object', properties: {} } } }];

const correct = new Rate('correct');
const costUSD = new Counter('cost_usd');
const served = new Counter('served');

const POLICIES = [
  { key: 'baseline-best', fn: 'runBest', dur: BASE_DUR },
  { key: 'baseline-cheap', fn: 'runCheap', dur: BASE_DUR },
  { key: 'round-robin', fn: 'runRR', dur: BASE_DUR },
  { key: 'default', fn: 'runLearn', dur: LEARN_DUR },
];

const scenarios = {};
const thresholds = {};
let start = 0;
for (const p of POLICIES) {
  scenarios[p.key] = {
    executor: 'constant-arrival-rate',
    rate: RATE, timeUnit: '1s', duration: `${p.dur}s`,
    preAllocatedVUs: 40, maxVUs: 300,
    startTime: `${start}s`,
    exec: p.fn,
  };
  start += p.dur + GAP;
  // Declaring a threshold forces k6 to materialise the tagged sub-metric for handleSummary.
  thresholds[`correct{policy:${p.key}}`] = ['rate>=0'];
  thresholds[`cost_usd{policy:${p.key}}`] = ['count>=0'];
  thresholds[`served{policy:${p.key}}`] = ['count>=0'];
}
for (const ph of ['early', 'late']) {
  thresholds[`correct{policy:default,phase:${ph}}`] = ['rate>=0'];
  thresholds[`cost_usd{policy:default,phase:${ph}}`] = ['count>=0'];
  thresholds[`served{policy:default,phase:${ph}}`] = ['count>=0'];
}
thresholds['http_req_failed{name:learning_traffic}'] = ['rate<0.01'];

export const options = { scenarios, thresholds };

function run(policy, total) {
  const t = pick(tasks);
  const res = chat(`policy:${policy}`, t.prompt, {
    task: t.task,
    tools: t.tools ? TOOLS : undefined,
    max_tokens: 64,
    tags: { name: 'learning_traffic', policy },
  });
  const i = exec.scenario.iterationInTest;
  const phase = policy === 'default' ? (i < total / 3 ? 'early' : i >= (2 * total) / 3 ? 'late' : 'mid') : 'all';

  const ok = res.status === 200 && answerOK(res);
  const cost = parseFloat(res.headers['X-Conduit-Cost-Usd'] || '0') || 0;
  // One emission per request. The `policy:X` sub-metric also matches points that carry an
  // extra `phase` tag, so emitting twice would double-count.
  const tags = phase !== 'all' ? { policy, phase } : { policy };
  correct.add(ok, tags);
  costUSD.add(cost, tags);
  served.add(1, tags);

  const did = res.headers['X-Conduit-Decision-Id'];
  if (did) feedback(did, ok ? 1 : 0); // simulated ground truth; real systems use thumbs/validators/judge
}

export function runBest() { run('baseline-best', RATE * BASE_DUR); }
export function runCheap() { run('baseline-cheap', RATE * BASE_DUR); }
export function runRR() { run('round-robin', RATE * BASE_DUR); }
export function runLearn() { run('default', RATE * LEARN_DUR); }

export function handleSummary(data) {
  const m = (name) => data.metrics[name];
  const get = (base, sel, key) => (m(`${base}{${sel}}`) ? m(`${base}{${sel}}`).values[key] : NaN);

  const stat = (sel) => {
    const n = get('served', sel, 'count');
    const acc = get('correct', sel, 'rate');
    const cost = get('cost_usd', sel, 'count');
    return { n, accuracy: acc, cost_per_request_usd: cost / n, cost_per_1k_usd: (cost / n) * 1000, accuracy_per_dollar: acc / (cost / n) };
  };

  const out = {
    'baseline-best': stat('policy:baseline-best'),
    'baseline-cheap': stat('policy:baseline-cheap'),
    'round-robin': stat('policy:round-robin'),
    'default (all)': stat('policy:default'),
    'default (early)': stat('policy:default,phase:early'),
    'default (late)': stat('policy:default,phase:late'),
  };
  const best = out['baseline-best'];
  const late = out['default (late)'];
  const early = out['default (early)'];
  const checks = {
    // Guard against vacuous passes (e.g. every request failed => 0 >= 0): need real samples and
    // a sane baseline before any comparison means anything.
    data_valid: late.n >= 100 && best.n >= 100 && best.accuracy > 0.5 && best.cost_per_request_usd > 0,
    accuracy_retained: late.accuracy >= 0.95 * best.accuracy,
    cost_reduced: late.cost_per_request_usd <= 0.6 * best.cost_per_request_usd,
    improves_or_holds: late.accuracy >= early.accuracy - 0.01,
  };
  const verdict = {
    pass: Object.values(checks).every(Boolean),
    checks,
    accuracy_ratio_vs_best: late.accuracy / best.accuracy,
    cost_ratio_vs_best: late.cost_per_request_usd / best.cost_per_request_usd,
  };

  const f = (x, d = 3) => (Number.isFinite(x) ? x.toFixed(d) : 'n/a');
  const rows = Object.entries(out).map(
    ([k, v]) => `  ${k.padEnd(18)} n=${String(v.n).padStart(6)}  acc=${f(v.accuracy)}  $/1k=${f(v.cost_per_1k_usd, 4)}  acc/$=${f(v.accuracy_per_dollar, 1)}`,
  );
  const text = ['', 'Cost vs quality', ...rows, '',
    `  accuracy vs best: ${f(verdict.accuracy_ratio_vs_best)}x   cost vs best: ${f(verdict.cost_ratio_vs_best)}x`,
    `  VERDICT: ${verdict.pass ? 'PASS' : 'FAIL'} ${JSON.stringify(checks)}`, ''].join('\n');

  const outPath = `${__ENV.RESULTS_DIR || 'test-results'}/learning.json`;
  return { stdout: text, [outPath]: JSON.stringify({ policies: out, verdict }, null, 2) };
}
