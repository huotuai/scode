import { useEffect, useRef, useState } from 'react';
import { ChevronDown, Cpu, Settings2 } from 'lucide-react';
import { useActiveRuntime } from '../../store/session';
import { useModelsStore } from '../../store/models';
import { useT, thinkingLabel } from '../../i18n';
import { useUiStore } from '../../store/ui';

type ModelChoice = { label: string; provider: string; model: string; reasoning?: boolean };

// Model picker: a dropdown of configured provider/model pairs, a
// reasoning-effort section, and a 管理模型 entry that opens the settings
// dialog's models page. Choices carry provider/model directly — the old
// label-splitting broke on ids containing " / ".
export default function ModelPicker() {
  const t = useT();
  const rt = useActiveRuntime();
  const provider = rt.provider;
  const model = rt.model;
  const thinking = rt.thinking;
  const running = rt.running;
  const models = useModelsStore(s => s.models);
  const defaultChoice = useModelsStore(s => s.defaultChoice);
  const switchModel = useModelsStore(s => s.switchModel);
  const switchThinking = useModelsStore(s => s.switchThinking);
  const openSettings = useUiStore(s => s.openSettings);

  const [open, setOpen] = useState(false);
  const ref = useRef<HTMLDivElement>(null);

  useEffect(() => {
    if (!open) return;
    const onDoc = (e: MouseEvent) => {
      if (ref.current && !ref.current.contains(e.target as Node)) setOpen(false);
    };
    document.addEventListener('mousedown', onDoc);
    return () => document.removeEventListener('mousedown', onDoc);
  }, [open]);

  // The server rejects model/effort switches mid-run; close the menu too.
  useEffect(() => {
    if (running) setOpen(false);
  }, [running]);

  // A draft session shows the configured default model until the
  // session is created (on first use) and reports its resolved model.
  const effProvider = provider || defaultChoice.provider;
  const effModel = model || defaultChoice.model;
  const label = effProvider && effModel ? `${effProvider} / ${effModel}` : effModel || t('modelPicker.selectModel');

  // Reasoning capability of the model in view: a listed model without
  // the reasoning flag hides the effort controls; unknown models stay
  // selectable (compat endpoints default to capable).
  const entry = models.find(m => m.provider === effProvider && m.model === effModel);
  const reasoningCapable = entry ? entry.reasoning !== false : true;
  const chipLabel =
    reasoningCapable && thinking ? `${label} · ${thinkingLabel(thinking)}` : label;

  const isCurrent = (m: ModelChoice) => m.provider === provider && m.model === model;

  const pick = (m: ModelChoice) => {
    setOpen(false);
    if (isCurrent(m)) return;
    switchModel(m.provider, m.model).catch(() => {});
  };

  const pickLevel = (level: string) => {
    setOpen(false);
    if (level === thinking) return;
    switchThinking(level).catch(() => {});
  };

  return (
    <div className="model-picker" ref={ref}>
      <button
        className="chip model-chip"
        onClick={() => setOpen(o => !o)}
        disabled={running}
        title={running ? t('modelPicker.runningNoSwitch') : t('modelPicker.switchTitle')}
      >
        <Cpu size={14} />
        <span className="model-label">{chipLabel}</span>
        <ChevronDown size={13} />
      </button>
      {open && (
        <div className="model-menu">
          {models.length === 0 && <div className="model-empty dim">{t('modelPicker.empty')}</div>}
          {models.map(m => (
            <button
              key={`${m.provider}\u0000${m.model}`}
              className={`model-option${isCurrent(m) ? ' active' : ''}`}
              onClick={() => pick(m)}
            >
              {m.label}
            </button>
          ))}
          <div
            className="model-levels"
            title={reasoningCapable ? undefined : t('modelPicker.notReasoning')}
          >
            <div className="model-levels-title">{t('modelPicker.levelsTitle')}</div>
            <div className="model-levels-row">
              {(['', 'off', 'low', 'medium', 'high'] as const).map(lv => (
                <button
                  key={lv || 'default'}
                  className={`model-level${thinking === lv ? ' active' : ''}`}
                  disabled={running || !reasoningCapable}
                  onClick={() => pickLevel(lv)}
                >
                  {thinkingLabel(lv)}
                </button>
              ))}
            </div>
          </div>
          <button
            className="model-option custom"
            onClick={() => {
              setOpen(false);
              openSettings('models');
            }}
          >
            <Settings2 size={13} />
            {t('modelPicker.manage')}
          </button>
        </div>
      )}
    </div>
  );
}
