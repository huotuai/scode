import { CheckCircle2, Circle, ListTodo, Loader2, RefreshCw, X } from 'lucide-react';
import type { PlanItem } from '../../types';
import { usePlanStore, planProgress } from '../../store/plan';
import '../../styles/plan.css';

function itemIcon(i: PlanItem) {
  if (i.status === 'completed') return <CheckCircle2 size={13} />;
  if (i.status === 'in_progress') return <Loader2 size={13} className="spin" />;
  return <Circle size={13} />;
}

function itemLabel(i: PlanItem): string {
  switch (i.status) {
    case 'completed':
      return '已完成';
    case 'in_progress':
      return '进行中';
    default:
      return '待办';
  }
}

export default function PlanPanel() {
  const open = usePlanStore(s => s.open);
  const plan = usePlanStore(s => s.plan);
  const error = usePlanStore(s => s.error);
  const setOpen = usePlanStore(s => s.setOpen);
  const refresh = usePlanStore(s => s.refresh);

  if (!open) return null;
  const { done, total } = planProgress(plan);
  const pct = total > 0 ? Math.round((done / total) * 100) : 0;

  return (
    <div className="plan-panel">
      <div className="plan-panel-head">
        <ListTodo size={14} />
        <span className="plan-panel-title">计划进度</span>
        {total > 0 && (
          <span className={`plan-count ${done === total ? 'done' : ''}`}>
            {done}/{total} 已完成
          </span>
        )}
        <span className="spacer" />
        <button className="plan-icon-btn" title="刷新" onClick={() => void refresh()}>
          <RefreshCw size={13} />
        </button>
        <button className="plan-icon-btn" title="关闭" onClick={() => setOpen(false)}>
          <X size={13} />
        </button>
      </div>

      {total > 0 && (
        <div className="plan-meter" title={`${pct}%`}>
          <div className="plan-meter-fill" style={{ width: `${Math.max(2, pct)}%` }} />
        </div>
      )}

      {error && <div className="plan-error">{error}</div>}

      {total === 0 ? (
        <div className="plan-empty">
          当前会话还没有计划。多步任务中 Agent 会通过 update_plan 汇报进度,并随会话持久化。
        </div>
      ) : (
        <>
          {plan?.explanation && <div className="plan-explanation">{plan.explanation}</div>}
          <ul className="plan-list">
            {plan!.items.map((item, idx) => (
              <li key={idx} className={`plan-item ${item.status}`}>
                <span className="plan-state" title={itemLabel(item)}>
                  {itemIcon(item)}
                </span>
                <span className="plan-step">{item.step}</span>
                <span className="plan-state-label">{itemLabel(item)}</span>
              </li>
            ))}
          </ul>
        </>
      )}
    </div>
  );
}

/** PlanButton is the TopBar trigger: the done/total badge keeps progress
 *  visible even with the panel closed. */
export function PlanButton() {
  const plan = usePlanStore(s => s.plan);
  const toggle = usePlanStore(s => s.toggle);
  const { done, total } = planProgress(plan);
  const active = total > 0 && done < total;
  return (
    <button
      className={`plan-btn ${total > 0 ? 'has-plan' : ''} ${active ? 'active' : ''}`}
      title={total > 0 ? `计划进度:${done}/${total} 已完成` : '计划进度'}
      onClick={toggle}
    >
      <ListTodo size={14} />
      {total > 0 && (
        <span className={`plan-badge ${done === total ? 'done' : ''}`}>
          {done}/{total}
        </span>
      )}
    </button>
  );
}
