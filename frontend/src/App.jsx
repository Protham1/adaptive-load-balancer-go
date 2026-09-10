import React, { useState, useEffect, useCallback } from 'react';
import { Header } from './components/Header';
import { TopologyCanvas } from './components/TopologyCanvas';
import { TrafficGenerator } from './components/TrafficGenerator';
import { BackendCards } from './components/BackendCards';
import { StrategyExplainer } from './components/StrategyExplainer';
import { RequestLog } from './components/RequestLog';
import {
  fetchStatus,
  setStrategy,
  toggleBackend,
  setBackendWeight,
  sendTestRequest,
  subscribeEvents,
} from './services/api';
import './App.css';

export function App() {
  const [activeStrategy, setActiveStrategy] = useState('Round-Robin');
  const [backends, setBackends] = useState([]);
  const [isConnected, setIsConnected] = useState(false);
  const [activeEvents, setActiveEvents] = useState([]);
  const [logs, setLogs] = useState([]);

  // Load cluster state from Go load balancer
  const loadClusterStatus = useCallback(async () => {
    try {
      const data = await fetchStatus();
      setActiveStrategy(data.active_strategy);
      setBackends(data.backends);
      setIsConnected(true);
    } catch {
      setIsConnected(false);
    }
  }, []);

  useEffect(() => {
    loadClusterStatus();
    const interval = setInterval(loadClusterStatus, 4000);
    return () => clearInterval(interval);
  }, [loadClusterStatus]);

  // Subscribe to real-time Server-Sent Events
  useEffect(() => {
    const unsubscribe = subscribeEvents(
      (evt) => {
        setIsConnected(true);

        if (evt.type === 'start') {
          // Trigger SVG particle animation
          setActiveEvents((prev) => [...prev, evt]);
          setTimeout(() => {
            setActiveEvents((prev) => prev.filter((e) => e.id !== evt.id));
          }, 900);

          // Update backend in-flight conns locally for instant feedback
          setBackends((prev) =>
            prev.map((b) =>
              b.name === evt.backend_name
                ? { ...b, active_conns: evt.active_conns, total_requests: (b.total_requests || 0) + 1 }
                : b
            )
          );

          // Add to log feed
          setLogs((prev) => [evt, ...prev].slice(0, 50));
        } else if (evt.type === 'complete') {
          // Update matching log entry
          setLogs((prev) =>
            prev.map((l) => (l.id === evt.id ? { ...l, ...evt } : l))
          );

          // Update backend active conns
          setBackends((prev) =>
            prev.map((b) =>
              b.name === evt.backend_name
                ? { ...b, active_conns: evt.active_conns }
                : b
            )
          );
        }
      },
      () => {
        setIsConnected(false);
      }
    );

    return () => unsubscribe();
  }, []);

  const handleStrategyChange = async (strat) => {
    try {
      await setStrategy(strat);
      setActiveStrategy(strat);
    } catch (err) {
      console.error('Failed to change strategy:', err);
    }
  };

  const handleToggleBackend = async (name) => {
    try {
      const res = await toggleBackend(name);
      setBackends((prev) =>
        prev.map((b) => (b.name === name ? { ...b, alive: res.alive } : b))
      );
    } catch (err) {
      console.error('Failed to toggle backend:', err);
    }
  };

  const handleWeightChange = async (name, weight) => {
    try {
      await setBackendWeight(name, weight);
      setBackends((prev) =>
        prev.map((b) => (b.name === name ? { ...b, weight } : b))
      );
    } catch (err) {
      console.error('Failed to update weight:', err);
    }
  };

  const handleSendRequest = async (params) => {
    try {
      await sendTestRequest(params);
    } catch (err) {
      console.error('Request failed:', err);
    }
  };

  return (
    <div className="app-container">
      <Header
        activeStrategy={activeStrategy}
        onStrategyChange={handleStrategyChange}
        isConnected={isConnected}
        onRefresh={loadClusterStatus}
      />

      <main className="dashboard-grid">
        {/* Top: Dynamic Network Topology Flow */}
        <div className="grid-full-width">
          <TopologyCanvas
            backends={backends}
            activeStrategy={activeStrategy}
            activeEvents={activeEvents}
          />
        </div>

        {/* Middle Left: Traffic Generator */}
        <div className="grid-col-left">
          <TrafficGenerator onSendRequest={handleSendRequest} />
          <StrategyExplainer activeStrategy={activeStrategy} />
        </div>

        {/* Middle Right: Cluster Node Cards */}
        <div className="grid-col-right">
          <BackendCards
            backends={backends}
            onToggleBackend={handleToggleBackend}
            onWeightChange={handleWeightChange}
          />
        </div>

        {/* Bottom: Request Execution Feed */}
        <div className="grid-full-width">
          <RequestLog logs={logs} onClear={() => setLogs([])} />
        </div>
      </main>
    </div>
  );
}

export default App;
