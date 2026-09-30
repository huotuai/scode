import { useMemo, useState } from 'react';
import { ChevronDown, ChevronRight, FileDiff, Loader2, RefreshCw, X } from 'lucide-react';
import type { GitChangedFile } from '../../types';
import type { FileDiff as ParsedFileDiff } from '../../lib/diff';
import { parseUnifiedDiff } from '../../lib/diff';
import DiffView from '../diff/DiffView';
import { useChangesStore } from '../../store/changes';
import { useT } from '../../i18n';
import '../../styles/diff.css';

// statusLabel maps a git status letter to its display label (localized at
// render time — a map built once would freeze the language at import).
function statusLabel(t: ReturnType<typeof useT>, code: string): string {
  switch (code) {
    case 'M':
      return t('changes.statusM');
    case 'A':
      return t('changes.statusA');
    case 'D':
      return t('changes.statusD');
    case 'R':
      return t('changes.statusR');
  }
  return t('changes.statusUntracked');
}

/** matchDiff pairs a status entry with its parsed diff. Deleted files are
 *  keyed by their old path; everything else by the new path. */
function matchDiff(file: GitChangedFile, diffs: ParsedFileDiff[]): ParsedFileDiff | null {
  for (const d of diffs) {
    if (d.newPath === file.path || d.oldPath === file.path) return d;
  }
  return null;
}

function ChangesItem({ file, diffs }: { file: GitChangedFile; diffs: ParsedFileDiff[] }) {
  const t = useT();
  const [open, setOpen] = useState(false);
  const diff = matchDiff(file, diffs);
  return (
    <li className="changes-item">
      <button className={`changes-row ${open ? 'open' : ''}`} onClick={() => setOpen(o => !o)}>
        {open ? <ChevronDown size={13} /> : <ChevronRight size={13} />}
        <span
          className={`changes-status ${file.untracked ? 'untracked' : file.status}`}
          title={statusLabel(t, file.status)}
        >
          {file.status}
        </span>
        <span className="changes-path" title={file.path}>
          {file.path}
        </span>
        {diff && (diff.additions > 0 || diff.deletions > 0) && (
          <span className="changes-stats">
            {diff.additions > 0 && <span className="diff-stat-add">+{diff.additions}</span>}
            {diff.deletions > 0 && <span className="diff-stat-del">−{diff.deletions}</span>}
          </span>
        )}
      </button>
      {open && (
        <div className="changes-item-body">
          {diff ? (
            <DiffView file={diff} />
          ) : (
            <div className="changes-untracked-note">
              {file.untracked ? t('changes.untrackedNote') : t('changes.noDiff')}
            </div>
          )}
        </div>
      )}
    </li>
  );
}

export default function ChangesPanel() {
  const t = useT();
  const open = useChangesStore(s => s.open);
  const loading = useChangesStore(s => s.loading);
  const result = useChangesStore(s => s.result);
  const error = useChangesStore(s => s.error);
  const notRepo = useChangesStore(s => s.notRepo);
  const setOpen = useChangesStore(s => s.setOpen);
  const refresh = useChangesStore(s => s.refresh);

  // Parse once per result: the combined diff text feeds every file row.
  const diffs = useMemo(() => (result ? parseUnifiedDiff(result.diff) : []), [result]);

  if (!open) return null;
  const files = result?.files ?? [];
  const additions = diffs.reduce((n, d) => n + d.additions, 0);
  const deletions = diffs.reduce((n, d) => n + d.deletions, 0);

  return (
    <div className="changes-panel">
      <div className="changes-panel-head">
        <FileDiff size={14} />
        <span className="changes-panel-title">{t('changes.title')}</span>
        {files.length > 0 && <span className="changes-count">{t('changes.fileCount', { n: files.length })}</span>}
        {(additions > 0 || deletions > 0) && (
          <span className="changes-stats">
            {additions > 0 && <span className="diff-stat-add">+{additions}</span>}
            {deletions > 0 && <span className="diff-stat-del">−{deletions}</span>}
          </span>
        )}
        <span className="spacer" />
        <button className="changes-icon-btn" title={t('common.refresh')} onClick={() => void refresh()}>
          {loading ? <Loader2 size={13} className="spin" /> : <RefreshCw size={13} />}
        </button>
        <button className="changes-icon-btn" title={t('common.close')} onClick={() => setOpen(false)}>
          <X size={13} />
        </button>
      </div>

      {error && <div className="changes-error">{error}</div>}
      {result?.truncated && (
        <div className="changes-truncated">{t('changes.truncated')}</div>
      )}

      {notRepo ? (
        <div className="changes-empty">{t('changes.notRepo')}</div>
      ) : files.length === 0 ? (
        <div className="changes-empty">{t('changes.clean')}</div>
      ) : (
        <ul className="changes-list">
          {files.map(f => (
            <ChangesItem key={f.path} file={f} diffs={diffs} />
          ))}
        </ul>
      )}
    </div>
  );
}

/** ChangesButton is the TopBar trigger: a summary row — 更改 +N −N —
 *  styled like the reference (green additions, red deletions). Clicking
 *  docks the changes pane on the right side of the window. */
export function ChangesButton() {
  const t = useT();
  const open = useChangesStore(s => s.open);
  const result = useChangesStore(s => s.result);
  const toggle = useChangesStore(s => s.toggle);

  // Totals for the summary text; parsed once per result (the panel parses
  // the same diff again for its per-file rows — both are memoized).
  const { additions, deletions, files } = useMemo(() => {
    const diffs = result ? parseUnifiedDiff(result.diff) : [];
    return {
      additions: diffs.reduce((n, d) => n + d.additions, 0),
      deletions: diffs.reduce((n, d) => n + d.deletions, 0),
      files: result?.files.length ?? 0,
    };
  }, [result]);

  return (
    <button
      className={`changes-btn ${open ? 'active' : ''}`}
      title={
        files > 0
          ? t('changes.buttonStats', { files, add: additions, del: deletions })
          : t('changes.button')
      }
      onClick={toggle}
    >
      <FileDiff size={14} />
      <span className="changes-label">{t('changes.label')}</span>
      {(additions > 0 || deletions > 0) && (
        <span className="changes-stats">
          {additions > 0 && <span className="diff-stat-add">+{additions}</span>}
          {deletions > 0 && <span className="diff-stat-del">−{deletions}</span>}
        </span>
      )}
    </button>
  );
}
