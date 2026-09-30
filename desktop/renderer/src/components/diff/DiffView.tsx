import { memo, useMemo, Fragment } from 'react';
import type { DiffHunk, DiffLine, FileDiff } from '../../lib/diff';
import { displayPath } from '../../lib/diff';
import { highlightLines, langFromPath, type Token } from '../../lib/highlight';
import '../../styles/diff.css';

// DiffLineRow: one line of a diff — line number gutter, colored left bar
// (green add / red del), and the code (syntax-highlighted when tokens are
// provided). The gutter shows the new-side number, falling back to the
// old-side number on deleted lines.
const DiffLineRow = memo(function DiffLineRow({ line, tokens }: { line: DiffLine; tokens?: Token[] }) {
  return (
    <div className={`diff-line ${line.type}`}>
      <span className="diff-no">{line.newNo ?? line.oldNo ?? ''}</span>
      <span className="diff-code">
        {tokens && tokens.length > 0
          ? tokens.map((tok, i) =>
              tok.t ? (
                <span key={i} className={`tok-${tok.t}`}>
                  {tok.s}
                </span>
              ) : (
                tok.s
              ),
            )
          : line.text || ' '}
      </span>
    </div>
  );
});

// useLineTokens highlights the lines' text once per (lines, lang) pair.
// Tokenization runs over the joined text so multiline comments/strings
// inside the shown window keep their state.
function useLineTokens(lines: DiffLine[], lang?: string): Token[][] | null {
  return useMemo(() => {
    if (!lang) return null;
    return highlightLines(
      lines.map(l => l.text),
      lang,
    );
  }, [lines, lang]);
}

/** DiffLines renders a bare sequence of lines (no hunk headers) — used by
 *  the tool cards, which build their own context. */
export function DiffLines({ lines, lang }: { lines: DiffLine[]; lang?: string }) {
  const tokens = useLineTokens(lines, lang);
  return (
    <div className="diff">
      <div className="diff-inner">
        {lines.map((l, i) => (
          <DiffLineRow key={i} line={l} tokens={tokens?.[i]} />
        ))}
      </div>
    </div>
  );
}

/** HunksView renders hunks with their @@ headers. Lines are flat children
 *  of the shrink-to-fit .diff-inner box (no per-hunk wrapper) so every row
 *  shares the widest line's width. Highlighting spans the whole hunk list
 *  so state survives across hunk boundaries. */
export function HunksView({ hunks, lang }: { hunks: DiffHunk[]; lang?: string }) {
  const allLines = useMemo(() => hunks.flatMap(h => h.lines), [hunks]);
  const tokens = useLineTokens(allLines, lang);
  let offset = 0;
  return (
    <div className="diff">
      <div className="diff-inner">
        {hunks.map((h, i) => {
          const start = offset;
          offset += h.lines.length;
          return (
            <Fragment key={i}>
              <div className="diff-hunk-head">{h.header}</div>
              {h.lines.map((l, j) => (
                <DiffLineRow key={j} line={l} tokens={tokens?.[start + j]} />
              ))}
            </Fragment>
          );
        })}
      </div>
    </div>
  );
}

/** DiffView renders one parsed file of a unified diff: header with the
 *  path and +/- counts, then its hunks. The language comes from the file
 *  extension, so it can be used without extra wiring. */
export default function DiffView({ file }: { file: FileDiff }) {
  const path = displayPath(file);
  const lang = langFromPath(path);
  return (
    <div className="diff-file">
      <div className="diff-file-head" title={path}>
        <span className="diff-file-path">{path}</span>
        {(file.additions > 0 || file.deletions > 0) && (
          <span className="diff-file-stats">
            {file.additions > 0 && <span className="diff-stat-add">+{file.additions}</span>}
            {file.deletions > 0 && <span className="diff-stat-del">−{file.deletions}</span>}
          </span>
        )}
        {file.isNew && <span className="diff-file-tag new">新增</span>}
        {file.isDeleted && <span className="diff-file-tag del">删除</span>}
        {file.isRename && <span className="diff-file-tag">重命名</span>}
      </div>
      {file.hunks.length > 0 ? (
        <HunksView hunks={file.hunks} lang={lang} />
      ) : (
        <div className="diff-binary">二进制文件或内容为空,无文本差异</div>
      )}
    </div>
  );
}
