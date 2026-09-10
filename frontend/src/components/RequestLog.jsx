import React from 'react';

export function RequestLog({ logs, onClear }) {
  return (
    <div className="glass-panel request-log-panel">
      <div className="panel-header">
        <div>
          <h2 className="panel-title">Real-Time Request Execution Log</h2>
          <span className="topology-subtitle">Live packet telemetry and latency metrics</span>
        </div>
        <div className="log-actions">
          <span className="badge badge-cyan">{logs.length} Logged Events</span>
          <button className="btn btn-secondary" onClick={onClear} title="Clear log entries">
            Clear Logs
          </button>
        </div>
      </div>

      <div className="table-wrapper">
        <table className="log-table">
          <thead>
            <tr>
              <th>Time</th>
              <th>Request ID</th>
              <th>Client IP</th>
              <th>Strategy</th>
              <th>Target Backend</th>
              <th>Active Conns</th>
              <th>Latency</th>
              <th>Status</th>
            </tr>
          </thead>
          <tbody>
            {logs.length === 0 ? (
              <tr>
                <td colSpan="8" className="empty-table">
                  No requests captured yet. Click "Send 1 Request" or "Burst" above to start live testing!
                </td>
              </tr>
            ) : (
              logs.map((log) => {
                const isComplete = log.type === 'complete';
                const timeStr = new Date(log.timestamp).toLocaleTimeString([], {
                  hour: '2-digit',
                  minute: '2-digit',
                  second: '2-digit',
                  fractionalSecondDigits: 3,
                });

                return (
                  <tr key={`${log.id}-${log.type}`} className={`log-row ${isComplete ? 'row-complete' : 'row-start'}`}>
                    <td className="text-mono text-muted">{timeStr}</td>
                    <td className="text-mono text-cyan">{log.id}</td>
                    <td className="text-mono">{log.client_ip || '127.0.0.1'}</td>
                    <td>
                      <span className="badge badge-purple">{log.strategy}</span>
                    </td>
                    <td>
                      <span className="text-emerald font-semibold">{log.backend_name}</span>
                    </td>
                    <td className="text-mono text-amber">{log.active_conns}</td>
                    <td className="text-mono">
                      {isComplete ? `${log.duration_ms} ms` : <span className="in-flight-dots">in-flight...</span>}
                    </td>
                    <td>
                      {isComplete ? (
                        <span className="badge badge-emerald">200 OK</span>
                      ) : (
                        <span className="badge badge-amber">Processing</span>
                      )}
                    </td>
                  </tr>
                );
              })
            )}
          </tbody>
        </table>
      </div>
    </div>
  );
}
