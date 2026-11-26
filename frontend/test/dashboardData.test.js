import test from 'node:test';
import assert from 'node:assert/strict';
import {
  CONFIDENCE_NOTICE, formatAge, fmt, pct, scaleLinear, niceTicks, linePath, extent, mergePoints, mergeEvents,
  segmentStrategies, eventMarkers, rewardComponentSeries, cumulativeSeries, mergeDecisions, decisionCounts,
  fallbackBadge, controllerStatus,
} from '../src/lib/dashboardData.js';

test('formatting never turns missing values into zero', () => {
  assert.equal(fmt(null), '–');
  assert.equal(fmt(undefined, 2, ' ms'), '–');
  assert.equal(fmt(NaN), '–');
  assert.equal(fmt(12.345, 1, ' ms'), '12.3 ms');
  assert.equal(pct(null), '–');
  assert.equal(pct(0.0456), '4.6%');
  assert.equal(formatAge(null), '–');
  assert.equal(formatAge(4000), '4s ago');
  assert.equal(formatAge(125000), '2m 5s ago');
  assert.equal(formatAge(-5), '0s ago');
});

test('scales and ticks', () => {
  const s = scaleLinear([0, 10], [100, 200]);
  assert.equal(s(0), 100);
  assert.equal(s(10), 200);
  assert.equal(s(5), 150);
  assert.equal(scaleLinear([3, 3], [0, 10])(3), 5); // degenerate domain does not divide by zero
  assert.deepEqual(niceTicks(0, 100, 5), [0, 20, 40, 60, 80, 100]);
  assert.deepEqual(niceTicks(7, 7), [7]);
  assert.deepEqual(niceTicks(NaN, 3), []);
});

test('line paths leave gaps for missing samples', () => {
  const xs = (t) => t;
  const ys = (v) => v;
  const d = linePath([{ t: 0, v: 1 }, { t: 1, v: 2 }, { t: 2, v: null }, { t: 3, v: 4 }, { t: 4, v: 5 }], xs, ys);
  assert.equal(d, 'M0.0,1.0L1.0,2.0M3.0,4.0L4.0,5.0');
  assert.deepEqual(extent([{ points: [{ t: 0, v: 3 }, { t: 1, v: null }, { t: 2, v: -1 }] }]), [-1, 3]);
  assert.deepEqual(extent([{ points: [] }]), [0, 1]);
});

test('history and events merge without duplicates and stay bounded', () => {
  const a = [{ t: 1 }, { t: 2 }, { t: 3 }];
  const m = mergePoints(a, [{ t: 3 }, { t: 4 }, { t: 5 }], 4);
  assert.deepEqual(m.map((p) => p.t), [2, 3, 4, 5]);
  const e = mergeEvents([{ id: 1 }, { id: 2 }], [{ id: 2 }, { id: 3 }], 10);
  assert.deepEqual(e.map((x) => x.id), [1, 2, 3]);
});

test('strategy history is segmented by consecutive strategy', () => {
  const pts = [
    { t: 1, strategy: 'A', policy_version: 1 }, { t: 2, strategy: 'A', policy_version: 1 },
    { t: 3, strategy: 'B', policy_version: 2 }, { t: 4, strategy: 'A', policy_version: 3 },
  ];
  assert.deepEqual(segmentStrategies(pts), [
    { strategy: 'A', start: 1, end: 2, version: 1 },
    { strategy: 'B', start: 3, end: 3, version: 2 },
    { strategy: 'A', start: 4, end: 4, version: 3 },
  ]);
});

test('fault and other events become chart markers inside the window only', () => {
  const t = Date.parse('2026-10-13T16:00:10Z');
  const evs = [
    { id: 1, at: '2026-10-13T16:00:05Z', kind: 'fault', backend: 'Backend-2', detail: '{"delay_ms":150}' },
    { id: 2, at: '2026-10-13T16:01:00Z', kind: 'policy', detail: 'x' },
    { id: 3, at: 'garbage', kind: 'note', detail: '' },
  ];
  const m = eventMarkers(evs, t - 10000, t + 10000);
  assert.equal(m.length, 1);
  assert.equal(m[0].label, 'fault: Backend-2');
  assert.equal(eventMarkers(evs, 0, Date.parse('2027-01-01'), ['policy']).length, 1);
});

test('reward components and cumulative reward', () => {
  const series = [
    { components: { throughput: 0.1, latency: -0.05, error: 0, imbalance: -0.01 }, cumulative: 0.04 },
    { components: null, cumulative: -0.96, override: 'severe_error_rate' }, // safety override: no components
    { components: { throughput: 0.1, latency: -0.02, error: 0, imbalance: 0 }, cumulative: -0.88 },
  ];
  const c = rewardComponentSeries(series);
  assert.deepEqual(c.latency.map((p) => p.v), [-0.05, null, -0.02]);
  assert.deepEqual(cumulativeSeries(series).map((p) => p.v), [0.04, -0.96, -0.88]);
});

test('decisions join the Go outcome with the learner view and count correctly', () => {
  const go = [
    { decision_id: '', reason: 'held_stale_or_invalid_state' },
    { decision_id: 'w1', at: 't', current_strategy: 'static_swrr', proposed_strategy: 'least_connections', reason: 'applied', applied: true, model_version: 'm', latency_ms: 7 },
    { decision_id: 'w2', current_strategy: 'least_connections', proposed_strategy: 'round_robin', reason: 'rejected_min_policy_age', applied: false },
    { decision_id: 'w3', reason: 'agent_unavailable', failure: 'timeout' },
  ];
  const learner = [
    { decision_id: 'w1', explored: true, action: 1, scores: [0.1, 0.25, null, null, 0.1], reward: -0.12, guard_overridden: false },
    { decision_id: 'w2', explored: false, action: 0, scores: [0.3, 0.1, null, null, 0.1], reward: null, guard_overridden: true },
  ];
  const rows = mergeDecisions(go, learner);
  assert.equal(rows.length, 3);                       // the local hold (no id) is not a decision
  assert.equal(rows[0].choice, 'explore');
  assert.equal(rows[0].ucbEstimate, 0.25);
  assert.equal(rows[0].reward, -0.12);
  assert.equal(rows[1].choice, 'exploit');
  assert.equal(rows[1].guardOverridden, true);
  assert.equal(rows[2].choice, null);                 // no learner record -> unknown, not guessed
  assert.deepEqual(decisionCounts(rows), { accepted: 1, rejected: 1, explore: 1, exploit: 1, failed: 1 });
});

test('fallback indicator states', () => {
  const agent = { enabled: true, shadow: false, killed: false };
  assert.equal(fallbackBadge({ kind: 'none' }, agent).tone, 'ok');
  assert.equal(fallbackBadge({ kind: 'kill_switch' }, { ...agent, killed: true }).tone, 'danger');
  const lock = fallbackBadge({ kind: 'safety_lockout', reason: 'error_fraction=0.2>=0.1' }, agent);
  assert.equal(lock.tone, 'warn');
  assert.match(lock.text, /error_fraction/);
  assert.equal(fallbackBadge({ kind: 'none' }, { enabled: true, shadow: true }).tone, 'info');
  assert.equal(fallbackBadge({ kind: 'none' }, { enabled: false }).tone, 'muted');
  assert.equal(controllerStatus({ enabled: true, killed: true }), 'disabled (kill switch)');
  assert.equal(controllerStatus(null), 'not configured');
});

test('the confidence notice explicitly denies a calibrated confidence', () => {
  assert.match(CONFIDENCE_NOTICE, /does not estimate a calibrated confidence/);
  assert.doesNotMatch(CONFIDENCE_NOTICE, /\d\s*%/);
});
