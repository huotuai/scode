import { useEffect, useRef, useState } from 'react';
import { ChevronDown, Shield, ShieldCheck, ShieldAlert, type LucideIcon } from 'lucide-react';
import { useSessionStore, useActiveRuntime } from '../../store/session';

// Sandbox mode picker (dsh sandbox-policy vocabulary). Confined modes
// fence file mutations to the workspace; bash needs a runner backend.
const MODES: { value: string; label: string; desc: string; icon: LucideIcon }[] = [
  { value: 'read-only', label: '只读', desc: '禁止一切文件修改', icon: ShieldAlert },
  { value: 'workspace-write', label: '工作区可写', desc: '仅工作区与临时目录可写', icon: ShieldCheck },
  { value: 'danger-full-access', label: '不限制', desc: '文件修改不受沙箱限制', icon: Shield },
];

export default function SandboxPicker() {
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

  const current = MODES.find(m => m.value === sandbox) ?? MODES[2];
  const confined = sandbox !== 'danger-full-access';
  const Icon = current.icon;

  return (
    <div className="model-picker" ref={ref}>
      <button
        className={`chip${confined ? ' active' : ''}`}
        onClick={() => setOpen(o => !o)}
        disabled={running}
        title={running ? '运行中无法切换沙箱' : `沙箱:${current.label} — ${current.desc}`}
      >
        <Icon size={14} />
        沙箱·{current.label}
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
