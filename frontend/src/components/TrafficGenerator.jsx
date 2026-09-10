import React, { useState, useEffect, useRef } from 'react';

const PRESET_IPS = [
  { label: 'Auto (Real Remote IP)', value: '' },
  { label: 'Client Alpha (192.168.1.10)', value: '192.168.1.10' },
  { label: 'Client Beta (10.0.0.5)', value: '10.0.0.5' },
  { label: 'Client Gamma (172.16.0.42)', value: '172.16.0.42' },
];

export function TrafficGenerator({ onSendRequest }) {
  const [selectedIP, setSelectedIP] = useState('');
  const [customIP, setCustomIP] = useState('');
  const [delayMs, setDelayMs] = useState(0);
  const [continuousRps, setContinuousRps] = useState(3);
  const [isStreaming, setIsStreaming] = useState(false);
  const streamIntervalRef = useRef(null);

  const getEffectiveIP = () => {
    return customIP.trim() || selectedIP;
  };

  const getEffectiveDelay = () => {
    return delayMs > 0 ? `${delayMs}ms` : '';
  };

  const handleSingle = () => {
    onSendRequest({ clientIP: getEffectiveIP(), delay: getEffectiveDelay() });
  };

  const handleBurst = (count) => {
    for (let i = 0; i < count; i++) {
      setTimeout(() => {
        onSendRequest({ clientIP: getEffectiveIP(), delay: getEffectiveDelay() });
      }, i * 35);
    }
  };

  const toggleStreaming = () => {
    if (isStreaming) {
      clearInterval(streamIntervalRef.current);
      setIsStreaming(false);
    } else {
      setIsStreaming(true);
    }
  };

  useEffect(() => {
    if (isStreaming) {
      const intervalMs = Math.max(50, 1000 / continuousRps);
      streamIntervalRef.current = setInterval(() => {
        onSendRequest({ clientIP: getEffectiveIP(), delay: getEffectiveDelay() });
      }, intervalMs);
    } else {
      clearInterval(streamIntervalRef.current);
    }
    return () => clearInterval(streamIntervalRef.current);
  }, [isStreaming, continuousRps, selectedIP, customIP, delayMs]);

  return (
    <div className="glass-panel traffic-generator-panel">
      <div className="panel-header">
        <h2 className="panel-title">Traffic Simulation & Load Testing</h2>
        <span className="badge badge-purple">Interactive</span>
      </div>

      <div className="generator-controls">
        {/* Trigger Buttons */}
        <div className="control-group">
          <label className="control-label">Manual Triggers</label>
          <div className="button-row">
            <button id="send-single-btn" className="btn btn-primary" onClick={handleSingle}>
              ⚡ Send 1 Request
            </button>
            <button id="send-burst-10-btn" className="btn btn-secondary" onClick={() => handleBurst(10)}>
              💥 Burst (10 Reqs)
            </button>
            <button id="send-burst-25-btn" className="btn btn-secondary" onClick={() => handleBurst(25)}>
              🔥 Burst (25 Reqs)
            </button>
          </div>
        </div>

        {/* Continuous Stream */}
        <div className="control-group">
          <div className="label-with-val">
            <label className="control-label">Continuous Generator (RPS)</label>
            <span className="control-val">{continuousRps} req/s</span>
          </div>
          <div className="slider-row">
            <input
              type="range"
              min="1"
              max="20"
              value={continuousRps}
              onChange={(e) => setContinuousRps(Number(e.target.value))}
              className="range-slider"
            />
            <button
              id="continuous-toggle-btn"
              className={`btn ${isStreaming ? 'btn-danger' : 'btn-success'}`}
              onClick={toggleStreaming}
            >
              {isStreaming ? '⏹ Stop Stream' : '▶ Start Stream'}
            </button>
          </div>
        </div>

        {/* Client IP Selector for Consistent Hashing */}
        <div className="control-group">
          <label className="control-label">
            Client IP / Session Key
            <span className="hint-text"> (Demonstrates Consistent Hashing affinity)</span>
          </label>
          <div className="ip-selector-row">
            <select
              className="select-input"
              value={selectedIP}
              onChange={(e) => {
                setSelectedIP(e.target.value);
                setCustomIP('');
              }}
            >
              {PRESET_IPS.map((p) => (
                <option key={p.value} value={p.value}>{p.label}</option>
              ))}
            </select>
            <input
              type="text"
              placeholder="Or custom IP / Key..."
              value={customIP}
              onChange={(e) => setCustomIP(e.target.value)}
              className="text-input"
            />
          </div>
        </div>

        {/* Simulated Delay for Least Connections */}
        <div className="control-group">
          <div className="label-with-val">
            <label className="control-label">
              Simulate Processing Delay
              <span className="hint-text"> (Demonstrates Least Connections routing away from slow nodes)</span>
            </label>
            <span className="control-val">{delayMs} ms</span>
          </div>
          <input
            type="range"
            min="0"
            max="1500"
            step="50"
            value={delayMs}
            onChange={(e) => setDelayMs(Number(e.target.value))}
            className="range-slider"
          />
        </div>
      </div>
    </div>
  );
}
