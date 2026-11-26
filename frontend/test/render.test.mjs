// Server-side render of the whole research dashboard with fixture data (no browser needed).
// It proves the components render, show every required element, and never invent a confidence value.
import test from 'node:test';
import assert from 'node:assert/strict';
import { createServer } from 'vite';
import React from 'react';
import { renderToStaticMarkup } from 'react-dom/server';

const T0 = Date.parse('2026-10-13T16:00:00Z');

function fixtures(overrides = {}) {
  const points = Array.from({ length: 30 }, (_, i) => ({
    t: T0 + i * 2000, rps: 300 + i, p50_ms: 22, p95_ms: i > 12 ? 400 : 25, p99_ms: i > 12 ? 650 : 30, error_rate: i > 12 ? 0.05 : 0,
    active_requests: 4, strategy: i < 20 ? 'Smooth-Weighted-Round-Robin' : 'Least-Connections', policy_version: i < 20 ? 2 : 3,
    window_valid: i !== 5, backends: {},
  }));
  const dash = {
    now: new Date(T0 + 60000).toISOString(), strategy: 'Least-Connections',
    policy: { strategy: 'Least-Connections', version: 3, reason: 'ai:explore', weights: null },
    agent: { enabled: true, ai_active: true, killed: false, shadow: false, last_decision_at: new Date(T0 + 50000).toISOString(),
      model_version: 'linucb-v1+12u', requests: 8, failures: 1, avg_latency_ms: 14.2, accepted_recent: 2, rejected_recent: 3 },
    fallback: { active: false, kind: 'none', at_baseline: false, baseline: 'Smooth-Weighted-Round-Robin' },
    global: { rps: 330, p95_ms: 400, p99_ms: 650, error_rate: 0.05, window_valid: true },
    rule_based_controller: { present: true, updated: false, hold_reason: 'hysteresis: max change below deadband', windows: 9 },
    backends: [
      { name: 'Backend-1', healthy: true, configured_weight: 3, effective_weight: null, weight_basis: 'not used by Least-Connections', observed_share: 0.5,
        active_conns: 7, total_requests: 1000, rps: 165, p95_ms: 24.5, error_rate: 0, cpu_pct: 41, memory_pct: 62, resource_scope: 'host' },
      { name: 'Backend-2', healthy: false, configured_weight: 2, effective_weight: 0.333, weight_basis: 'policy weight', observed_share: 0, active_conns: 0,
        total_requests: 500, rps: 0, p95_ms: 0, error_rate: 0.2, cpu_pct: null, memory_pct: null, resource_scope: 'host' },
    ],
    host_resources: { source: 'host', note: 'CPU and memory are host-level readings shared by every backend process on this machine; they are not per-backend values.' },
    ...overrides.dash,
  };
  const events = [
    { id: 1, at: new Date(T0 + 24000).toISOString(), kind: 'fault', backend: 'Backend-2', detail: '{"delay_ms":150}' },
    { id: 2, at: new Date(T0 + 40000).toISOString(), kind: 'policy', backend: '', detail: 'Least-Connections (policy v3, ai:explore)' },
    { id: 3, at: new Date(T0 + 44000).toISOString(), kind: 'safety', backend: '', detail: 'fallback to baseline: error_fraction=0.20>=0.10' },
  ];
  const learner = {
    learner: { name: 'linucb', contextual: true, model_version: 'linucb-v1+12u', updates: 12 },
    reward_config: { w_throughput: 0.25, w_latency: 0.35, w_error: 0.25, w_imbalance: 0.15, safety_penalty: -1.0, severe_error_rate: 0.1 },
    exploration: { recent_explored: 4, recent_decisions: 8, guard: { max_fraction: 0.5, overrides: 2 } },
    drift: { drift_suspected: true, reasons: ['workload shift in rps (PSI 0.41 > 0.25)'] },
    persistence: { last_checkpoint: { updates: 12, reason: 'update' } },
    recent_decisions: [
      { decision_id: 'window-00001', explored: true, action: 1, scores: [0.1, 0.22, null, null, 0.1], reward: -0.12, guard_overridden: false },
      { decision_id: 'window-00002', explored: false, action: 2, scores: [0.1, 0.1, 0.2, null, 0.1], reward: -1.0, override: 'severe_error_rate', guard_overridden: true },
    ],
    reward_series: [
      { reward: -0.12, cumulative: -0.12, components: { throughput: 0.1, latency: -0.12, error: -0.05, imbalance: -0.05 } },
      { reward: -1.0, cumulative: -1.12, components: null, override: 'severe_error_rate' },
      { reward: -0.05, cumulative: -1.17, components: { throughput: 0.12, latency: -0.1, error: 0, imbalance: -0.07 } },
    ],
    confidence: { available: false },
    ...overrides.learner,
  };
  const agentStatus = {
    recent_decisions: [
      { decision_id: '', reason: 'held_stale_or_invalid_state', applied: false },
      { decision_id: 'window-00001', at: new Date(T0 + 40000).toISOString(), current_strategy: 'static_swrr', proposed_strategy: 'least_connections', reason: 'applied', applied: true, model_version: 'linucb-v1+11u', latency_ms: 9.5 },
      { decision_id: 'window-00002', at: new Date(T0 + 50000).toISOString(), current_strategy: 'least_connections', proposed_strategy: 'round_robin', reason: 'rejected_min_policy_age', applied: false, model_version: 'linucb-v1+12u', latency_ms: 11.0 },
      { decision_id: 'window-00003', at: new Date(T0 + 56000).toISOString(), current_strategy: 'least_connections', reason: 'agent_unavailable', applied: false, failure: 'timeout' },
    ],
  };
  return { dash, points, events, learner, agentStatus, now: T0 + 60000 };
}

let vite;
let ResearchView;
test.before(async () => {
  vite = await createServer({ server: { middlewareMode: true }, appType: 'custom', logLevel: 'error' });
  ({ ResearchView } = await vite.ssrLoadModule('/src/components/research/ResearchView.jsx'));
});
test.after(async () => vite && (await vite.close()));

const html = (props) => renderToStaticMarkup(React.createElement(ResearchView, props));

test('renders every required element of Phase 18', () => {
  const h = html(fixtures());
  // status: strategy, policy version, model version, last decision time, controller status
  assert.match(h, /Least-Connections/);
  assert.match(h, /v3/);
  assert.match(h, /linucb-v1\+12u/);
  assert.match(h, /10s ago/);
  assert.match(h, /Controller/);
  assert.match(h, /holding: hysteresis/);
  // per-backend table: health, weight, active, RPS, p95, error rate, CPU, memory
  assert.match(h, /Backend-1/);
  assert.match(h, /healthy/);
  assert.match(h, /down/);
  assert.match(h, /24\.5 ms/);
  assert.match(h, /host-level readings/);
  // charts: latency, throughput, errors, reward components, cumulative reward, strategy history
  for (const id of ['chart-latency', 'chart-throughput', 'chart-errors', 'chart-components', 'chart-cumulative']) assert.ok(h.includes(`id="${id}"`), id);
  assert.match(h, /aria-label="strategy history"/);
  // fault-injection markers on the time axis (title text of the marker)
  assert.match(h, /fault: Backend-2/);
  assert.match(h, /safety/);
  // decisions: accepted / rejected / failure, explore vs exploit, reasons
  assert.match(h, /accepted/);
  assert.match(h, /rejected_min_policy_age/);
  assert.match(h, /agent timeout/);
  assert.match(h, />explore</);
  assert.match(h, /exploit/);
  assert.match(h, /capped/);
  assert.match(h, /severe_error_rate/);
  assert.match(h, /drift suspected/);
});

test('AI toggle and reset controls, with a clear fallback indicator', () => {
  let h = html(fixtures());
  assert.match(h, /Disable AI \(kill switch\)/);
  assert.match(h, /Reset to baseline/);
  assert.match(h, /AI active/);
  h = html(fixtures({ dash: { agent: { enabled: true, ai_active: false, killed: true, shadow: false, requests: 0, failures: 0, avg_latency_ms: 0, accepted_recent: 0, rejected_recent: 0 },
    fallback: { active: true, kind: 'kill_switch', reason: 'AI disabled by the operator', at_baseline: true } } }));
  assert.match(h, /FALLBACK: AI disabled/);
  assert.match(h, /Enable AI/);
  h = html(fixtures({ dash: { fallback: { active: true, kind: 'safety_lockout', reason: 'error_fraction=0.20>=0.10', until: new Date(T0 + 99000).toISOString() } } }));
  assert.match(h, /FALLBACK: safety lockout/);
  assert.match(h, /error_fraction=0\.20/);
});

test('never shows a confidence percentage; states why', () => {
  const h = html(fixtures());
  assert.doesNotMatch(h, /confidence\s*[:=]?\s*\d/i);
  assert.doesNotMatch(h, /\d\s*%\s*confiden/i);
  assert.match(h, /does not estimate a calibrated confidence/);
  assert.match(h, /UCB est\./);
});

test('missing measurements stay missing: null CPU/memory render as a dash, not 0%', () => {
  const h = html(fixtures());
  // Backend-2: error rate 20.00%, then CPU and memory are null -> two dashes (never "0%")
  assert.match(h, /<td>20\.00%<\/td><td>–<\/td><td>–<\/td><\/tr>/);
  // Backend-1 has real readings
  assert.match(h, /<td>41%<\/td><td>62%<\/td>/);
});

test('degrades gracefully: no agent, no learner view, no data', () => {
  const noAgent = fixtures({ dash: { agent: { enabled: false, requests: 0, failures: 0, avg_latency_ms: 0, accepted_recent: 0, rejected_recent: 0 },
    fallback: { active: false, kind: 'none' }, rule_based_controller: { present: false } } });
  const h = html({ ...noAgent, learner: { error: 'agent not configured' }, agentStatus: null, points: [], events: [] });
  assert.match(h, /AI not configured/);
  assert.match(h, /agent not configured/);
  assert.match(h, /no data yet/);
  assert.doesNotMatch(h, /Disable AI/);
  assert.match(html({ dash: null, points: [], events: [], learner: null, agentStatus: null, errors: { dashboard: 'connection refused' } }), /Waiting for the load balancer/);
});
