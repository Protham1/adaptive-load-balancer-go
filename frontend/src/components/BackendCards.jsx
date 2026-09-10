import React from 'react';

export function BackendCards({ backends, onToggleBackend, onWeightChange }) {
  return (
    <div className="glass-panel backend-cards-panel">
      <div className="panel-header">
        <h2 className="panel-title">Upstream Server Cluster</h2>
        <span className="badge badge-emerald">Live Cluster Status</span>
      </div>

      <div className="backends-grid">
        {backends.map((b) => {
          const isAlive = b.alive;
          return (
            <div key={b.name} className={`backend-card ${isAlive ? 'is-alive' : 'is-dead'}`}>
              <div className="card-top">
                <div>
                  <div className="card-name-row">
                    <h3 className="card-name">{b.name}</h3>
                    <span className={`pulse-dot ${isAlive ? 'online' : 'offline'}`}></span>
                  </div>
                  <div className="card-url">{b.url}</div>
                </div>

                <button
                  id={`toggle-btn-${b.name}`}
                  className={`btn ${isAlive ? 'btn-danger' : 'btn-success'}`}
                  onClick={() => onToggleBackend(b.name)}
                  title={isAlive ? 'Simulate server crash / outage' : 'Revive server'}
                >
                  {isAlive ? '✖ Kill Node' : '✓ Revive Node'}
                </button>
              </div>

              {/* Metrics Section */}
              <div className="card-metrics">
                <div className="metric-box">
                  <span className="metric-label">In-Flight Conns</span>
                  <div className="metric-val-row">
                    <span className="metric-val text-amber">{b.active_conns}</span>
                    <div className="mini-meter">
                      <div
                        className="mini-meter-fill"
                        style={{ width: `${Math.min(100, b.active_conns * 20)}%` }}
                      ></div>
                    </div>
                  </div>
                </div>

                <div className="metric-box">
                  <span className="metric-label">Total Requests</span>
                  <span className="metric-val text-cyan">{b.total_requests || 0}</span>
                </div>
              </div>

              {/* Weight Config Slider */}
              <div className="card-weight-section">
                <div className="weight-label-row">
                  <span className="weight-label">Capacity Weight (SWRR):</span>
                  <span className="weight-val badge badge-purple">{b.weight}x</span>
                </div>
                <input
                  type="range"
                  min="1"
                  max="10"
                  value={b.weight}
                  onChange={(e) => onWeightChange(b.name, Number(e.target.value))}
                  className="range-slider"
                  disabled={!isAlive}
                />
              </div>
            </div>
          );
        })}
      </div>
    </div>
  );
}
