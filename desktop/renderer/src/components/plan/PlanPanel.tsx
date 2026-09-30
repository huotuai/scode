import { CheckCircle2, Circle, ListTodo, Loader2, RefreshCw, X } from 'lucide-react';
import type { PlanItem } from '../../types';
import { usePlanStore, planProgress } from '../../store/plan';
import { useT } from '../../i18n';
import '../../styles/plan.css';

function itemIcon(i: PlanItem) {
  if (i.status === 'completed') return <CheckCircle2 size={13} />;
  if (i.status === 'in_progress') return <Loader2 size={13} className="spin" />;
  return <Circle size={13} />;
}

function itemLabel(t: ReturnType<typeof useT>, i: PlanItem): string {
  switch (i.status) {
    case 'completed':
      return t('plan.done');
    case 'in_progress':
      return t('plan.inProgress');
    default:
      return t('plan.pending');
  }
}

export default function PlanPanel() {
  const t = useT();
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
        <span className="plan-panel-title">{t('plan.title')}</span>
        {total > 0 && (
          <span className={`plan-count ${done === total ? 'done' : ''}`}>
            {t('plan.doneCount', { done, total })}
          </span>
        )}
        <span className="spacer" />
        <button className="plan-icon-btn" title={t('common.refresh')} onClick={() => void refresh()}>
          <RefreshCw size={13} />
        </button>
        <button className="plan-icon-btn" title={t('common.close')} onClick={() => setOpen(false)}>
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
          {t('plan.empty')}
        </div>
      ) : (
        <>
          {plan?.explanation && <div className="plan-explanation">{plan.explanation}</div>}
          <ul className="plan-list">
            {plan!.items.map((item, idx) => (
              <li key={idx} className={`plan-item ${item.status}`}>
                <span className="plan-state" title={itemLabel(t, item)}>
                  {itemIcon(item)}
                </span>
                <span className="plan-step">{item.step}</span>
                <span className="plan-state-label">{itemLabel(t, item)}</span>
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
  const t = useT();
  const plan = usePlanStore(s => s.plan);
  const toggle = usePlanStore(s => s.toggle);
  const { done, total } = planProgress(plan);
  const active = total > 0 && done < total;
  return (
    <button
      className={`plan-btn ${total > 0 ? 'has-plan' : ''} ${active ? 'active' : ''}`}
      title={total > 0 ? t('plan.buttonTooltip', { done, total }) : t('plan.title')}
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
