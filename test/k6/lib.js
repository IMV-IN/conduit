import http from 'k6/http';

export const BASE = __ENV.BASE || 'http://localhost:8080';
export const ADMIN = __ENV.ADMIN || 'http://localhost:8081';
export const MOCK = __ENV.MOCK || 'http://localhost:9001';
export const KEY = __ENV.CONDUIT_KEY || 'sk-conduit-dev';
export const ADMIN_KEY = __ENV.CONDUIT_ADMIN_KEY || 'admin-dev';

export function headers(extra = {}) {
  return Object.assign(
    { 'Content-Type': 'application/json', Authorization: `Bearer ${KEY}` },
    extra,
  );
}

// Send a chat completion through Conduit.
// opts: { max_tokens, stream, task (eval label for the mock), headers, tags, tools, timeout }
export function chat(model, prompt, opts = {}) {
  const body = {
    model,
    messages: [{ role: 'user', content: prompt }],
    max_tokens: opts.max_tokens || 64,
    stream: !!opts.stream,
    metadata: opts.task ? { eval_task: opts.task } : {},
  };
  if (opts.tools) body.tools = opts.tools;
  return http.post(`${BASE}/v1/chat/completions`, JSON.stringify(body), {
    headers: headers(opts.headers),
    tags: opts.tags || {},
    timeout: opts.timeout || '30s',
  });
}

export function feedback(decisionId, reward) {
  return http.post(
    `${BASE}/v1/feedback`,
    JSON.stringify({ decision_id: decisionId, reward }),
    { headers: headers(), tags: { name: 'feedback' } },
  );
}

// ---- mock provider control (upstream model names: premium, balanced, fast, tiny, notools) ----
export function mockProfile(model, patch) {
  return http.post(`${MOCK}/__control/profile?model=${model}`, JSON.stringify(patch), {
    headers: { 'Content-Type': 'application/json' },
    tags: { name: 'mock_control' },
  });
}
export function mockReset() {
  return http.post(`${MOCK}/__control/reset`, null, { tags: { name: 'mock_control' } });
}

export function adminGet(path) {
  return http.get(`${ADMIN}${path}`, {
    headers: { Authorization: `Bearer ${ADMIN_KEY}` },
    tags: { name: 'admin' },
  });
}

// Did the (simulated) model answer correctly? The mock appends ANSWER:OK / ANSWER:WRONG.
export function answerOK(res) {
  try {
    return String(res.json('choices.0.message.content')).endsWith('ANSWER:OK');
  } catch (_) {
    return false;
  }
}

export function pick(arr) {
  return arr[Math.floor(Math.random() * arr.length)];
}

export function pct(values, p) {
  if (!values || values.length === 0) return NaN;
  return values[Math.min(values.length - 1, Math.floor((p / 100) * values.length))];
}
