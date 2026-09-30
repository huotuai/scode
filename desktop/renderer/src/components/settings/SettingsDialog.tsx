import { useCallback, useEffect, useRef } from 'react';
import { Cpu, Palette, X } from 'lucide-react';
import { useUiStore, type SettingsSection } from '../../store/ui';
import ModelSettings from './ModelSettings';
import AppearanceSettings from './AppearanceSettings';
import '../../styles/settings.css';

const SECTIONS: { key: SettingsSection; label: string; icon: typeof Cpu }[] = [
  { key: 'appearance', label: '外观', icon: Palette },
  { key: 'models', label: '模型', icon: Cpu },
];

// Settings dialog: left nav / right content split. Model profiles track
// unsaved edits (dirtyRef); appearance options apply immediately. The
// active page lives in the ui store so callers can open straight to a
// section (e.g. the composer's 管理模型 lands on models).
export default function SettingsDialog() {
  const open = useUiStore(s => s.settingsOpen);
  const setOpen = useUiStore(s => s.setSettingsOpen);
  const section = useUiStore(s => s.settingsSection);
  const setSection = useUiStore(s => s.setSettingsSection);
  // Mirrors the embedded panel's unsaved-edits flag so mask/X/Esc closing
  // can confirm instead of silently dropping them.
  const dirtyRef = useRef(false);

  const onDirtyChange = useCallback((d: boolean) => {
    dirtyRef.current = d;
  }, []);

  const requestClose = useCallback(() => {
    if (dirtyRef.current && !confirm('有未保存的修改,确定关闭?')) return;
    setOpen(false);
  }, [setOpen]);

  useEffect(() => {
    if (!open) return;
    const onKey = (e: KeyboardEvent) => {
      if (e.key === 'Escape') requestClose();
    };
    document.addEventListener('keydown', onKey);
    return () => document.removeEventListener('keydown', onKey);
  }, [open, requestClose]);

  if (!open) return null;

  const active = SECTIONS.find(s => s.key === section) ?? SECTIONS[0];

  return (
    <div className="modal-mask" onMouseDown={e => e.target === e.currentTarget && requestClose()}>
      <div className="modal-panel settings-panel">
        <div className="settings-body">
          <nav className="settings-nav">
            <div className="settings-nav-title">设置</div>
            {SECTIONS.map(s => {
              const Icon = s.icon;
              return (
                <button
                  key={s.key}
                  className={`settings-nav-item${s.key === section ? ' active' : ''}`}
                  onClick={() => setSection(s.key)}
                >
                  <Icon size={15} />
                  <span>{s.label}</span>
                </button>
              );
            })}
          </nav>
          <div className="settings-content">
            <div className="settings-content-head">
              <span>{active.label}</span>
              <span className="spacer" />
              <button className="icon-btn" onClick={requestClose} title="关闭">
                <X size={16} />
              </button>
            </div>
            <div className="settings-content-body">
              {section === 'appearance' && <AppearanceSettings />}
              {section === 'models' && <ModelSettings onDirtyChange={onDirtyChange} />}
            </div>
          </div>
        </div>
      </div>
    </div>
  );
}
