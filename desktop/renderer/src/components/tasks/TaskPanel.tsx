import { useState } from 'react';
import { CheckCircle2, Loader2, RefreshCw, Square, Terminal, X, XCircle } from 'lucide-react';
import type { BackgroundTask } from '../../types';
import { useTasksStore, runningTaskCount } from '../../store/tasks';
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

function stateLabel(t: BackgroundTask): string {
  switch (t.state) {
    case 'running':
      return '运行中';
    case 'killed':
      return '已终止';
    default:
      return t.exitCode === 0 ? '已完成' : `退出码 ${t.exitCode}`;
  }
}

export default function TaskPanel() {
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
        <span className="task-panel-title">后台任务</span>
        {running > 0 && <span className="task-running-count">{running} 运行中</span>}
        <span className="spacer" />
        <button className="task-icon-btn" title="刷新" onClick={() => void refresh()}>
          <RefreshCw size={13} />
        </button>
        <button className="task-icon-btn" title="关闭" onClick={() => setOpen(false)}>
          <X size={13} />
        </button>
      </div>

      {error && <div className="task-error">{error}</div>}

      {tasks.length === 0 ? (
        <div className="task-empty">
          没有后台任务。命令超时或使用 run_in_background 后会出现在这里。
        </div>
      ) : (
        <ul className="task-list">
          {tasks.map(t => (
            <li key={t.id} className={`task-item ${t.state}`}>
              <div className="task-row">
                <span className="task-state" title={stateLabel(t)}>
                  {stateIcon(t)}
                </span>
                <span className="task-id">#{t.id}</span>
                <span className="task-state-label">{stateLabel(t)}</span>
                <span className="task-duration">{fmtDuration(t.durationMs)}</span>
                <span className="spacer" />
                {t.state === 'running' && (
                  <button
                    className="task-kill"
                    title="终止该任务的进程树"
                    onClick={() => void kill(t.id)}
                  >
                    <Square size={11} />
                    终止
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
                    {expanded[t.id] ? '收起输出' : '查看输出'}
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
  const tasks = useTasksStore(s => s.tasks);
  const toggle = useTasksStore(s => s.toggle);
  const running = runningTaskCount(tasks);
  return (
    <button
      className={`task-btn ${running > 0 ? 'active' : ''}`}
      title={running > 0 ? `${running} 个后台任务运行中` : '后台任务'}
      onClick={toggle}
    >
      <Terminal size={14} />
      {running > 0 && <span className="task-badge">{running}</span>}
    </button>
  );
}
