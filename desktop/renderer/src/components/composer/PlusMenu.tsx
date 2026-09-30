import { useEffect, useRef, useState } from 'react';
import { Plus, ImagePlus, ListTodo, Shrink, Loader2 } from 'lucide-react';
import { useSessionStore, useActiveRuntime } from '../../store/session';
import { useT } from '../../i18n';

// "+" menu at the left of the model picker: secondary composer actions
// (image attachments, plan mode, context compaction) grouped into one
// upward popup so the bar stays compact.
export default function PlusMenu({ onFiles }: { onFiles: (files: File[]) => void }) {
  const t = useT();
  const [open, setOpen] = useState(false);
  const ref = useRef<HTMLDivElement>(null);
  const fileRef = useRef<HTMLInputElement>(null);
  const rt = useActiveRuntime();
  const running = rt.running;
  const compacting = rt.compacting;
  const mode = rt.mode;
  const sessionId = useSessionStore(s => s.sessionId);
  const toggleMode = useSessionStore(s => s.toggleMode);
  const compact = useSessionStore(s => s.compact);

  useEffect(() => {
    if (!open) return;
    const onDoc = (e: MouseEvent) => {
      if (ref.current && !ref.current.contains(e.target as Node)) setOpen(false);
    };
    document.addEventListener('mousedown', onDoc);
    return () => document.removeEventListener('mousedown', onDoc);
  }, [open]);

  return (
    <div className="plus-menu-wrap" ref={ref}>
      <input
        ref={fileRef}
        type="file"
        accept="image/*"
        multiple
        hidden
        onChange={e => {
          onFiles(Array.from(e.target.files ?? []));
          e.target.value = ''; // re-picking the same file must fire again
        }}
      />
      <button
        className={`icon-btn plus-btn${open ? ' active' : ''}`}
        onClick={() => setOpen(o => !o)}
        title={t('plusMenu.title')}
      >
        <Plus size={16} />
      </button>
      {open && (
        <div className="model-menu plus-menu">
          <button
            className="model-option"
            onClick={() => {
              setOpen(false);
              fileRef.current?.click();
            }}
          >
            <ImagePlus size={13} />
            {t('plusMenu.addImage')}
            <span className="plus-hint">{t('plusMenu.pasteHint')}</span>
          </button>
          <button
            className={`model-option${mode === 'plan' ? ' active' : ''}`}
            disabled={running || compacting}
            onClick={() => {
              setOpen(false);
              toggleMode();
            }}
          >
            <ListTodo size={13} />
            {t('plusMenu.planMode')}
            {mode === 'plan' && <span className="plus-hint">{t('plusMenu.planOn')}</span>}
          </button>
          <button
            className="model-option"
            disabled={!sessionId || running}
            onClick={() => {
              setOpen(false);
              compact();
            }}
          >
            {compacting ? <Loader2 size={13} className="spin" /> : <Shrink size={13} />}
            {compacting ? t('plusMenu.compacting') : t('plusMenu.compact')}
          </button>
        </div>
      )}
    </div>
  );
}
