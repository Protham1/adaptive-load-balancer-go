// Pure helpers for the research dashboard (Phase 18). No React, no DOM: unit-tested with `node --test`.

export const CONFIDENCE_NOTICE =
  'No confidence is shown: the bandit does not estimate a calibrated confidence. ' +
  'The "UCB value estimate" is an optimistic reward estimate used for exploration, not a probability.';

export const STRATEGY_COLORS = {
  'Round-Robin': '#6c8cff',
  'Least-Connections': '#35c2a1',
  'Smooth-Weighted-Round-Robin': '#f2b84b',
  'Consistent-Hash': '#c07cff',
  'Adaptive-Weighted-Round-Robin': '#ff7a7a',
};

export const EVENT_STYLE = {
  fault: { color: '#ff6b6b', label: 'fault' },
  policy: { color: '#6c8cff', label: 'policy' },
  safety: { color: '#ff9f43', label: 'safety' },
  kill: { color: '#ff4d4d', label: 'kill' },
  resume: { color: '#35c2a1', label: 'resume' },
  reset: { color: '#c07cff', label: 'reset' },
  health: { color: '#f2b84b', label: 'health' },
  note: { color: '#8a93a6', label: 'note' },
  experiment: { color: '#8a93a6', label: 'experiment' },
};

export function formatAge(ms) {
  if (ms == null || Number.isNaN(ms)) return '–';
  if (ms < 0) ms = 0;
  const s = Math.round(ms / 1000);
  if (s < 60) return `${s}s ago`;
  const m = Math.floor(s / 60);
  if (m < 60) return `${m}m ${s % 60}s ago`;
  return `${Math.floor(m / 60)}h ${m % 60}m ago`;
}

export function fmt(v, digits = 1, unit = '') {
  if (v == null || Number.isNaN(v)) return '–';
  return `${Number(v).toFixed(digits)}${unit}`;
}

export const pct = (fraction, digits = 1) => (fraction == null || Number.isNaN(fraction) ? '–' : `${(fraction * 100).toFixed(digits)}%`);

// ---------------------------------------------------------------------------------- scales
export function scaleLinear([d0, d1], [r0, r1]) {
  const span = d1 - d0;
  return (v) => (span === 0 ? (r0 + r1) / 2 : r0 + ((v - d0) / span) * (r1 - r0));
}

export function niceTicks(min, max, count = 5) {
  if (!Number.isFinite(min) || !Number.isFinite(max)) return [];
  if (min === max) return [min];
  const raw = (max - min) / Math.max(1, count);
  const mag = 10 ** Math.floor(Math.log10(raw));
  const norm = raw / mag;
  const step = (norm >= 7 ? 10 : norm >= 3 ? 5 : norm >= 1.5 ? 2 : 1) * mag;
  const start = Math.ceil(min / step) * step;
  const ticks = [];
  for (let v = start; v <= max + step * 1e-9; v += step) ticks.push(Number(v.toPrecision(12)));
  return ticks;
}

export function linePath(points, xs, ys) {
  let d = '';
  let pen = false;
  for (const p of points) {
    if (p.v == null || Number.isNaN(p.v)) {
      pen = false; // gaps stay gaps: a missing sample is not drawn as zero
      continue;
    }
    d += `${pen ? 'L' : 'M'}${xs(p.t).toFixed(1)},${ys(p.v).toFixed(1)}`;
    pen = true;
  }
  return d;
}

export function extent(seriesList, key = 'v') {
  let lo = Infinity;
  let hi = -Infinity;
  for (const s of seriesList) {
    for (const p of s.points) {
      const v = p[key];
      if (v == null || Number.isNaN(v)) continue;
      if (v < lo) lo = v;
      if (v > hi) hi = v;
    }
  }
  return lo === Infinity ? [0, 1] : [lo, hi];
}

// ------------------------------------------------------------------------------- history
export function mergePoints(existing, incoming, cap = 900) {
  const seen = new Set(existing.map((p) => p.t));
  const merged = existing.concat(incoming.filter((p) => !seen.has(p.t)));
  merged.sort((a, b) => a.t - b.t);
  return merged.length > cap ? merged.slice(merged.length - cap) : merged;
}

export function mergeEvents(existing, incoming, cap = 500) {
  const seen = new Set(existing.map((e) => e.id));
  const merged = existing.concat(incoming.filter((e) => !seen.has(e.id)));
  merged.sort((a, b) => a.id - b.id);
  return merged.length > cap ? merged.slice(merged.length - cap) : merged;
}

export function seriesFromPoints(points, pick) {
  return points.map((p) => ({ t: p.t, v: pick(p) }));
}

/** Consecutive runs of the same strategy -> [{strategy, start, end, version}] */
export function segmentStrategies(points) {
  const segs = [];
  for (const p of points) {
    const last = segs[segs.length - 1];
    if (last && last.strategy === p.strategy) {
      last.end = p.t;
      last.version = p.policy_version;
    } else {
      segs.push({ strategy: p.strategy, start: p.t, end: p.t, version: p.policy_version });
    }
  }
  return segs;
}

/** Events visible in [t0, t1] (event.at is an ISO time) as chart markers. */
export function eventMarkers(events, t0, t1, kinds = null) {
  const out = [];
  for (const e of events) {
    const t = Date.parse(e.at);
    if (Number.isNaN(t) || t < t0 || t > t1) continue;
    if (kinds && !kinds.includes(e.kind)) continue;
    const st = EVENT_STYLE[e.kind] || EVENT_STYLE.note;
    out.push({ t, kind: e.kind, color: st.color, label: e.backend ? `${st.label}: ${e.backend}` : st.label, detail: e.detail });
  }
  return out;
}

// -------------------------------------------------------------------------------- reward
export const REWARD_COMPONENTS = ['throughput', 'latency', 'error', 'imbalance'];

export function rewardComponentSeries(rewardSeries) {
  const out = Object.fromEntries(REWARD_COMPONENTS.map((c) => [c, []]));
  rewardSeries.forEach((r, i) => {
    for (const c of REWARD_COMPONENTS) {
      const v = r.components ? r.components[c] : null; // safety-override windows carry no components
      out[c].push({ t: i, v: v == null ? null : v });
    }
  });
  return out;
}

export function cumulativeSeries(rewardSeries) {
  return rewardSeries.map((r, i) => ({ t: i, v: r.cumulative }));
}

// ------------------------------------------------------------------------------ decisions
/** Join the Go-side decision records (accepted/rejected + reason) with the learner view (explore/exploit, reward). */
export function mergeDecisions(goRecent, learnerRecent) {
  const byId = new Map((learnerRecent || []).map((d) => [d.decision_id, d]));
  const rows = [];
  for (const g of goRecent || []) {
    if (!g.decision_id) continue; // local holds carry no decision id
    const l = byId.get(g.decision_id) || {};
    rows.push({
      id: g.decision_id,
      at: g.at,
      current: g.current_strategy,
      proposed: g.proposed_strategy || null,
      reason: g.reason,
      accepted: Boolean(g.applied),
      latencyMs: g.latency_ms,
      model: g.model_version || null,
      failure: g.failure || null,
      choice: l.explored === undefined ? null : l.explored ? 'explore' : 'exploit',
      guardOverridden: Boolean(l.guard_overridden),
      ucbEstimate: Array.isArray(l.scores) && l.action != null && l.scores[l.action] != null ? l.scores[l.action] : null,
      reward: l.reward ?? null,
      override: l.override ?? null,
    });
  }
  return rows;
}

export function decisionCounts(rows) {
  const c = { accepted: 0, rejected: 0, explore: 0, exploit: 0, failed: 0 };
  for (const r of rows) {
    if (r.failure) c.failed += 1;
    else if (r.accepted) c.accepted += 1;
    else if (r.proposed) c.rejected += 1;
    if (r.choice === 'explore') c.explore += 1;
    if (r.choice === 'exploit') c.exploit += 1;
  }
  return c;
}

// ------------------------------------------------------------------------------- fallback
export function fallbackBadge(fallback, agent) {
  if (!agent || !agent.enabled) return { tone: 'muted', label: 'AI not configured', text: 'No agent URL is configured; a static or rule-based policy is serving traffic.' };
  if (fallback && fallback.kind === 'kill_switch') {
    return { tone: 'danger', label: 'FALLBACK: AI disabled', text: 'Kill switch is on. Baseline strategy in force; the agent is not consulted.' };
  }
  if (fallback && fallback.kind === 'safety_lockout') {
    return { tone: 'warn', label: 'FALLBACK: safety lockout', text: `Safety fallback active${fallback.reason ? ` (${fallback.reason})` : ''}; AI proposals are ignored until the lockout ends.` };
  }
  if (agent.shadow) return { tone: 'info', label: 'SHADOW: recommending only', text: 'The agent recommends but cannot apply policies.' };
  return { tone: 'ok', label: 'AI active', text: 'The agent may propose policies; the policy manager validates every proposal.' };
}

export function controllerStatus(agent) {
  if (!agent || !agent.enabled) return 'not configured';
  if (agent.killed) return 'disabled (kill switch)';
  if (agent.shadow) return 'shadow';
  return 'active';
}
