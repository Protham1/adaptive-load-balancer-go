import React from 'react';
import { StrategyStrip, TimeSeriesChart } from './charts';
import {
  CONFIDENCE_NOTICE, REWARD_COMPONENTS, controllerStatus, cumulativeSeries, decisionCounts, eventMarkers, fallbackBadge,
  fmt, formatAge, mergeDecisions, pct, rewardComponentSeries, seriesFromPoints, EVENT_STYLE,
} from '../../lib/dashboardData';

const COMPONENT_COLORS = { throughput: '#35c2a1', latency: '#ff7a7a', error: '#ff9f43', imbalance: '#c07cff' };

function Badge({ tone = 'muted', children, title }) {
  return <span className={`rs-badge rs-${tone}`} title={title}>{children}</span>;
}

// ------------------------------------------------------------------------------ status
export function StatusPanel({ dash, learner, now, onToggleAI, onReset, busy }) {
  const a = dash.agent;
  const fb = fallbackBadge(dash.fallback, a);
  const lastAt = a.last_decision_at ? now - Date.parse(a.last_decision_at) : null;
  const pol = dash.policy || {};
  const ld = learner && learner.learner;
  return (
    <section className="glass-panel rs-panel" aria-label="status">
      <h2>Controller status</h2>
      <div className={`rs-fallback rs-${fb.tone}`} role="status">
        <strong>{fb.label}</strong>
        <span>{fb.text}</span>
      </div>
      <dl className="rs-kv">
        <div><dt>Current strategy</dt><dd>{dash.strategy}</dd></div>
        <div><dt>Policy version</dt><dd>v{pol.version}{pol.reason ? <span className="rs-muted"> ({pol.reason})</span> : null}</dd></div>
        <div><dt>Model version</dt><dd>{a.model_version || (ld && ld.model_version) || '–'}</dd></div>
        <div><dt>Last decision</dt><dd>{a.last_decision_at ? formatAge(lastAt) : '–'}</dd></div>
        <div><dt>Controller</dt><dd>{controllerStatus(a)}</dd></div>
        <div><dt>Agent</dt><dd>{a.enabled ? `${a.requests} requests, ${a.failures} failures, avg ${fmt(a.avg_latency_ms, 1, ' ms')}` : 'not configured'}</dd></div>
        <div><dt>Rule-based controller</dt><dd>{dash.rule_based_controller.present ? (dash.rule_based_controller.updated ? 'publishing weights' : `holding: ${dash.rule_based_controller.hold_reason || 'no decision yet'}`) : 'not configured'}</dd></div>
        <div><dt>Recent decisions</dt><dd>{a.accepted_recent} accepted / {a.rejected_recent} rejected</dd></div>
      </dl>
      {a.enabled && (
        <div className="rs-actions">
          <button className={`btn ${a.killed ? 'btn-primary' : 'btn-secondary'}`} disabled={busy} onClick={() => onToggleAI(!a.killed)}
            title={a.killed ? 'Re-enable AI decisions' : 'Disable AI decisions and return to the baseline strategy'}>
            {a.killed ? 'Enable AI' : 'Disable AI (kill switch)'}
          </button>
          <button className="btn btn-secondary" disabled={busy} onClick={onReset}
            title="Apply the baseline immediately and restart learning from scratch (the model is archived)">
            Reset to baseline
          </button>
        </div>
      )}
    </section>
  );
}

// ------------------------------------------------------------------------------ backends
export function BackendTable({ dash }) {
  return (
    <section className="glass-panel rs-panel" aria-label="backends">
      <h2>Backends</h2>
      <table className="rs-table">
        <thead>
          <tr>
            <th>Backend</th><th>Health</th><th>Eff. weight</th><th>Traffic share</th><th>Active</th><th>RPS</th>
            <th>p95</th><th>Errors</th><th>CPU*</th><th>Mem*</th>
          </tr>
        </thead>
        <tbody>
          {dash.backends.map((b) => (
            <tr key={b.name}>
              <td>{b.name}<div className="rs-muted">w={b.configured_weight}</div></td>
              <td><Badge tone={b.healthy ? 'ok' : 'danger'}>{b.healthy ? 'healthy' : 'down'}</Badge></td>
              <td title={b.weight_basis}>{b.effective_weight == null ? '–' : fmt(b.effective_weight, 3)}</td>
              <td>{pct(b.observed_share)}</td>
              <td>{b.active_conns}</td>
              <td>{fmt(b.rps, 1)}</td>
              <td>{fmt(b.p95_ms, 1, ' ms')}</td>
              <td>{pct(b.error_rate, 2)}</td>
              <td>{b.cpu_pct == null ? '–' : fmt(b.cpu_pct, 0, '%')}</td>
              <td>{b.memory_pct == null ? '–' : fmt(b.memory_pct, 0, '%')}</td>
            </tr>
          ))}
        </tbody>
      </table>
      <p className="rs-note">* {dash.host_resources.note} “–” means not measured (never shown as 0). Effective weight is shown only for weight-based strategies; traffic share is what was actually routed.</p>
    </section>
  );
}

// ------------------------------------------------------------------------------ decisions
export function DecisionPanel({ rows, learner }) {
  const counts = decisionCounts(rows);
  const ex = learner && learner.exploration;
  const drift = learner && learner.drift;
  const per = learner && learner.persistence;
  return (
    <section className="glass-panel rs-panel rs-wide" aria-label="decisions">
      <h2>Policy decisions</h2>
      <div className="rs-chips">
        <Badge tone="ok">{counts.accepted} accepted</Badge>
        <Badge tone="warn">{counts.rejected} rejected</Badge>
        {counts.failed > 0 && <Badge tone="danger">{counts.failed} agent failures</Badge>}
        <Badge tone="info">{counts.explore} explore</Badge>
        <Badge tone="muted">{counts.exploit} exploit</Badge>
        {ex && ex.guard && <Badge tone="muted" title="Cap on repeated exploration">explore budget {pct(ex.guard.max_fraction, 0)}, {ex.guard.overrides} overridden</Badge>}
        {drift && <Badge tone={drift.drift_suspected ? 'warn' : 'ok'} title={(drift.reasons || []).join('; ')}>{drift.drift_suspected ? 'drift suspected' : 'no drift detected'}</Badge>}
        {per && per.last_checkpoint && <Badge tone="muted">checkpoint {per.last_checkpoint.updates}u · {per.last_checkpoint.reason}</Badge>}
      </div>
      <table className="rs-table">
        <thead>
          <tr><th>Time</th><th>Decision</th><th>Current → proposed</th><th>Choice</th><th>Policy</th><th>Reason</th><th>UCB est.</th><th>Reward</th><th>Latency</th></tr>
        </thead>
        <tbody>
          {rows.length === 0 && <tr><td colSpan="9" className="rs-muted">no decisions yet</td></tr>}
          {rows.slice().reverse().slice(0, 25).map((r) => (
            <tr key={r.id}>
              <td>{r.at ? new Date(r.at).toLocaleTimeString([], { hour12: false }) : '–'}</td>
              <td>{r.id}</td>
              <td>{r.current || '–'} → {r.proposed || '–'}</td>
              <td>{r.choice ? <Badge tone={r.choice === 'explore' ? 'info' : 'muted'}>{r.choice}{r.guardOverridden ? ' (capped)' : ''}</Badge> : '–'}</td>
              <td>{r.failure ? <Badge tone="danger">agent {r.failure}</Badge> : <Badge tone={r.accepted ? 'ok' : 'warn'}>{r.accepted ? 'accepted' : 'rejected'}</Badge>}</td>
              <td className="rs-muted">{r.reason}</td>
              <td>{r.ucbEstimate == null ? '–' : fmt(r.ucbEstimate, 3)}</td>
              <td>{r.reward == null ? '–' : <>{fmt(r.reward, 3)}{r.override ? <Badge tone="danger" title="safety override">{r.override}</Badge> : null}</>}</td>
              <td>{r.latencyMs == null ? '–' : fmt(r.latencyMs, 1, ' ms')}</td>
            </tr>
          ))}
        </tbody>
      </table>
      <p className="rs-note" data-testid="confidence-notice">{CONFIDENCE_NOTICE}</p>
    </section>
  );
}

// -------------------------------------------------------------------------------- whole view
export function ResearchView({ dash, points, events, learner, agentStatus, now = 0, onToggleAI = () => {}, onReset = () => {}, busy = false, errors = {} }) {
  if (!dash) {
    return <div className="glass-panel rs-panel">Waiting for the load balancer… {errors.dashboard ? <span className="rs-muted">({errors.dashboard})</span> : null}</div>;
  }
  const t0 = points.length ? points[0].t : now - 60000;
  const t1 = points.length ? points[points.length - 1].t : now;
  const markers = eventMarkers(events, t0, t1);
  const latency = [
    { name: 'p50', color: '#6c8cff', points: seriesFromPoints(points, (p) => (p.window_valid ? p.p50_ms : null)) },
    { name: 'p95', color: '#f2b84b', points: seriesFromPoints(points, (p) => (p.window_valid ? p.p95_ms : null)) },
    { name: 'p99', color: '#ff7a7a', points: seriesFromPoints(points, (p) => (p.window_valid ? p.p99_ms : null)) },
  ];
  const throughput = [{ name: 'req/s', color: '#35c2a1', points: seriesFromPoints(points, (p) => p.rps) }];
  const errors_ = [{ name: 'error rate', color: '#ff9f43', points: seriesFromPoints(points, (p) => p.error_rate * 100) }];
  const reward = (learner && learner.reward_series) || [];
  const comps = rewardComponentSeries(reward);
  const compSeries = REWARD_COMPONENTS.map((c) => ({ name: c, color: COMPONENT_COLORS[c], points: comps[c] }));
  const cumSeries = [{ name: 'cumulative reward', color: '#6c8cff', points: cumulativeSeries(reward) }];
  const rows = mergeDecisions(agentStatus && agentStatus.recent_decisions, learner && learner.recent_decisions);
  const switchMarks = markers.filter((m) => m.kind !== 'health');
  return (
    <div className="rs-grid">
      <StatusPanel dash={dash} learner={learner} now={now} onToggleAI={onToggleAI} onReset={onReset} busy={busy} />
      <BackendTable dash={dash} />

      <section className="glass-panel rs-panel rs-wide" aria-label="performance">
        <h2>Performance over time</h2>
        <div className="rs-marker-legend">
          {Object.entries(EVENT_STYLE).filter(([k]) => ['fault', 'policy', 'safety', 'kill', 'resume', 'reset', 'health'].includes(k)).map(([k, s]) => (
            <span key={k} className="rs-legend-item"><i style={{ background: s.color }} /> {s.label}</span>
          ))}
        </div>
        <TimeSeriesChart id="chart-latency" title="Latency (ms, request window)" series={latency} markers={markers} unit=" ms" />
        <TimeSeriesChart id="chart-throughput" title="Throughput (req/s)" series={throughput} markers={markers} />
        <TimeSeriesChart id="chart-errors" title="Error rate (%)" series={errors_} markers={markers} unit="%" />
        <h3>Strategy history</h3>
        <StrategyStrip points={points} />
        <p className="rs-note">{events.filter((e) => e.kind === 'fault').length} fault injection(s) in the log. Markers come from the load generator (faults) and the controller (switches, safety, kill switch). Hover a marker for its detail.</p>
      </section>

      <section className="glass-panel rs-panel rs-wide" aria-label="reward">
        <h2>Reward</h2>
        {learner && learner.reward_config ? (
          <p className="rs-note">R = {learner.reward_config.w_throughput}·T − {learner.reward_config.w_latency}·L95 − {learner.reward_config.w_error}·E − {learner.reward_config.w_imbalance}·V
            {' '}(safety override {learner.reward_config.safety_penalty} at error rate ≥ {learner.reward_config.severe_error_rate}). Components are the signed contributions to R.</p>
        ) : <p className="rs-muted">{(learner && learner.error) || 'learner view not available (agent not configured or unreachable)'}</p>}
        <TimeSeriesChart id="chart-components" title="Reward components per evaluated window" series={compSeries} timeAxis={false} zeroLine />
        <TimeSeriesChart id="chart-cumulative" title="Cumulative reward" series={cumSeries} timeAxis={false} zeroLine />
      </section>

      <DecisionPanel rows={rows} learner={learner} />
      {switchMarks.length > 0 && (
        <section className="glass-panel rs-panel rs-wide" aria-label="events">
          <h2>Event log</h2>
          <ul className="rs-events">
            {events.slice(-12).reverse().map((e) => (
              <li key={e.id}><Badge tone="muted">{e.kind}</Badge> {new Date(e.at).toLocaleTimeString([], { hour12: false })} {e.backend ? `${e.backend}: ` : ''}{e.detail}</li>
            ))}
          </ul>
        </section>
      )}
    </div>
  );
}

export default ResearchView;
