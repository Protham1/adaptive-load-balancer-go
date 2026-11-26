import React, { useCallback, useEffect, useRef, useState } from 'react';
import { ResearchView } from './ResearchView';
import { mergeEvents, mergePoints } from '../../lib/dashboardData';
import { fetchAgentStatus, fetchDashboard, fetchEventLog, fetchHistory, fetchLearner, resetAgent, setAIKill } from '../../services/api';

/** Polling container: dashboard + history every 2 s, events 2 s, learner + agent log every 4 s. Sources fail independently. */
export function ResearchDashboard() {
  const [dash, setDash] = useState(null);
  const [points, setPoints] = useState([]);
  const [events, setEvents] = useState([]);
  const [learner, setLearner] = useState(null);
  const [agentStatus, setAgentStatus] = useState(null);
  const [errors, setErrors] = useState({});
  const [busy, setBusy] = useState(false);
  const [now, setNow] = useState(() => Date.now());
  const sinceT = useRef(0);
  const sinceE = useRef(0);

  const setErr = (k, v) => setErrors((e) => (e[k] === v ? e : { ...e, [k]: v }));

  const pollFast = useCallback(async () => {
    setNow(Date.now());
    try {
      setDash(await fetchDashboard());
      setErr('dashboard', null);
    } catch (e) {
      setErr('dashboard', e.message);
    }
    try {
      const h = await fetchHistory(sinceT.current);
      if (h.points && h.points.length) {
        sinceT.current = h.points[h.points.length - 1].t;
        setPoints((p) => mergePoints(p, h.points));
      }
      setErr('history', null);
    } catch (e) {
      setErr('history', e.message);
    }
    try {
      const ev = await fetchEventLog(sinceE.current);
      if (ev.events && ev.events.length) {
        sinceE.current = ev.last_id;
        setEvents((p) => mergeEvents(p, ev.events));
      }
      setErr('events', null);
    } catch (e) {
      setErr('events', e.message);
    }
  }, []);

  const pollSlow = useCallback(async () => {
    try {
      const l = await fetchLearner(100);
      setLearner(l.available === false ? { error: l.error } : l);
    } catch (e) {
      setLearner({ error: e.message });
    }
    try {
      setAgentStatus(await fetchAgentStatus());
    } catch {
      setAgentStatus(null);
    }
  }, []);

  useEffect(() => {
    pollFast();
    pollSlow();
    const a = setInterval(pollFast, 2000);
    const b = setInterval(pollSlow, 4000);
    return () => {
      clearInterval(a);
      clearInterval(b);
    };
  }, [pollFast, pollSlow]);

  const act = async (fn) => {
    setBusy(true);
    try {
      await fn();
    } catch (e) {
      setErr('action', e.message);
    } finally {
      setBusy(false);
      pollFast();
      pollSlow();
    }
  };

  return (
    <>
      {errors.action && <div className="rs-error" role="alert">Action failed: {errors.action}</div>}
      <ResearchView
        dash={dash} points={points} events={events} learner={learner} agentStatus={agentStatus} now={now} busy={busy} errors={errors}
        onToggleAI={(kill) => act(() => setAIKill(kill))}
        onReset={() => {
          if (window.confirm('Reset to baseline? The baseline strategy is applied immediately and the learned model is archived and restarted.')) {
            act(() => resetAgent(true));
          }
        }}
      />
    </>
  );
}

export default ResearchDashboard;
