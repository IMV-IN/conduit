// 02_accuracy.js — Is Conduit routing *correctly*?
//
// Three checks:
//  A) Task classification accuracy: labeled prompts -> compare x-conduit-task with the label.
//  B) Hard-constraint safety (must be exactly zero violations):
//       - prompts too long for mock/tiny (4k ctx) must never land on mock/tiny
//       - requests with tools must never land on mock/notools
//     Run under `auto:cheap`, the objective most tempted to pick those models, with exploration on.
//  C) Objective sanity: auto:best prefers the premium model for code; auto:cheap avoids it.
//
// Run: k6 run test/k6/02_accuracy.js
import { check } from 'k6';
import { SharedArray } from 'k6/data';
import { Rate, Counter } from 'k6/metrics';
import { chat, pick } from './lib.js';

const tasks = new SharedArray('tasks', () =>
  open('../datasets/tasks.jsonl')
    .split('\n')
    .filter((l) => l.trim())
    .map((l) => JSON.parse(l)),
);

const TOOLS = [{ type: 'function', function: { name: 'lookup', description: 'lookup', parameters: { type: 'object', properties: {} } } }];
const LONG_PROMPT = 'lorem ipsum dolor sit amet '.repeat(3000); // ~81k chars ≈ 20k tokens

const classMatch = new Rate('task_class_match');
const violation = new Rate('constraint_violation');
const bestPremium = new Rate('best_prefers_premium_for_code');
const cheapAvoidsPremium = new Rate('cheap_avoids_premium');
const mismatches = new Counter('classification_mismatches');

export const options = {
  scenarios: {
    classification: {
      executor: 'shared-iterations',
      vus: 10,
      iterations: tasks.length * 10,
      exec: 'classification',
    },
    constraints: {
      executor: 'constant-arrival-rate',
      rate: 50, timeUnit: '1s', duration: '40s',
      preAllocatedVUs: 20, maxVUs: 100,
      exec: 'constraints',
      startTime: '5s',
    },
    objectives: {
      executor: 'shared-iterations',
      vus: 10, iterations: 400,
      exec: 'objectives',
      startTime: '50s',
    },
  },
  thresholds: {
    task_class_match: ['rate>0.85'],
    constraint_violation: ['rate==0'],
    'http_req_failed{scenario:constraints}': ['rate==0'], // a violation would surface as mock 400s
    best_prefers_premium_for_code: ['rate>0.85'],
    cheap_avoids_premium: ['rate>0.90'],
  },
};

export function classification() {
  const t = pick(tasks);
  const res = chat('auto', t.prompt, {
    task: t.task, // only used by the mock's quality simulation, not by Conduit
    tools: t.tools ? TOOLS : undefined,
    tags: { name: 'classification' },
  });
  const got = res.headers['X-Conduit-Task'];
  const ok = res.status === 200 && got === t.task;
  classMatch.add(ok, { label: t.task });
  if (!ok) {
    mismatches.add(1, { label: t.task, got: String(got) });
    if (__ITER % 25 === 0) console.warn(`mismatch label=${t.task} got=${got}`);
  }
}

export function constraints() {
  if (Math.random() < 0.5) {
    const res = chat('auto:cheap', LONG_PROMPT, { tags: { name: 'long_context' } });
    const model = res.headers['X-Conduit-Model'] || '';
    const bad = model === 'mock/tiny' || res.status !== 200;
    violation.add(bad, { kind: 'context' });
    check(res, { 'long ctx served': (r) => r.status === 200 });
  } else {
    const res = chat('auto:cheap', 'Use the tool to look something up.', {
      tools: TOOLS, tags: { name: 'tools' },
    });
    const model = res.headers['X-Conduit-Model'] || '';
    const bad = model === 'mock/notools' || res.status !== 200;
    violation.add(bad, { kind: 'capability' });
    check(res, { 'tool request served': (r) => r.status === 200 });
  }
}

export function objectives() {
  const code = tasks.filter((t) => t.task === 'code');
  const cls = tasks.filter((t) => t.task === 'classification_short');

  const a = chat('auto:best', pick(code).prompt, { tags: { name: 'best_code' } });
  bestPremium.add((a.headers['X-Conduit-Model'] || '') === 'mock/premium');

  const b = chat('auto:cheap', pick(cls).prompt, { tags: { name: 'cheap_cls' } });
  cheapAvoidsPremium.add((b.headers['X-Conduit-Model'] || '') !== 'mock/premium');
}
