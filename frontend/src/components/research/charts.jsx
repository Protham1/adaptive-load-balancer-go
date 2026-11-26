import React from 'react';
import { STRATEGY_COLORS, extent, fmt, linePath, niceTicks, scaleLinear, segmentStrategies } from '../../lib/dashboardData';

const W = 640;
const PAD = { l: 52, r: 12, t: 10, b: 26 };

/**
 * Dependency-free SVG line chart.
 * series:  [{ name, color, points: [{ t, v }] }]   (v === null => gap, never zero)
 * markers: [{ t, color, label, detail }]           vertical lines on the time axis (fault injections, switches, ...)
 * timeAxis: x values are unix ms (rendered as HH:MM:SS) or plain indices (decision number)
 */
export function TimeSeriesChart({ title, series, markers = [], unit = '', height = 190, timeAxis = true, zeroLine = false, id }) {
  const H = height;
  const allPoints = series.flatMap((s) => s.points);
  const hasData = allPoints.some((p) => p.v != null && !Number.isNaN(p.v));
  const [x0, x1] = extent(series, 't');
  let [y0, y1] = extent(series, 'v');
  if (zeroLine) {
    y0 = Math.min(y0, 0);
    y1 = Math.max(y1, 0);
  }
  if (y0 === y1) {
    y0 -= 1;
    y1 += 1;
  }
  const pad = (y1 - y0) * 0.08;
  const lo = zeroLine ? y0 : Math.max(0, y0 - pad) === 0 && y0 >= 0 ? 0 : y0 - pad;
  const hi = y1 + pad;
  const xs = scaleLinear([x0, x1 === x0 ? x0 + 1 : x1], [PAD.l, W - PAD.r]);
  const ys = scaleLinear([lo, hi], [H - PAD.b, PAD.t]);
  const yTicks = niceTicks(lo, hi, 4);
  const xTicks = niceTicks(x0, x1, 5);
  const fmtX = (t) => (timeAxis ? new Date(t).toLocaleTimeString([], { hour12: false }) : String(Math.round(t)));

  return (
    <figure className="rs-chart" id={id}>
      <figcaption className="rs-chart-title">
        <span>{title}</span>
        <span className="rs-legend">
          {series.map((s) => {
            const last = [...s.points].reverse().find((p) => p.v != null && !Number.isNaN(p.v));
            return (
              <span key={s.name} className="rs-legend-item">
                <i style={{ background: s.color }} /> {s.name}
                {last ? <b> {fmt(last.v, Math.abs(last.v) < 1 ? 3 : 1)}{unit}</b> : null}
              </span>
            );
          })}
        </span>
      </figcaption>
      <svg viewBox={`0 0 ${W} ${H}`} role="img" aria-label={title} className="rs-svg">
        {yTicks.map((v) => (
          <g key={`y${v}`}>
            <line x1={PAD.l} x2={W - PAD.r} y1={ys(v)} y2={ys(v)} className="rs-gridline" />
            <text x={PAD.l - 6} y={ys(v) + 3} className="rs-axis" textAnchor="end">{fmt(v, Math.abs(v) < 10 && v % 1 ? 2 : 0)}</text>
          </g>
        ))}
        {xTicks.map((t) => (
          <text key={`x${t}`} x={xs(t)} y={H - 8} className="rs-axis" textAnchor="middle">{fmtX(t)}</text>
        ))}
        {markers.filter((m) => m.t >= x0 && m.t <= x1).map((m, i) => (
          <g key={`m${i}`} className="rs-marker">
            <line x1={xs(m.t)} x2={xs(m.t)} y1={PAD.t} y2={H - PAD.b} stroke={m.color} strokeDasharray="4 3" />
            <circle cx={xs(m.t)} cy={PAD.t + 2} r="3.5" fill={m.color} />
            <title>{`${m.label}${m.detail ? ` — ${m.detail}` : ''}`}</title>
          </g>
        ))}
        {series.map((s) => (
          <path key={s.name} d={linePath(s.points, xs, ys)} fill="none" stroke={s.color} strokeWidth="1.8" />
        ))}
        {!hasData && <text x={W / 2} y={H / 2} className="rs-axis" textAnchor="middle">no data yet</text>}
      </svg>
    </figure>
  );
}

/** Horizontal strip showing which strategy was active over the visible period. */
export function StrategyStrip({ points }) {
  const segs = segmentStrategies(points);
  if (segs.length === 0) return <div className="rs-muted">no history yet</div>;
  const t0 = segs[0].start;
  const t1 = Math.max(segs[segs.length - 1].end, t0 + 1);
  const xs = scaleLinear([t0, t1], [0, W]);
  const names = [...new Set(segs.map((s) => s.strategy))];
  return (
    <div className="rs-strip">
      <svg viewBox={`0 0 ${W} 28`} role="img" aria-label="strategy history" className="rs-svg">
        {segs.map((s, i) => (
          <rect key={i} x={xs(s.start)} y="2" width={Math.max(2, xs(s.end) - xs(s.start))} height="22" rx="3"
            fill={STRATEGY_COLORS[s.strategy] || '#8a93a6'}>
            <title>{`${s.strategy} (policy v${s.version})`}</title>
          </rect>
        ))}
      </svg>
      <div className="rs-legend">
        {names.map((n) => (
          <span key={n} className="rs-legend-item"><i style={{ background: STRATEGY_COLORS[n] || '#8a93a6' }} /> {n}</span>
        ))}
        <span className="rs-muted">{segs.length} segment{segs.length === 1 ? '' : 's'}</span>
      </div>
    </div>
  );
}
