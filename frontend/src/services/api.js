// API client for communicating with Go Load Balancer

const API_BASE = '/api';

export async function fetchStatus() {
  const res = await fetch(`${API_BASE}/status`);
  if (!res.ok) throw new Error(`Status fetch failed: ${res.statusText}`);
  return res.json();
}

export async function setStrategy(strategy) {
  const res = await fetch(`${API_BASE}/strategy`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ strategy }),
  });
  if (!res.ok) throw new Error(`Set strategy failed: ${res.statusText}`);
  return res.json();
}

export async function toggleBackend(name) {
  const res = await fetch(`${API_BASE}/backends/toggle`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ name }),
  });
  if (!res.ok) throw new Error(`Toggle backend failed: ${res.statusText}`);
  return res.json();
}

export async function setBackendWeight(name, weight) {
  const res = await fetch(`${API_BASE}/backends/weight`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ name, weight: Number(weight) }),
  });
  if (!res.ok) throw new Error(`Set weight failed: ${res.statusText}`);
  return res.json();
}

export async function sendTestRequest({ clientIP, delay } = {}) {
  const res = await fetch(`${API_BASE}/send-request`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({
      client_ip: clientIP || '',
      delay: delay || '',
    }),
  });
  if (!res.ok) throw new Error(`Send request failed: ${res.statusText}`);
  return res.json();
}

export function subscribeEvents(onMessage, onError) {
  const es = new EventSource(`${API_BASE}/events`);

  es.onmessage = (event) => {
    try {
      const data = JSON.parse(event.data);
      onMessage(data);
    } catch {
      // Ignore heartbeat or non-json
    }
  };

  es.onerror = (err) => {
    if (onError) onError(err);
  };

  return () => {
    es.close();
  };
}

// ---------------------------------------------------------------- research dashboard (Phase 18)
async function getJSON(path) {
  const res = await fetch(`${API_BASE}${path}`);
  if (!res.ok) throw new Error(`${path}: ${res.status} ${res.statusText}`);
  return res.json();
}

async function postJSON(path, body) {
  const res = await fetch(`${API_BASE}${path}`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(body || {}),
  });
  if (!res.ok) throw new Error(`${path}: ${res.status} ${(await res.text()).trim()}`);
  return res.json();
}

export const fetchDashboard = () => getJSON('/dashboard');
export const fetchHistory = (since = 0) => getJSON(`/history?since=${since}`);
export const fetchEventLog = (since = 0) => getJSON(`/events/log?since=${since}`);
export const fetchAgentStatus = () => getJSON('/agent');
export const fetchLearner = (limit = 100) => getJSON(`/agent/learner?limit=${limit}`);
/** kill=true disables AI decisions and applies the baseline; kill=false re-enables them. */
export const setAIKill = (kill, baselineAction) =>
  postJSON('/agent/kill', baselineAction == null ? { kill } : { kill, baseline_action: baselineAction });
export const resetAgent = (archive = true) => postJSON('/agent/reset', { archive });
