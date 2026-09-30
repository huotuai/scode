import { memo, useMemo, useState } from 'react';
import { ChevronDown, ChevronRight, Wrench, Loader2, CheckCircle2, XCircle } from 'lucide-react';
import type { ChatItem } from '../../types';
import type { DiffLine } from '../../lib/diff';
import { diffTexts } from '../../lib/diff';
import { langFromPath } from '../../lib/highlight';
import { DiffLines, HunksView } from '../diff/DiffView';

type ToolItem = Extract<ChatItem, { kind: 'tool' }>;

function abbrev(s: unknown, n: number): string {
  const str = String(s ?? '');
  return str.length > n ? str.slice(0, n) + '…' : str;
}

function parseArgs(args: unknown): Record<string, unknown> | null {
  if (args && typeof args === 'object') return args as Record<string, unknown>;
  if (typeof args === 'string') {
    try {
      const v = JSON.parse(args);
      if (v && typeof v === 'object') return v as Record<string, unknown>;
    } catch {
      /* raw shown below */
    }
  }
  return null;
}

interface EditPair {
  oldText: string;
  newText: string;
}

// parseEdits reads the edit tool's real argument shape: edits[] (the
// backend also repairs a JSON-string edits or a flat oldText/newText
// pair, so accept all three).
function parseEdits(args: Record<string, unknown>): EditPair[] | null {
  const one = (v: unknown): EditPair | null => {
    if (v && typeof v === 'object') {
      const o = v as Record<string, unknown>;
      if (typeof o.oldText === 'string') {
        return { oldText: o.oldText, newText: typeof o.newText === 'string' ? o.newText : '' };
      }
    }
    return null;
  };
  let list = args.edits;
  if (typeof list === 'string') {
    try {
      list = JSON.parse(list);
    } catch {
      list = null;
    }
  }
  if (Array.isArray(list)) {
    const out = list.map(one).filter((e): e is EditPair => e !== null);
    if (out.length > 0) return out;
  }
  const single = one(args);
  return single ? [single] : null;
}

const ToolCard = memo(function ToolCard({ item }: { item: ToolItem }) {
  // Diffs are the point of edit/write calls: start those expanded,
  // keep the rest collapsed by default.
  const [open, setOpen] = useState(item.name === 'edit' || item.name === 'write');
  const args = parseArgs(item.args);

  let body: React.ReactNode = null;
  const editPairs = args && item.name === 'edit' ? parseEdits(args) : null;
  if (args && editPairs) {
    body = <EditDiff path={String(args.path || '')} edits={editPairs} />;
  } else if (args && item.name === 'write' && args.path) {
    body = <WriteDiff path={String(args.path)} content={String(args.content ?? '')} />;
  } else if (args && (item.name === 'read' || item.name === 'grep' || item.name === 'find' || item.name === 'ls') && args.path) {
    body = <div className="tool-path">{String(args.path)}{args.pattern ? ` · ${String(args.pattern)}` : ''}</div>;
  } else if (args && item.name === 'bash' && args.command) {
    body = <pre className="tool-args">{abbrev(args.command, 300)}</pre>;
  } else {
    body = (
      <pre className="tool-args">
        {args ? abbrev(JSON.stringify(args), 300) : abbrev(item.args, 300)}
      </pre>
    );
  }

  const statusIcon =
    item.state === 'running' ? (
      <Loader2 size={13} className="spin" />
    ) : item.state === 'ok' ? (
      <CheckCircle2 size={13} />
    ) : (
      <XCircle size={13} />
    );

  return (
    <div className={`tool ${item.state}`}>
      <button className="tool-head" onClick={() => setOpen(o => !o)}>
        {open ? <ChevronDown size={13} /> : <ChevronRight size={13} />}
        <Wrench size={13} />
        <span className="tool-name">{item.name}</span>
        <span className={`tool-status ${item.state}`}>{statusIcon}</span>
      </button>
      {open && body}
      {/* The edit tool's result IS a unified diff of the change — the
          EditDiff body above already renders it highlighted, so echoing
          it as raw result text would duplicate it. Errors still show. */}
      {item.result !== undefined &&
        open &&
        !(editPairs && item.state !== 'error') && <pre className="tool-result">{item.result}</pre>}
    </div>
  );
});

// EditDiff renders an edit call's edits[] as line-level diffs: removed
// lines red, added lines green, syntax-highlighted by the file's
// extension, a few lines of context per hunk. Oversized inputs fall back
// to the raw two-block view (the LCS table would be both slow and
// unreadable at that size).
const EDIT_DIFF_MAX = 40_000;

function EditDiff({ path, edits }: { path: string; edits: EditPair[] }) {
  const lang = langFromPath(path);
  const tooBig = edits.reduce((n, e) => n + e.oldText.length + e.newText.length, 0) > EDIT_DIFF_MAX;
  const allHunks = useMemo(
    () => (tooBig ? null : edits.map(e => diffTexts(e.oldText, e.newText))),
    [tooBig, edits],
  );
  return (
    <div>
      <div className="tool-path">编辑 {path}</div>
      {allHunks ? (
        edits.map((e, i) => (
          <div key={i}>
            {edits.length > 1 && (
              <div className="tool-path">
                修改 {i + 1}/{edits.length}
              </div>
            )}
            {allHunks[i].length > 0 ? (
              <HunksView hunks={allHunks[i]} lang={lang} />
            ) : (
              <div className="tool-path">内容无变化</div>
            )}
          </div>
        ))
      ) : (
        edits.map((e, i) => (
          <div key={i}>
            <pre className="tool-args diff-old">{abbrev(e.oldText, 800)}</pre>
            <pre className="tool-args diff-new">{abbrev(e.newText, 800)}</pre>
          </div>
        ))
      )}
    </div>
  );
}

// WriteDiff shows a write call's content as all-added lines, capped so a
// large file does not flood the chat; the tail announces the remainder.
const WRITE_DIFF_MAX_LINES = 80;

function WriteDiff({ path, content }: { path: string; content: string }) {
  const lang = langFromPath(path);
  const lines = useMemo(() => {
    const all = content === '' ? [] : content.split('\n');
    const shown = all.slice(0, WRITE_DIFF_MAX_LINES);
    const out: DiffLine[] = shown.map((text, i) => ({
      type: 'add',
      text,
      oldNo: null,
      newNo: i + 1,
    }));
    return { out, total: all.length };
  }, [content]);
  return (
    <div>
      <div className="tool-path">写入 {path}</div>
      <DiffLines lines={lines.out} lang={lang} />
      {lines.total > WRITE_DIFF_MAX_LINES && (
        <div className="tool-path">… 共 {lines.total} 行,仅显示前 {WRITE_DIFF_MAX_LINES} 行</div>
      )}
    </div>
  );
}

export default ToolCard;
