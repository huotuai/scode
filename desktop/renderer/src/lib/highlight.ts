// Tiny dependency-free syntax highlighter for the diff views. It is a
// table-driven scanner (keywords / strings / comments / numbers / types /
// calls), NOT a parser: the goal is readable diff coloring for the common
// languages, not perfect grammar coverage.

export interface Token {
  /** '', 'kw', 'str', 'com', 'num', 'type', 'fn' */
  t: string;
  s: string;
}

interface LangSpec {
  line: string[]; // line-comment openers
  block: [string, string][]; // block-comment (or python docstring) pairs
  strings: string[]; // quote characters
  kw: Set<string>;
  capTypes?: boolean; // Capitalized identifiers are types
  keyColon?: boolean; // identifier followed by ':' is a key (css, yaml)
  keyEq?: boolean; // identifier followed by '=' is a key (toml, html attrs)
  atKw?: boolean; // @word is a keyword (css at-rules, decorators)
  tagOpen?: boolean; // <tag names are keywords (html/xml)
}

type SpecInput = Partial<Omit<LangSpec, 'kw'>> & { kw: string[] };

function spec(partial: SpecInput): LangSpec {
  const { kw, ...rest } = partial;
  return {
    line: ['//'],
    block: [['/*', '*/']],
    strings: ['"', "'", '`'],
    ...rest,
    kw: new Set(kw),
  };
}

const JS_KW =
  'const let var function return if else for while do switch case default break continue class extends new delete typeof instanceof in of try catch finally throw async await import export from this super null undefined true false void yield static get set implements interface type enum public private protected readonly abstract declare namespace keyof infer satisfies never unknown any string number boolean object symbol bigint';
const C_KW =
  'auto break case char const continue default do double else enum extern float for goto if inline int long register restrict return short signed sizeof static struct switch typedef union unsigned void volatile while bool true false NULL';
const JAVA_KW =
  'abstract assert boolean break byte case catch char class const continue default do double else enum extends final finally float for goto if implements import instanceof int interface long native new package private protected public return short static strictfp super switch synchronized this throw throws transient try void volatile while true false null var record sealed permits yield';
const PY_KW =
  'def class return if elif else for while break continue import from as with try except finally raise lambda pass yield global nonlocal assert del in is not and or None True False async await match case';
const GO_KW =
  'package import func return if else for range switch case default break continue go defer select struct interface map chan var const type nil true false iota fallthrough';
const RS_KW =
  'fn let mut const static struct enum impl trait pub use mod crate self Self super where for while loop if else match return break continue move ref async await dyn as in type unsafe extern true false';
const SH_KW =
  'if then else elif fi for while until do done case esac function in select echo cd export local readonly return exit source alias set unset shift read printf eval exec test declare trap umask';
const SQL_KW =
  'select insert update delete from where join inner left right outer full on group by order having limit offset union all distinct as and or not null is in exists between like case when then else end create table alter drop index view primary key foreign references unique default values into set begin commit rollback transaction grant revoke';

const LANGS: Record<string, LangSpec> = {
  ts: spec({ kw: JS_KW.split(' '), capTypes: true }),
  js: spec({ kw: JS_KW.split(' '), capTypes: true }),
  go: spec({ kw: GO_KW.split(' '), capTypes: true, strings: ['"', "'", '`'] }),
  py: spec({
    kw: PY_KW.split(' '),
    line: ['#'],
    block: [
      ['"""', '"""'],
      ["'''", "'''"],
    ],
    strings: ['"', "'"],
    capTypes: true,
  }),
  rs: spec({ kw: RS_KW.split(' '), strings: ['"'], capTypes: true }),
  java: spec({ kw: JAVA_KW.split(' '), capTypes: true }),
  kt: spec({ kw: (JAVA_KW + ' val var fun data object companion init by lazy when is as').split(' '), capTypes: true }),
  c: spec({ kw: C_KW.split(' '), capTypes: true }),
  cpp: spec({
    kw: (C_KW + ' namespace template typename class public private protected virtual override final noexcept constexpr auto nullptr this new delete using friend operator mutable explicit export concept requires').split(' '),
    capTypes: true,
  }),
  cs: spec({
    kw: (JAVA_KW + ' string namespace using var get set value partial async await nameof is as base decimal object delegate event out ref params readonly sealed internal unsafe fixed stackalloc uint ulong ushort byte sbyte').split(' '),
    capTypes: true,
  }),
  json: spec({ kw: ['true', 'false', 'null'], line: [], block: [], strings: ['"'] }),
  yaml: spec({
    kw: ['true', 'false', 'null', 'yes', 'no', 'on', 'off', '~'],
    line: ['#'],
    block: [],
    strings: ['"', "'"],
    keyColon: true,
  }),
  toml: spec({ kw: ['true', 'false'], line: ['#'], block: [], strings: ['"', "'"], keyEq: true }),
  sh: spec({ kw: SH_KW.split(' '), line: ['#'], block: [], strings: ['"', "'", '`'] }),
  sql: spec({ kw: SQL_KW.split(' '), line: ['--'], strings: ["'", '"', '`'] }),
  css: spec({ kw: ['important'], block: [['/*', '*/']], strings: ['"', "'"], line: [], keyColon: true, atKw: true }),
  html: spec({ kw: [], line: [], block: [['<!--', '-->']], strings: ['"', "'"], tagOpen: true, keyEq: true }),
};

const EXT_TO_LANG: Record<string, string> = {
  ts: 'ts', tsx: 'ts', mts: 'ts', cts: 'ts',
  js: 'js', jsx: 'js', mjs: 'js', cjs: 'js',
  go: 'go', py: 'py', rs: 'rs', java: 'java', kt: 'kt', kts: 'kt',
  c: 'c', h: 'c', cc: 'cpp', cpp: 'cpp', cxx: 'cpp', hpp: 'cpp', hh: 'cpp',
  cs: 'cs', json: 'json', jsonc: 'json',
  yaml: 'yaml', yml: 'yaml', toml: 'toml',
  sh: 'sh', bash: 'sh', zsh: 'sh',
  sql: 'sql', css: 'css', scss: 'css', less: 'css',
  html: 'html', htm: 'html', xml: 'html', svg: 'html', vue: 'html',
};

/** langFromPath maps a file path to a highlighter language id ('' = none). */
export function langFromPath(path: string): string {
  const base = path.split(/[\\/]/).pop() ?? '';
  if (base === 'Dockerfile' || base.endsWith('.sh')) return 'sh';
  const dot = base.lastIndexOf('.');
  if (dot <= 0) return '';
  return EXT_TO_LANG[base.slice(dot + 1).toLowerCase()] ?? '';
}

// Fence aliases beyond EXT_TO_LANG (markdown code fences use full names).
const FENCE_ALIAS: Record<string, string> = {
  typescript: 'ts', javascript: 'js', python: 'py', golang: 'go',
  shell: 'sh', console: 'sh', dockerfile: 'sh',
  'c++': 'cpp', csharp: 'cs', 'c#': 'cs', objc: 'c',
  plaintext: '', text: '', none: '',
};

/** langFromFence maps a markdown fence info string (```go) to a
 *  highlighter language id ('' = none). */
export function langFromFence(info: string): string {
  const n = info.trim().toLowerCase().split(/\s+/)[0] ?? '';
  if (!n) return '';
  if (n in FENCE_ALIAS) return FENCE_ALIAS[n];
  if (LANGS[n]) return n;
  return EXT_TO_LANG[n] ?? '';
}

function isIdentChar(ch: string | undefined): boolean {
  return !!ch && /[A-Za-z0-9_$]/.test(ch);
}

// highlightLines tokenizes the given lines (state carried ACROSS lines, so
// multiline comments and strings inside the shown window color correctly)
// and returns one token array per input line.
export function highlightLines(lines: string[], lang: string): Token[][] {
  const spec = LANGS[lang];
  if (!spec || lines.length === 0) return lines.map(s => (s ? [{ t: '', s }] : []));
  const text = lines.join('\n');
  const n = text.length;
  const tokens: Token[] = [];
  let plainStart = 0;
  let i = 0;
  const push = (t: string, s: string) => {
    if (s) tokens.push({ t, s });
  };
  const flush = (upto: number) => push('', text.slice(plainStart, upto));

  while (i < n) {
    const ch = text[i];
    // line comments
    let jumped = false;
    for (const lc of spec.line) {
      if (lc && text.startsWith(lc, i)) {
        flush(i);
        let j = text.indexOf('\n', i);
        if (j === -1) j = n;
        push('com', text.slice(i, j));
        i = j;
        plainStart = j;
        jumped = true;
        break;
      }
    }
    if (jumped) continue;
    // block comments / docstrings
    for (const [o, c] of spec.block) {
      if (text.startsWith(o, i)) {
        flush(i);
        let j = text.indexOf(c, i + o.length);
        j = j === -1 ? n : j + c.length;
        push('com', text.slice(i, j));
        i = j;
        plainStart = j;
        jumped = true;
        break;
      }
    }
    if (jumped) continue;
    // strings
    if (spec.strings.includes(ch)) {
      flush(i);
      let j = i + 1;
      while (j < n) {
        if (text[j] === '\\') {
          j += 2;
          continue;
        }
        if (text[j] === ch) {
          j++;
          break;
        }
        j++;
      }
      push('str', text.slice(i, Math.min(j, n)));
      i = Math.min(j, n);
      plainStart = i;
      continue;
    }
    // html/xml tag names
    if (spec.tagOpen && ch === '<' && /[A-Za-z/!]/.test(text[i + 1] ?? '')) {
      flush(i);
      let j = i + 1;
      if (text[j] === '/') j++;
      while (j < n && /[A-Za-z0-9-]/.test(text[j])) j++;
      push('kw', text.slice(i, j));
      i = j;
      plainStart = j;
      continue;
    }
    // css at-rules / decorators
    if (spec.atKw && ch === '@' && isIdentChar(text[i + 1])) {
      flush(i);
      let j = i + 1;
      while (j < n && /[A-Za-z0-9_-]/.test(text[j])) j++;
      push('kw', text.slice(i, j));
      i = j;
      plainStart = j;
      continue;
    }
    // numbers (not mid-identifier)
    if (/[0-9]/.test(ch) && !isIdentChar(text[i - 1])) {
      flush(i);
      let j = i;
      while (j < n && /[0-9a-fA-FxXoObB._]/.test(text[j])) j++;
      push('num', text.slice(i, j));
      i = j;
      plainStart = j;
      continue;
    }
    // identifiers and keywords
    if (/[A-Za-z_$]/.test(ch)) {
      let j = i;
      while (j < n && /[A-Za-z0-9_$]/.test(text[j])) j++;
      const word = text.slice(i, j);
      let k = j;
      while (k < n && (text[k] === ' ' || text[k] === '\t')) k++;
      const next = text[k] ?? '';
      let t = '';
      if (spec.kw.has(word)) t = 'kw';
      else if (next === '(') t = 'fn';
      else if (spec.keyColon && next === ':' && text[k + 1] !== ':') t = 'type';
      else if (spec.keyEq && next === '=' && text[k + 1] !== '=' && text[k + 1] !== '>') t = 'type';
      else if (spec.capTypes && /^[A-Z]/.test(word)) t = 'type';
      if (t) {
        flush(i);
        push(t, word);
        plainStart = j;
      }
      i = j;
      continue;
    }
    i++;
  }
  flush(n);

  // Split the flat token stream back into per-line arrays.
  const out: Token[][] = [[]];
  for (const tok of tokens) {
    let s = tok.s;
    for (;;) {
      const nl = s.indexOf('\n');
      if (nl === -1) {
        if (s) out[out.length - 1].push({ t: tok.t, s });
        break;
      }
      out[out.length - 1].push({ t: tok.t, s: s.slice(0, nl) });
      out.push([]);
      s = s.slice(nl + 1);
    }
  }
  while (out.length < lines.length) out.push([]);
  return out;
}
