import React, { useEffect, useRef } from 'react';

export function TopologyCanvas({ backends, activeStrategy, activeEvents }) {
  const canvasRef = useRef(null);

  // Define node coordinate positions in SVG space (viewBox 0 0 900 360)
  const clientPos = { x: 100, y: 180 };
  const lbPos = { x: 400, y: 180 };
  const backendPositions = {
    'Backend-1': { x: 740, y: 70 },
    'Backend-2': { x: 740, y: 180 },
    'Backend-3': { x: 740, y: 290 },
  };

  // Helper to construct curved SVG path
  const makeBezier = (start, end) => {
    const midX = (start.x + end.x) / 2;
    return `M ${start.x} ${start.y} C ${midX} ${start.y}, ${midX} ${end.y}, ${end.x} ${end.y}`;
  };

  return (
    <div className="glass-panel topology-container">
      <div className="topology-header">
        <div className="topology-title-group">
          <h2 className="panel-title">Live Request Flow Topology</h2>
          <span className="topology-subtitle">Real-time packet trajectories & node connection states</span>
        </div>
        <span className="badge badge-cyan">Strategy: {activeStrategy}</span>
      </div>

      <div className="svg-wrapper">
        <svg
          ref={canvasRef}
          className="topology-svg"
          viewBox="0 0 880 360"
          preserveAspectRatio="xMidYMid meet"
        >
          <defs>
            {/* Glow filters */}
            <filter id="glow-cyan" x="-20%" y="-20%" width="140%" height="140%">
              <feGaussianBlur stdDeviation="6" result="blur" />
              <feMerge>
                <feMergeNode in="blur" />
                <feMergeNode in="SourceGraphic" />
              </feMerge>
            </filter>
            <filter id="glow-emerald" x="-20%" y="-20%" width="140%" height="140%">
              <feGaussianBlur stdDeviation="6" result="blur" />
              <feMerge>
                <feMergeNode in="blur" />
                <feMergeNode in="SourceGraphic" />
              </feMerge>
            </filter>

            {/* Gradient definition for lines */}
            <linearGradient id="line-gradient" x1="0%" y1="0%" x2="100%" y2="0%">
              <stop offset="0%" stopColor="#00f0ff" stopOpacity="0.3" />
              <stop offset="100%" stopColor="#8b5cf6" stopOpacity="0.3" />
            </linearGradient>
          </defs>

          {/* 1. Static connection lines */}
          {/* Client to LB */}
          <path
            d={makeBezier(clientPos, lbPos)}
            fill="none"
            stroke="url(#line-gradient)"
            strokeWidth="2.5"
            strokeDasharray="6 4"
            className="wire-line"
          />

          {/* LB to Backends */}
          {backends.map((b) => {
            const bPos = backendPositions[b.name] || { x: 740, y: 180 };
            return (
              <path
                key={`wire-${b.name}`}
                d={makeBezier(lbPos, bPos)}
                fill="none"
                stroke={b.alive ? 'url(#line-gradient)' : 'rgba(244, 63, 94, 0.25)'}
                strokeWidth={b.alive ? '2.5' : '1.5'}
                strokeDasharray={b.alive ? '6 4' : '3 3'}
                className="wire-line"
              />
            );
          })}

          {/* 2. Dynamic Flying Request Particles */}
          {activeEvents.map((evt) => {
            const bPos = backendPositions[evt.backend_name] || { x: 740, y: 180 };
            const fullPath = `M ${clientPos.x} ${clientPos.y} C ${(clientPos.x + lbPos.x) / 2} ${clientPos.y}, ${(clientPos.x + lbPos.x) / 2} ${lbPos.y}, ${lbPos.x} ${lbPos.y} C ${(lbPos.x + bPos.x) / 2} ${lbPos.y}, ${(lbPos.x + bPos.x) / 2} ${bPos.y}, ${bPos.x} ${bPos.y}`;

            return (
              <g key={evt.id} className="active-particle-group">
                <circle r="6" fill="#00f0ff" filter="url(#glow-cyan)">
                  <animateMotion
                    path={fullPath}
                    dur="0.8s"
                    repeatCount="1"
                    fill="freeze"
                  />
                  <animate
                    attributeName="opacity"
                    values="1;1;0"
                    keyTimes="0;0.85;1"
                    dur="0.8s"
                    fill="freeze"
                  />
                </circle>
              </g>
            );
          })}

          {/* 3. Node representations */}
          {/* CLIENT NODE */}
          <g transform={`translate(${clientPos.x}, ${clientPos.y})`} className="node-group">
            <circle r="36" fill="#111827" stroke="#00f0ff" strokeWidth="2" filter="url(#glow-cyan)" />
            <text textAnchor="middle" dy="-4" fill="#f8fafc" fontSize="18">🌐</text>
            <text textAnchor="middle" dy="16" fill="#00f0ff" fontSize="10" fontWeight="600" fontFamily="Inter">CLIENTS</text>
            <text textAnchor="middle" dy="48" fill="#64748b" fontSize="11" fontFamily="Inter">Incoming HTTP</text>
          </g>

          {/* LOAD BALANCER CORE */}
          <g transform={`translate(${lbPos.x}, ${lbPos.y})`} className="node-group">
            <rect
              x="-54"
              y="-40"
              width="108"
              height="80"
              rx="16"
              fill="#111827"
              stroke="#8b5cf6"
              strokeWidth="2.5"
              filter="url(#glow-emerald)"
            />
            <text textAnchor="middle" dy="-14" fill="#a855f7" fontSize="11" fontWeight="700" fontFamily="Outfit">
              LOAD BALANCER
            </text>
            <text textAnchor="middle" dy="6" fill="#f8fafc" fontSize="14" fontWeight="600" fontFamily="JetBrains Mono">
              :8080
            </text>
            <text textAnchor="middle" dy="24" fill="#00f0ff" fontSize="10" fontFamily="Inter">
              Reverse Proxy
            </text>
          </g>

          {/* BACKEND NODES */}
          {backends.map((b) => {
            const pos = backendPositions[b.name] || { x: 740, y: 180 };
            const isAlive = b.alive;
            const strokeColor = isAlive ? '#10b981' : '#f43f5e';

            return (
              <g
                key={b.name}
                transform={`translate(${pos.x}, ${pos.y})`}
                className={`node-group ${isAlive ? '' : 'dead-node'}`}
              >
                <rect
                  x="-70"
                  y="-34"
                  width="140"
                  height="68"
                  rx="12"
                  fill="#111827"
                  stroke={strokeColor}
                  strokeWidth="2"
                  filter={isAlive ? 'url(#glow-emerald)' : 'none'}
                />
                <text textAnchor="middle" dy="-12" fill={isAlive ? '#f8fafc' : '#f43f5e'} fontSize="13" fontWeight="700" fontFamily="Outfit">
                  {b.name}
                </text>
                <text textAnchor="middle" dy="6" fill="#94a3b8" fontSize="11" fontFamily="JetBrains Mono">
                  {b.url.replace('http://localhost', ':')}
                </text>
                <text textAnchor="middle" dy="22" fill={isAlive ? '#10b981' : '#f43f5e'} fontSize="10" fontWeight="600" fontFamily="Inter">
                  {isAlive ? `● UP | ${b.active_conns} in-flight` : '✖ DOWN (OFFLINE)'}
                </text>
              </g>
            );
          })}
        </svg>
      </div>
    </div>
  );
}
