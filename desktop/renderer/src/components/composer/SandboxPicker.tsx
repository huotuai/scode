import { useEffect, useRef, useState } from 'react';
import { ChevronDown, Shield, ShieldCheck, ShieldAlert, type LucideIcon } from 'lucide-react';
import { useSessionStore, useActiveRuntime } from '../../store/session';
import { useT } from '../../i18n';

// Sandbox mode picker (dsh sandbox-policy vocabulary). Confined modes
// fence file mutations to the workspace; bash needs a runner backend.
// Labels/descriptions resolve per call so a language switch re-renders.
function modes(t: ReturnType<typeof useT>): { value: string; label: string; desc: string; icon: LucideIcon }[] {
  return [
    { value: 'read-only', label: t('sandbox.readOnlyLabel'), desc: t('sandbox.readOnlyDesc'), icon: ShieldAlert },
    { value: 'workspace-write', label: t('sandbox.workspaceLabel'), desc: t('sandbox.workspaceDesc'), icon: ShieldCheck },
    { value: 'danger-full-access', label: t('sandbox.fullLabel'), desc: t('sandbox.fullDesc'), icon: Shield },
  ];
}

export default function SandboxPicker() {
  const t = useT();
  const rt = useActiveRuntime();
  const sandbox = rt.sandbox;
  const setSandbox = useSessionStore(s => s.setSandbox);
  const running = rt.running;
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

  // Avoid changing sandbox modes mid-run.
  useEffect(() => {
    if (running) setOpen(false);
  }, [running]);

  const MODES = modes(t);
  const current = MODES.find(m => m.value === sandbox) ?? MODES[2];
  const confined = sandbox !== 'danger-full-access';
  const Icon = current.icon;

  return (
    <div className="model-picker" ref={ref}>
      <button
        className={`chip${confined ? ' active' : ''}`}
        onClick={() => setOpen(o => !o)}
        disabled={running}
        title={running ? t('sandbox.runningNoSwitch') : t('sandbox.tooltip', { label: current.label, desc: current.desc })}
      >
        <Icon size={14} />
        {t('sandbox.button', { label: current.label })}
        <ChevronDown size={13} />
      </button>
      {open && (
        <div className="model-menu">
          {MODES.map(m => {
            const MIcon = m.icon;
            return (
              <button
                key={m.value}
                className={`model-option sandbox-option${m.value === sandbox ? ' active' : ''}`}
                onClick={() => {
                  setOpen(false);
                  if (m.value !== sandbox) setSandbox(m.value);
                }}
              >
                <MIcon size={14} />
                <span className="sandbox-option-text">
                  <span>{m.label}</span>
                  <span className="sandbox-option-desc">{m.desc}</span>
                </span>
              </button>
            );
          })}
        </div>
      )}
    </div>
  );
}
