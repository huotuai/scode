// Line-level diff utilities shared by the changes panel (unified diffs
// produced by git) and the chat tool cards (edit/write diffs computed
// in the renderer from tool arguments).

export type DiffLineType = 'add' | 'del' | 'ctx';

export interface DiffLine {
  type: DiffLineType;
  text: string;
  oldNo: number | null; // line number on the old side (null for additions)
  newNo: number | null; // line number on the new side (null for deletions)
}

export interface DiffHunk {
  header: string; // the raw "@@ -a,b +c,d @@" line
  oldStart: number;
  newStart: number;
  lines: DiffLine[];
}

export interface FileDiff {
  oldPath: string;
  newPath: string;
  isNew: boolean;
  isDeleted: boolean;
  isRename: boolean;
  hunks: DiffHunk[];
  additions: number;
  deletions: number;
}

/** displayPath is the label for a diffed file: the new name, else old. */
export function displayPath(f: FileDiff): string {
  return f.newPath || f.oldPath;
}

function stripPrefix(p: string): string {
  if (p === '/dev/null') return '';
  // quoted paths ("a/x y") lose the quotes; escapes are rare enough to show raw
  if (p.startsWith('"') && p.endsWith('"')) p = p.slice(1, -1);
  if (p.startsWith('a/') || p.startsWith('b/')) return p.slice(2);
  return p;
}

// parseUnifiedDiff turns `git diff` output into per-file structured hunks.
// Lines outside any file/hunk (index, mode markers) are skipped; binary
// files end up as a FileDiff with no hunks.
export function parseUnifiedDiff(text: string): FileDiff[] {
  const files: FileDiff[] = [];
  let cur: FileDiff | null = null;
  let hunk: DiffHunk | null = null;
  let oldNo = 0;
  let newNo = 0;
  for (const raw of text.split('\n')) {
    if (raw.startsWith('diff --git ')) {
      cur = {
        oldPath: '',
        newPath: '',
        isNew: false,
        isDeleted: false,
        isRename: false,
        hunks: [],
        additions: 0,
        deletions: 0,
      };
      files.push(cur);
      hunk = null;
      const m = /^diff --git a\/(.+) b\/(.+)$/.exec(raw);
      if (m) {
        cur.oldPath = m[1];
        cur.newPath = m[2];
      }
      continue;
    }
    if (!cur) continue;
    if (raw.startsWith('new file mode')) {
      cur.isNew = true;
      continue;
    }
    if (raw.startsWith('deleted file mode')) {
      cur.isDeleted = true;
      continue;
    }
    if (raw.startsWith('rename from ')) {
      cur.isRename = true;
      cur.oldPath = raw.slice('rename from '.length);
      continue;
    }
    if (raw.startsWith('rename to ')) {
      cur.newPath = raw.slice('rename to '.length);
      continue;
    }
    if (raw.startsWith('--- ')) {
      cur.oldPath = stripPrefix(raw.slice(4));
      continue;
    }
    if (raw.startsWith('+++ ')) {
      cur.newPath = stripPrefix(raw.slice(4));
      continue;
    }
    const hm = /^@@ -(\d+)(?:,\d+)? \+(\d+)(?:,\d+)? @@/.exec(raw);
    if (hm) {
      oldNo = parseInt(hm[1], 10);
      newNo = parseInt(hm[2], 10);
      hunk = { header: raw, oldStart: oldNo, newStart: newNo, lines: [] };
      cur.hunks.push(hunk);
      continue;
    }
    if (!hunk) continue;
    if (raw.startsWith('\\')) continue; // "\ No newline at end of file"
    const t = raw[0];
    const body = raw.slice(1);
    if (t === '+') {
      hunk.lines.push({ type: 'add', text: body, oldNo: null, newNo: newNo++ });
      cur.additions++;
    } else if (t === '-') {
      hunk.lines.push({ type: 'del', text: body, oldNo: oldNo++, newNo: null });
      cur.deletions++;
    } else if (t === ' ' || raw === '') {
      // A completely empty line is context too (git prints a single space,
      // but tolerate the bare-empty form some tools emit).
      hunk.lines.push({ type: 'ctx', text: t === ' ' ? body : '', oldNo: oldNo++, newNo: newNo++ });
    }
  }
  return files;
}

// ---------------------------------------------------------------------------
// diffTexts: compute hunks between two strings (edit tool oldText/newText)
// ---------------------------------------------------------------------------

interface Op {
  t: DiffLineType;
  s: string;
}

// lcsOps produces the add/del/ctx op stream via classic LCS dynamic
// programming. Inputs above the size guard degrade to a whole-block
// replace (still correct, just coarser).
const DP_GUARD = 4_000_000;

function lcsOps(a: string[], b: string[]): Op[] {
  const n = a.length;
  const m = b.length;
  if (n === 0 && m === 0) return [];
  if (n * m > DP_GUARD) {
    return [
      ...a.map(s => ({ t: 'del', s }) as Op),
      ...b.map(s => ({ t: 'add', s }) as Op),
    ];
  }
  const W = m + 1;
  // dp[i*W + j] = LCS length of a[i:] vs b[j:]
  const dp = new Uint32Array((n + 1) * W);
  for (let i = n - 1; i >= 0; i--) {
    for (let j = m - 1; j >= 0; j--) {
      dp[i * W + j] =
        a[i] === b[j]
          ? dp[(i + 1) * W + j + 1] + 1
          : Math.max(dp[(i + 1) * W + j], dp[i * W + j + 1]);
    }
  }
  const ops: Op[] = [];
  let i = 0;
  let j = 0;
  while (i < n && j < m) {
    if (a[i] === b[j]) {
      ops.push({ t: 'ctx', s: a[i] });
      i++;
      j++;
    } else if (dp[(i + 1) * W + j] >= dp[i * W + j + 1]) {
      ops.push({ t: 'del', s: a[i++] });
    } else {
      ops.push({ t: 'add', s: b[j++] });
    }
  }
  while (i < n) ops.push({ t: 'del', s: a[i++] });
  while (j < m) ops.push({ t: 'add', s: b[j++] });
  return ops;
}

// diffTexts computes unified-diff-style hunks between oldText and newText
// with `context` unchanged lines around each change. Adjacent changes no
// more than 2*context context lines apart merge into one hunk. Returns []
// when the texts are identical.
export function diffTexts(oldText: string, newText: string, context = 3): DiffHunk[] {
  if (oldText === newText) return [];
  const a = oldText === '' ? [] : oldText.split('\n');
  const b = newText === '' ? [] : newText.split('\n');
  const ops = lcsOps(a, b);
  const changed: number[] = [];
  for (let i = 0; i < ops.length; i++) if (ops[i].t !== 'ctx') changed.push(i);
  if (changed.length === 0) return [];

  // Group changed ops: a gap of > 2*context ctx lines splits hunks.
  const groups: [number, number][] = [];
  let gs = changed[0];
  let prev = changed[0];
  for (let k = 1; k < changed.length; k++) {
    const c = changed[k];
    if (c - prev - 1 > context * 2) {
      groups.push([gs, prev]);
      gs = c;
    }
    prev = c;
  }
  groups.push([gs, prev]);

  // Old/new line numbers immediately before each op (1-based).
  const oldNos = new Array<number>(ops.length + 1);
  const newNos = new Array<number>(ops.length + 1);
  let on = 1;
  let nn = 1;
  for (let i = 0; i < ops.length; i++) {
    oldNos[i] = on;
    newNos[i] = nn;
    if (ops[i].t !== 'add') on++;
    if (ops[i].t !== 'del') nn++;
  }

  const hunks: DiffHunk[] = [];
  for (const [s, e] of groups) {
    const from = Math.max(0, s - context);
    const to = Math.min(ops.length - 1, e + context);
    const oldStart = oldNos[from];
    const newStart = newNos[from];
    const lines: DiffLine[] = [];
    let oldCount = 0;
    let newCount = 0;
    for (let i = from; i <= to; i++) {
      const op = ops[i];
      lines.push({
        type: op.t,
        text: op.s,
        oldNo: op.t === 'add' ? null : oldNos[i],
        newNo: op.t === 'del' ? null : newNos[i],
      });
      if (op.t !== 'add') oldCount++;
      if (op.t !== 'del') newCount++;
    }
    hunks.push({
      header: `@@ -${oldStart},${oldCount} +${newStart},${newCount} @@`,
      oldStart,
      newStart,
      lines,
    });
  }
  return hunks;
}
