import React from 'react';

const EXPLANATIONS = {
  'Round-Robin': {
    title: 'Atomic Round-Robin',
    badge: 'Static',
    description: 'Sequentially distributes incoming requests one by one across all healthy backends using lock-free atomic counters. Skips unhealthy nodes immediately.',
    tip: 'Best for uniform backend capacities and short stateless requests. Watch how requests cycle rhythmically 1 -> 2 -> 3.',
  },
  'Least-Connections': {
    title: 'Least-Connections (LeastConn)',
    badge: 'Dynamic / State-Aware',
    description: 'Maintains atomic in-flight connection counters per backend. Dynamically routes each new request to the backend currently executing the fewest simultaneous requests. Ties are broken via Round-Robin.',
    tip: 'Test this by dragging the "Simulate Processing Delay" slider to 500ms and sending a burst. Watch how the load balancer prevents traffic from piling up on busy servers!',
  },
  'Smooth-Weighted-Round-Robin': {
    title: 'Smooth Weighted Round-Robin (SWRR)',
    badge: 'Weighted / Deterministic',
    description: 'Nginx’s algorithm that distributes load proportionally to configured hardware weights (e.g. 3x vs 2x vs 1x), while interleaving requests smoothly (A, B, A, C, A, B) to prevent bursts.',
    tip: 'Adjust the weight sliders on the backend cards to see immediate distribution ratio changes without restart.',
  },
  'Consistent-Hash': {
    title: 'Consistent Hashing (Ketama Hash Ring)',
    badge: 'Stateful / Affinity',
    description: 'Maps client IP addresses onto a 32-bit circular hash ring populated with 100 virtual nodes per backend. Guarantees deterministic session stickiness while minimizing re-hashes if backends go down.',
    tip: 'Select different Client IPs in the traffic generator. Notice how Client Alpha always hits the exact same backend, while Client Beta routes to another!',
  },
};

export function StrategyExplainer({ activeStrategy }) {
  const info = EXPLANATIONS[activeStrategy] || EXPLANATIONS['Round-Robin'];

  return (
    <div className="glass-panel strategy-explainer-panel">
      <div className="explainer-header">
        <div className="explainer-title-row">
          <span className="explainer-icon">💡</span>
          <h3 className="explainer-title">How {info.title} Operates</h3>
        </div>
        <span className="badge badge-purple">{info.badge}</span>
      </div>

      <p className="explainer-desc">{info.description}</p>
      
      <div className="explainer-tip-box">
        <span className="tip-label">Live Demonstration Tip:</span>
        <span className="tip-text">{info.tip}</span>
      </div>
    </div>
  );
}
