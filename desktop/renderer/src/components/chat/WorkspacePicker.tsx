import { useEffect, useRef, useState } from 'react';
import { Check, ChevronDown, Folder, FolderOpen, FolderPlus, X } from 'lucide-react';
import { useSessionStore } from '../../store/session';
import { useWorkspacesStore } from '../../store/workspaces';
import { folderName, sameFolder } from '../../lib/paths';
import { useT } from '../../i18n';

// Workspace picker for the new-session page: the current folder plus a
// dropdown of recently used directories (persisted across restarts) and a
// "pick a new folder" escape hatch. Picking an entry switches the active
// workspace; the × on a stale entry just forgets it.
export default function WorkspacePicker() {
  const t = useT();
  const workspace = useSessionStore(s => s.workspace);
  const selectWorkspace = useSessionStore(s => s.selectWorkspace);
  const chooseWorkspace = useSessionStore(s => s.chooseWorkspace);
  const recent = useWorkspacesStore(s => s.recent);
  const forget = useWorkspacesStore(s => s.forget);

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

  // The current folder leads the list even if the remembered entries
  // predate it (e.g. workspace came from the environment, not the UI).
  const dirs = workspace
    ? [workspace, ...recent.filter(d => !sameFolder(d, workspace))]
    : recent;

  const pick = (dir: string) => {
    setOpen(false);
    selectWorkspace(dir);
  };

  const pickNew = () => {
    setOpen(false);
    chooseWorkspace(); // native folder dialog
  };

  const remove = (e: React.MouseEvent, dir: string) => {
    e.stopPropagation();
    forget(dir);
  };

  return (
    <div className="ws-picker" ref={ref}>
      <button
        className="ws-current"
        onClick={() => setOpen(o => !o)}
        title={workspace ? t('workspace.titleWith', { ws: workspace }) : t('workspace.select')}
      >
        <FolderOpen size={14} />
        <span className="ws-label">{t('workspace.label')}</span>
        <span className="ws-path">{workspace || t('workspace.select')}</span>
        <ChevronDown size={13} className={`ws-caret${open ? ' open' : ''}`} />
      </button>
      {open && (
        <div className="ws-menu">
          <div className="ws-menu-title">{t('workspace.recentTitle')}</div>
          {dirs.length === 0 && <div className="ws-menu-empty dim">{t('workspace.empty')}</div>}
          {dirs.map(d => {
            const active = sameFolder(d, workspace);
            return (
              <div
                key={d}
                className={`ws-option${active ? ' active' : ''}`}
                onClick={() => !active && pick(d)}
                title={d}
                role="button"
                tabIndex={0}
                onKeyDown={e => {
                  if (e.target !== e.currentTarget) return;
                  if (e.key !== 'Enter' && e.key !== ' ') return;
                  e.preventDefault();
                  if (!active) pick(d);
                }}
              >
                <Folder size={14} />
                <span className="ws-option-name">{folderName(d)}</span>
                <span className="ws-option-path">{d}</span>
                {active && <Check size={13} className="ws-check" />}
                {!active && (
                  <button
                    className="icon-btn ws-remove"
                    onClick={e => remove(e, d)}
                    title={t('workspace.remove')}
                  >
                    <X size={12} />
                  </button>
                )}
              </div>
            );
          })}
          <button className="ws-option ws-new" onClick={pickNew}>
            <FolderPlus size={14} />
            {t('workspace.browse')}
          </button>
        </div>
      )}
    </div>
  );
}
