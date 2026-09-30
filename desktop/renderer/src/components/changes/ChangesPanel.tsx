import { useMemo, useState } from 'react';
import { ChevronDown, ChevronRight, FileDiff, Loader2, RefreshCw, X } from 'lucide-react';
import type { GitChangedFile } from '../../types';
import type { FileDiff as ParsedFileDiff } from '../../lib/diff';
import { parseUnifiedDiff } from '../../lib/diff';
import DiffView from '../diff/DiffView';
import { useChangesStore } from '../../store/changes';
import '../../styles/diff.css';

const STATUS_LABEL: Record<string, string> = {
  M: '修改',
  A: '新增',
  D: '删除',
  R: '重命名',
  '??': '未跟踪',
};

/** matchDiff pairs a status entry with its parsed diff. Deleted files are
 *  keyed by their old path; everything else by the new path. */
function matchDiff(file: GitChangedFile, diffs: ParsedFileDiff[]): ParsedFileDiff | null {
  for (const d of diffs) {
    if (d.newPath === file.path || d.oldPath === file.path) return d;
  }
  return null;
}

function ChangesItem({ file, diffs }: { file: GitChangedFile; diffs: ParsedFileDiff[] }) {
  const [open, setOpen] = useState(false);
  const diff = matchDiff(file, diffs);
  return (
    <li className="changes-item">
      <button className={`changes-row ${open ? 'open' : ''}`} onClick={() => setOpen(o => !o)}>
        {open ? <ChevronDown size={13} /> : <ChevronRight size={13} />}
        <span
          className={`changes-status ${file.untracked ? 'untracked' : file.status}`}
          title={STATUS_LABEL[file.status] ?? file.status}
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
              {file.untracked ? '未跟踪的新文件,不在 git diff 中' : '无文本差异(可能为二进制文件)'}
            </div>
          )}
        </div>
      )}
    </li>
  );
}

export default function ChangesPanel() {
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
        <span className="changes-panel-title">代码更改</span>
        {files.length > 0 && <span className="changes-count">{files.length} 个文件</span>}
        {(additions > 0 || deletions > 0) && (
          <span className="changes-stats">
            {additions > 0 && <span className="diff-stat-add">+{additions}</span>}
            {deletions > 0 && <span className="diff-stat-del">−{deletions}</span>}
          </span>
        )}
        <span className="spacer" />
        <button className="changes-icon-btn" title="刷新" onClick={() => void refresh()}>
          {loading ? <Loader2 size={13} className="spin" /> : <RefreshCw size={13} />}
        </button>
        <button className="changes-icon-btn" title="关闭" onClick={() => setOpen(false)}>
          <X size={13} />
        </button>
      </div>

      {error && <div className="changes-error">{error}</div>}
      {result?.truncated && (
        <div className="changes-truncated">差异内容过大,仅显示前一部分</div>
      )}

      {notRepo ? (
        <div className="changes-empty">当前工作区不是 git 仓库,无法显示更改。</div>
      ) : files.length === 0 ? (
        <div className="changes-empty">工作区没有未提交的更改。</div>
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
          ? `代码更改:${files} 个文件(+${additions} −${deletions})`
          : '代码更改(git diff)'
      }
      onClick={toggle}
    >
      <FileDiff size={14} />
      <span className="changes-label">更改</span>
      {(additions > 0 || deletions > 0) && (
        <span className="changes-stats">
          {additions > 0 && <span className="diff-stat-add">+{additions}</span>}
          {deletions > 0 && <span className="diff-stat-del">−{deletions}</span>}
        </span>
      )}
    </button>
  );
}
