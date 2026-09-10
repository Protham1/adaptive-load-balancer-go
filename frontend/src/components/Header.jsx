import React from 'react';

const STRATEGIES = [
  { id: 'Round-Robin', label: 'Round-Robin', icon: '🔄', desc: 'Atomic sequential rotation' },
  { id: 'Least-Connections', label: 'Least-Connections', icon: '⚖️', desc: 'Routes to lowest active conns' },
  { id: 'Smooth-Weighted-Round-Robin', label: 'Smooth-Weighted', icon: '📊', desc: 'Nginx smooth capacity interleaving' },
  { id: 'Consistent-Hash', label: 'Consistent-Hash', icon: '🎯', desc: '32-bit FNV-1a IP/key affinity ring' },
];

export function Header({ activeStrategy, onStrategyChange, isConnected, onRefresh }) {
  return (
    <header className="glass-panel app-header">
      <div className="header-brand">
        <div className="brand-icon">⚡</div>
        <div>
          <h1 className="brand-title">Adaptive Load Balancer</h1>
          <div className="brand-subtitle">
            <span className={`pulse-dot ${isConnected ? 'online' : 'offline'}`}></span>
            <span>{isConnected ? 'Connected to LB (:8080)' : 'Reconnecting to :8080...'}</span>
          </div>
        </div>
      </div>

      <div className="strategy-selector-container">
        <span className="selector-label">Active Strategy:</span>
        <div className="strategy-pills">
          {STRATEGIES.map((strat) => {
            const isActive = activeStrategy === strat.id;
            return (
              <button
                key={strat.id}
                id={`strategy-btn-${strat.id}`}
                className={`strategy-pill ${isActive ? 'active' : ''}`}
                onClick={() => onStrategyChange(strat.id)}
                title={strat.desc}
              >
                <span className="pill-icon">{strat.icon}</span>
                <span className="pill-name">{strat.label}</span>
                {isActive && <span className="active-glow"></span>}
              </button>
            );
          })}
        </div>
      </div>

      <div className="header-actions">
        <button className="btn btn-secondary" onClick={onRefresh} title="Sync status with load balancer">
          ↻ Sync
        </button>
      </div>
    </header>
  );
}
