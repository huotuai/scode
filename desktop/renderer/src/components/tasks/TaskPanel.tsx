import { useState } from 'react';
import { CheckCircle2, Loader2, RefreshCw, Square, Terminal, X, XCircle } from 'lucide-react';
import type { BackgroundTask } from '../../types';
import { useTasksStore, runningTaskCount } from '../../store/tasks';
import { useT } from '../../i18n';
import '../../styles/tasks.css';

function fmtDuration(ms: number): string {
  const s = Math.floor(ms / 1000);
  if (s < 60) return `${s}s`;
  const m = Math.floor(s / 60);
  if (m < 60) return `${m}m ${s % 60}s`;
  return `${Math.floor(m / 60)}h ${m % 60}m`;
}

function stateIcon(t: BackgroundTask) {
  if (t.state === 'running') return <Loader2 size={13} className="spin" />;
  if (t.state === 'killed') return <XCircle size={13} />;
  return <CheckCircle2 size={13} />;
}

function stateLabel(tr: ReturnType<typeof useT>, task: BackgroundTask): string {
  switch (task.state) {
    case 'running':
      return tr('taskPanel.running');
    case 'killed':
      return tr('taskPanel.killed');
    default:
      return task.exitCode === 0 ? tr('taskPanel.done') : tr('taskPanel.exitCode', { code: task.exitCode ?? -1 });
  }
}

export default function TaskPanel() {
  const tr = useT();
  const open = useTasksStore(s => s.open);
  const tasks = useTasksStore(s => s.tasks);
  const error = useTasksStore(s => s.error);
  const setOpen = useTasksStore(s => s.setOpen);
  const refresh = useTasksStore(s => s.refresh);
  const kill = useTasksStore(s => s.kill);
  const [expanded, setExpanded] = useState<Record<number, boolean>>({});

  if (!open) return null;
  const running = runningTaskCount(tasks);

  return (
    <div className="task-panel">
      <div className="task-panel-head">
        <Terminal size={14} />
        <span className="task-panel-title">{tr('taskPanel.title')}</span>
        {running > 0 && <span className="task-running-count">{tr('taskPanel.runningCount', { n: running })}</span>}
        <span className="spacer" />
        <button className="task-icon-btn" title={tr('common.refresh')} onClick={() => void refresh()}>
          <RefreshCw size={13} />
        </button>
        <button className="task-icon-btn" title={tr('common.close')} onClick={() => setOpen(false)}>
          <X size={13} />
        </button>
      </div>

      {error && <div className="task-error">{error}</div>}

      {tasks.length === 0 ? (
        <div className="task-empty">
          {tr('taskPanel.empty')}
        </div>
      ) : (
        <ul className="task-list">
          {tasks.map(t => (
            <li key={t.id} className={`task-item ${t.state}`}>
              <div className="task-row">
                <span className="task-state" title={stateLabel(tr, t)}>
                  {stateIcon(t)}
                </span>
                <span className="task-id">#{t.id}</span>
                <span className="task-state-label">{stateLabel(tr, t)}</span>
                <span className="task-duration">{fmtDuration(t.durationMs)}</span>
                <span className="spacer" />
                {t.state === 'running' && (
                  <button
                    className="task-kill"
                    title={tr('taskPanel.killTitle')}
                    onClick={() => void kill(t.id)}
                  >
                    <Square size={11} />
                    {tr('taskPanel.kill')}
                  </button>
                )}
              </div>
              <div className="task-command" title={t.command}>
                {t.command}
              </div>
              {t.output && (
                <div className="task-output-wrap">
                  <button
                    className="task-output-toggle"
                    onClick={() => setExpanded(e => ({ ...e, [t.id]: !e[t.id] }))}
                  >
                    {expanded[t.id] ? tr('taskPanel.collapseOutput') : tr('taskPanel.expandOutput')}
                  </button>
                  {expanded[t.id] && <pre className="task-output">{t.output}</pre>}
                </div>
              )}
            </li>
          ))}
        </ul>
      )}
    </div>
  );
}

/** TaskButton is the TopBar trigger: a live badge makes a leaked background
 *  task visible even with the panel closed (the human's safety net). */
export function TaskButton() {
  const tr = useT();
  const tasks = useTasksStore(s => s.tasks);
  const toggle = useTasksStore(s => s.toggle);
  const running = runningTaskCount(tasks);
  return (
    <button
      className={`task-btn ${running > 0 ? 'active' : ''}`}
      title={running > 0 ? tr('taskPanel.buttonTooltip', { n: running }) : tr('taskPanel.title')}
      onClick={toggle}
    >
      <Terminal size={14} />
      {running > 0 && <span className="task-badge">{running}</span>}
    </button>
  );
}
