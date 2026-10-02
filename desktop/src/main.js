// scode desktop — Electron main process.
// Owns a POOL of `scode serve` children (stdio NDJSON JSON-RPC, the only
// transport), one per workspace folder, and bridges them to the renderer
// over IPC. Sessions bind to cwd, so parallel sessions across project
// folders run on parallel servers; switching sessions or workspaces never
// tears a server down.
// Design: docs/design-permission-plan-desktop.md §五.
const { app, BrowserWindow, ipcMain, dialog, shell } = require('electron');
const { spawn, execFile } = require('child_process');
const fs = require('fs');
const os = require('os');
const path = require('path');
const readline = require('readline');

// App identity shown in the taskbar, jump list, notifications and the
// Windows "Apps" view. In dev the OS process is still electron.exe, so
// Task Manager's per-process name only changes in a packaged build (see
// README): these calls can't rename the running binary.
app.setName('SCode');
if (process.platform === 'win32') app.setAppUserModelId('com.scode.desktop');

let win = null;
// The active workspace: where new sessions are created. Other folders'
// servers stay alive in the pool.
let workspace = process.env.SCODE_WORKSPACE || process.cwd();

const servers = new Map(); // wsKey → Server {proc, rl, pending, nextId, workspace, key}
const sessionOwner = new Map(); // sessionId → wsKey (routes session-scoped calls)
const approvalRoutes = new Map(); // renderer-facing approval id → {srv, id}
let nextApprovalId = 1;

function wsKey(dir) {
  const r = path.resolve(dir);
  return process.platform === 'win32' ? r.toLowerCase() : r;
}

function binPath() {
  if (process.env.SCODE_BIN) return process.env.SCODE_BIN;
  const name = process.platform === 'win32' ? 'scode.exe' : 'scode';
  const bundled = path.join(__dirname, '..', '..', 'dist', name);
  if (fs.existsSync(bundled)) return bundled;
  return name; // fall back to PATH
}

// ---------------------------------------------------------------------------
// session history (grouped by project folder)
//
// Sessions are persisted per cwd under <configDir>/sessions/<safe-cwd>/
// (internal/session.DefaultRoot). The desktop wants the full history across
// every project, so the main process reads each session header's `cwd` — the
// folder a session belongs to. Sessions whose header cannot be read have no
// folder and are dropped by the UI (they cannot be grouped).
// ---------------------------------------------------------------------------

function configDir() {
  return process.env.SCODE_DIR || path.join(os.homedir(), '.scode');
}

// sessionStoreDir mirrors Go's session.DefaultRoot sanitization (Windows and
// POSIX separators, drive colons and spaces folded to [--_]) so a header cwd
// maps back to the directory the server wrote.
function sessionStoreDir(cwd) {
  let safe = path.normalize(cwd).replace(/[:\\/ ]/g, ch => (ch === ' ' ? '_' : '-'));
  safe = safe.replace(/^-/, '').replace(/^\./, '');
  if (!safe) safe = 'default';
  return path.join(configDir(), 'sessions', safe);
}

// cleanTitle turns a stored user message into a sidebar title. Explicit
// `$name` skill invocations are persisted as the expanded block
// `<skill name="…">…body…</skill>` plus optional args; the body is noise
// for a title, so prefer the args, else the skill name.
function cleanTitle(text) {
  const m = /^<skill\s+name="([^"]*)"[^>]*>[\s\S]*?<\/skill>\s*([\s\S]*)$/.exec(text);
  if (m) {
    const args = m[2].replace(/\s+/g, ' ').trim();
    return args || `skill ${m[1]}`;
  }
  return text;
}

// parseSessionLine extracts what the history list needs from one JSONL
// line: whether it is a message, and (for the first user turn) its text.
// v2 wraps messages as {"type":"message","message":{…}}; legacy v1 lines
// are the bare message objects.
function parseSessionLine(line) {
  if (!line) return null;
  let obj;
  try {
    obj = JSON.parse(line);
  } catch {
    return null; // torn tail / future line
  }
  const msg = obj.type === 'message' ? obj.message : obj;
  if (!msg || typeof msg.role !== 'string') return null;
  let userText = '';
  if (msg.role === 'user' && Array.isArray(msg.content)) {
    const parts = [];
    for (const b of msg.content) {
      if (b && b.kind === 'text' && typeof b.text === 'string') parts.push(b.text);
    }
    userText = cleanTitle(parts.join(' ').replace(/\s+/g, ' ').trim());
  }
  return { isMessage: true, userText };
}

// parseTitleLine returns a persisted (server-generated) title entry, or ''.
function parseTitleLine(line) {
  if (!line.includes('"type":"title"')) return ''; // cheap pre-filter
  try {
    const obj = JSON.parse(line);
    return obj && obj.type === 'title' && typeof obj.title === 'string' ? obj.title.trim() : '';
  } catch {
    return '';
  }
}

// The sidebar title prefers a persisted server-generated title entry (an
// LLM summary written during the first turn); when there is none it falls
// back to the first user message. readSessionMeta also reports the header
// cwd and whether any message exists (header-only drafts created by
// toggling plan/sandbox/model stay out of history). The scan stops when the
// persisted title is found (or at the byte cap).
const TITLE_MAX = 48;
// depth guards the fork-of chain: a forked session's title is "fork of
// <parent title>", resolved by reading the parent file the header's
// parentSession path points to.
function readSessionMeta(file, depth = 0) {
  const out = { cwd: '', hasMessage: false, title: '' };
  let derived = ''; // first user message (fallback title)
  let saved = ''; // persisted server-generated title (preferred)
  let parent = ''; // fork origin: absolute path of the source session file
  let partial = '';
  let firstLine = true;
  try {
    const fd = fs.openSync(file, 'r');
    try {
      const buf = Buffer.alloc(256 * 1024);
      let pos = 0;
      for (;;) {
        const n = fs.readSync(fd, buf, 0, buf.length, pos);
        if (n <= 0) break;
        pos += n;
        const lines = (partial + buf.toString('utf8', 0, n)).split('\n');
        partial = lines.pop() ?? ''; // last line may be split across chunks
        for (const line of lines) {
          if (firstLine) {
            firstLine = false;
            try {
              const h = JSON.parse(line);
              if (typeof h.cwd === 'string') out.cwd = h.cwd;
              if (typeof h.parentSession === 'string') parent = h.parentSession;
            } catch {
              /* malformed header: leave cwd empty (session is dropped) */
            }
            continue;
          }
          const meta = parseSessionLine(line);
          if (meta) {
            out.hasMessage = true;
            if (!derived && meta.userText) derived = meta.userText;
            continue;
          }
          if (!saved) saved = parseTitleLine(line);
        }
        if (saved || pos >= 512 * 1024) break;
      }
    } finally {
      fs.closeSync(fd);
    }
  } catch {
    return { cwd: '', hasMessage: false, title: '' };
  }
  out.title = saved || derived;
  if (parent && depth < 4) {
    // Forked session: title after its origin. A missing/unreadable
    // parent falls back to its session id (the file's basename).
    let pt = '';
    try {
      if (fs.existsSync(parent)) pt = readSessionMeta(parent, depth + 1).title;
    } catch {
      /* fall through to the id */
    }
    if (!pt) pt = path.basename(parent).replace(/\.jsonl$/i, '');
    if (pt) out.title = `fork of ${pt}`;
  }
  if (out.title.length > TITLE_MAX) out.title = out.title.slice(0, TITLE_MAX) + '…';
  return out;
}

// readDefaultSandbox mirrors cli.Setup's deployment default: settings.sandbox
// .mode when valid, otherwise workspace-write (the shipped posture).
function readDefaultSandbox() {
  const valid = ['read-only', 'workspace-write', 'danger-full-access'];
  try {
    const raw = fs.readFileSync(path.join(configDir(), 'settings.json'), 'utf8');
    const mode = JSON.parse(raw)?.sandbox?.mode;
    if (typeof mode === 'string' && valid.includes(mode)) return mode;
  } catch {
    /* absent or unparseable settings: use the shipped default */
  }
  return 'workspace-write';
}

// listSessionHistory returns every session across all project folders,
// newest activity first. The header cwd is the grouping key; '' marks an
// ungroupable session.
function listSessionHistory() {
  const base = path.join(configDir(), 'sessions');
  let dirs;
  try {
    dirs = fs.readdirSync(base, { withFileTypes: true });
  } catch {
    return [];
  }
  const out = [];
  for (const d of dirs) {
    if (!d.isDirectory()) continue;
    const dir = path.join(base, d.name);
    let files;
    try {
      files = fs.readdirSync(dir);
    } catch {
      continue;
    }
    for (const f of files) {
      if (!f.endsWith('.jsonl')) continue;
      const full = path.join(dir, f);
      const meta = readSessionMeta(full);
      // Header-only drafts (created by toggling plan/sandbox/model on an
      // unused chat) are clutter, not history.
      if (!meta.hasMessage) continue;
      let mtime = 0;
      try {
        mtime = fs.statSync(full).mtimeMs;
      } catch {
        /* unreadable entry: keep it, sorted last */
      }
      out.push({
        id: f.slice(0, -'.jsonl'.length),
        cwd: meta.cwd,
        mtime,
        title: meta.title,
      });
    }
  }
  out.sort((a, b) => b.mtime - a.mtime);
  return out;
}

// ---------------------------------------------------------------------------
// git changes (the diff panel)
//
// The renderer's changes panel shows the workspace's uncommitted work. The
// main process shells out to git (never the agent's server): status for the
// file list, and one combined `git diff HEAD` for staged+unstaged content.
// ---------------------------------------------------------------------------

const GIT_DIFF_CAP = 512 * 1024; // beyond this the panel shows a truncation note

// runGit executes git in a workspace folder. The first stderr line becomes
// the error message; ENOENT (git not installed) is flagged on err.code.
function runGit(args, cwd) {
  return new Promise((resolve, reject) => {
    execFile(
      'git',
      args,
      { cwd, maxBuffer: 32 * 1024 * 1024, timeout: 20000 },
      (err, stdout, stderr) => {
        if (err) {
          const msg = String(stderr || err.message || 'git failed').trim().split('\n')[0];
          const e = new Error(msg || 'git failed');
          e.code = err.code;
          reject(e);
          return;
        }
        resolve(stdout);
      },
    );
  });
}

// parseGitStatus reads `git status --porcelain=v1 -z` output: NUL-separated
// "XY path" entries (rename/copy entries carry a second NUL field, skipped).
function parseGitStatus(raw) {
  const out = [];
  const entries = raw.split('\0');
  for (let i = 0; i < entries.length; i++) {
    const e = entries[i];
    if (e.length < 4) continue;
    const x = e[0];
    const y = e[1];
    const p = e.slice(3);
    let status = 'M';
    if (x === '?' || y === '?') status = '??';
    else if (x === 'A' || y === 'A') status = 'A';
    else if (x === 'D' || y === 'D') status = 'D';
    else if (x === 'R' || y === 'R') status = 'R';
    if (x === 'R' || x === 'C') i++; // rename/copy: skip the source-path field
    out.push({ path: p, status, untracked: status === '??' });
  }
  return out;
}

// gitChanges collects the file list plus the combined unified diff for a
// workspace. `git diff HEAD` covers staged and unstaged changes in one
// output; a repo with no commits yet falls back to diff + diff --cached.
async function gitChanges(cwd) {
  let inside;
  try {
    inside = (await runGit(['rev-parse', '--is-inside-work-tree'], cwd)).trim();
  } catch (err) {
    if (err.code === 'ENOENT') throw new Error('git command not found — install Git first');
    throw new Error('not a git repository');
  }
  if (inside !== 'true') throw new Error('not a git repository');
  const files = parseGitStatus(await runGit(['status', '--porcelain=v1', '-z'], cwd));
  let hasHead = true;
  try {
    await runGit(['rev-parse', '--verify', 'HEAD'], cwd);
  } catch {
    hasHead = false; // fresh repo, no commits yet
  }
  let diff;
  if (hasHead) {
    diff = await runGit(['diff', 'HEAD', '--no-color', '--no-ext-diff', '--'], cwd);
  } else {
    const staged = await runGit(['diff', '--cached', '--no-color', '--no-ext-diff', '--'], cwd);
    const unstaged = await runGit(['diff', '--no-color', '--no-ext-diff', '--'], cwd);
    diff = staged + unstaged;
  }
  let truncated = false;
  if (diff.length > GIT_DIFF_CAP) {
    diff = diff.slice(0, GIT_DIFF_CAP);
    truncated = true;
  }
  return { files, diff, truncated };
}

function send(srv, frame) {
  if (srv.proc && srv.proc.stdin.writable) {
    srv.proc.stdin.write(JSON.stringify(frame) + '\n');
  }
}

function rpcOn(srv, method, params) {
  return new Promise((resolve, reject) => {
    const id = srv.nextId++;
    srv.pending.set(id, { resolve, reject });
    send(srv, { jsonrpc: '2.0', id, method, params: params || {} });
  });
}

// ensureServer returns the live server for a workspace, spawning and
// handshaking it on first use. Spawns are lazy; nothing here disturbs the
// other folders' servers.
function ensureServer(dir) {
  const key = wsKey(dir);
  let srv = servers.get(key);
  if (srv) return srv;
  const proc = spawn(binPath(), ['serve'], { cwd: dir });
  srv = { proc, rl: null, pending: new Map(), nextId: 1, workspace: dir, key };
  servers.set(key, srv);
  proc.stderr.on('data', d => console.error('[scode]', String(d).trimEnd()));
  srv.rl = readline.createInterface({ input: proc.stdout });
  srv.rl.on('line', line => {
    let m;
    try { m = JSON.parse(line); } catch { return; }
    if (m.id !== undefined && (m.result !== undefined || m.error !== undefined)) {
      // response to a main→server call
      const p = srv.pending.get(m.id);
      if (p) {
        srv.pending.delete(m.id);
        m.error ? p.reject(new Error(m.error.message)) : p.resolve(m.result);
      }
      return;
    }
    if (m.method === 'approval/request' && m.id !== undefined) {
      // server→client reverse request: the renderer answers it. Each
      // server numbers its own reverse calls from 1, so ids from two
      // servers collide — namespace them and remember the route back.
      const gid = nextApprovalId++;
      approvalRoutes.set(gid, { srv, id: m.id });
      if (m.params && typeof m.params.sessionId === 'string') {
        sessionOwner.set(m.params.sessionId, key);
      }
      win?.webContents.send('approval', { id: gid, params: m.params });
      return;
    }
    if (m.method === 'session/event') {
      if (m.params && typeof m.params.sessionId === 'string') {
        sessionOwner.set(m.params.sessionId, key);
      }
      win?.webContents.send('event', m.params);
    }
  });
  const onGone = code => {
    if (servers.get(key) !== srv) return; // superseded entry
    servers.delete(key);
    for (const [, p] of srv.pending) p.reject(new Error(`scode serve exited (${code})`));
    srv.pending.clear();
    for (const [gid, r] of approvalRoutes) {
      if (r.srv === srv) approvalRoutes.delete(gid);
    }
    win?.webContents.send('server-exit', { workspace: dir, code: code ?? -1 });
  };
  proc.on('exit', onGone);
  proc.on('error', err => {
    console.error('[scode] spawn failed:', err);
    onGone(-1);
  });
  // handshake; stdio ordering lets later calls queue behind it.
  rpcOn(srv, 'initialize', {}).catch(err => console.error('initialize failed:', err));
  return srv;
}

// route picks the server a call belongs to: the session's owning server
// when known, else the hinted workspace, else the active workspace.
function route(method, params, hint) {
  const sid = params && typeof params === 'object' ? params.sessionId : '';
  if (sid && typeof sid === 'string') {
    const srv = servers.get(sessionOwner.get(sid));
    if (srv) return srv;
  }
  return ensureServer(typeof hint === 'string' && hint ? hint : workspace);
}

function rpc(method, params, hint) {
  const srv = route(method, params, hint);
  return rpcOn(srv, method, params).then(result => {
    if (
      (method === 'session/create' || method === 'session/resume') &&
      result && typeof result.sessionId === 'string'
    ) {
      sessionOwner.set(result.sessionId, srv.key);
    }
    if (method === 'session/delete' && params && params.sessionId) {
      sessionOwner.delete(params.sessionId);
    }
    return result;
  });
}

function stopAll() {
  for (const srv of servers.values()) {
    if (srv.rl) srv.rl.close();
    srv.proc.kill();
    for (const [, p] of srv.pending) p.reject(new Error('app quitting'));
    srv.pending.clear();
  }
  servers.clear();
  approvalRoutes.clear();
}

// handle registers a non-server IPC channel with the same structured
// {ok, result|error} envelope as the rpc channel, so renderer failures are
// clean messages instead of Electron's "Error invoking remote method" prefix.
function handle(channel, fn) {
  ipcMain.handle(channel, async (_event, ...args) => {
    try {
      return { ok: true, result: await fn(...args) };
    } catch (err) {
      return { ok: false, error: String((err && err.message) || err) };
    }
  });
}

app.whenReady().then(() => {
  win = new BrowserWindow({
    width: 1200,
    height: 800,
    minWidth: 720,
    minHeight: 480,
    backgroundColor: '#f5f6f8', // light theme default: avoids a dark flash on boot
    webPreferences: {
      preload: path.join(__dirname, 'preload.js'),
      contextIsolation: true,
      nodeIntegration: false,
      // The right-side browser panel embeds pages in a <webview> (a
      // separate guest process), so webview guests must be allowed.
      webviewTag: true,
    },
  });
  // Dev: Vite dev server (`npm run dev` passes --dev). Prod: the built
  // renderer next to the app.
  const devUrl = process.env.SCODE_DEV_URL || (process.argv.includes('--dev') ? 'http://localhost:5173' : '');
  // Diagnostics: SCODE_DIAG=1 logs renderer console/crash events;
  // SCODE_EVAL=<js> runs a script in the page after load (UI smoke tests).
  if (process.env.SCODE_DIAG) {
    win.webContents.on('console-message', e => console.error('[console]', e.level, e.message.slice(0, 300)));
    win.webContents.on('render-process-gone', (_e, d) => console.error('[gone]', JSON.stringify(d)));
    win.webContents.on('preload-error', (_e, p, err) => console.error('[preload-err]', p, String(err)));
    win.webContents.on('did-fail-load', (_e, c, d) => console.error('[fail-load]', c, d));
    const evalJs = process.env.SCODE_EVAL;
    if (evalJs) {
      win.webContents.on('did-finish-load', () => {
        setTimeout(() => win.webContents.executeJavaScript(evalJs)
          .then(r => console.error('[eval]', JSON.stringify(r)))
          .catch(err => console.error('[eval-err]', String(err))), 2500);
      });
    }
  }
  // A clicked link must never navigate the chat UI itself away. External
  // navigations and window.open calls are rerouted to the renderer, which
  // opens them in the right-side browser panel instead.
  const isAppUrl = u =>
    u === 'about:blank' ||
    u.startsWith('file://') ||
    u.startsWith('devtools://') ||
    (devUrl && u.startsWith(devUrl));
  const openInPanel = url => {
    if (/^https?:\/\//i.test(url)) win?.webContents.send('open-url', url);
  };
  win.webContents.on('will-navigate', (e, url) => {
    if (isAppUrl(url)) return;
    e.preventDefault();
    openInPanel(url);
  });
  win.webContents.setWindowOpenHandler(({ url }) => {
    if (!isAppUrl(url)) openInPanel(url);
    return { action: 'deny' };
  });
  if (devUrl) {
    win.loadURL(devUrl);
  } else {
    win.loadFile(path.join(__dirname, '..', 'dist-renderer', 'index.html'));
  }

  ipcMain.handle('rpc', async (_e, { method, params, hint }) => {
    // Structured errors, not rejections: Electron prefixes handler
    // rejections with "Error occurred in handler for 'rpc': …" —
    // useless noise in the UI. The preload wrapper rethrows clean.
    try {
      return { ok: true, result: await rpc(method, params, hint) };
    } catch (err) {
      return { ok: false, error: String(err && err.message || err) };
    }
  });
  ipcMain.handle('approval-answer', (_e, { id, result }) => {
    const r = approvalRoutes.get(id);
    if (!r) return; // server already gone
    approvalRoutes.delete(id);
    send(r.srv, { jsonrpc: '2.0', id: r.id, result });
  });
  ipcMain.handle('get-workspace', () => workspace);
  handle('set-workspace', async dir => {
    if (!dir || typeof dir !== 'string') return workspace;
    if (path.resolve(dir) === path.resolve(workspace)) return workspace;
    try {
      if (!fs.statSync(dir).isDirectory()) throw new Error('not a directory');
    } catch {
      throw new Error(`workspace unavailable: ${dir}`);
    }
    // Switch the active folder and make sure its server is up. The
    // previous folder's server (and its running sessions) stays alive.
    workspace = dir;
    ensureServer(dir);
    return workspace;
  });
  handle('list-sessions', () => ({ sessions: listSessionHistory() }));
  handle('get-default-sandbox', () => readDefaultSandbox());
  handle('git-diff', dir => gitChanges(typeof dir === 'string' && dir ? dir : workspace));
  handle('delete-session', async ({ id, cwd }) => {
    if (!id || /[\\/]/.test(id)) throw new Error('invalid session id');
    const srv = servers.get(sessionOwner.get(id));
    if (srv) {
      // Live handle: the owning server cancels any run in flight and
      // removes the file itself.
      await rpcOn(srv, 'session/delete', { sessionId: id });
      sessionOwner.delete(id);
      return { ok: true };
    }
    const file = path.join(sessionStoreDir(cwd || workspace), id + '.jsonl');
    try {
      fs.unlinkSync(file);
    } catch (err) {
      if (err.code !== 'ENOENT') throw err;
    }
    return { ok: true };
  });
  handle('pick-workspace', async () => {
    const r = await dialog.showOpenDialog(win, { properties: ['openDirectory'] });
    if (r.canceled || r.filePaths.length === 0) return null; // canceled
    const dir = r.filePaths[0];
    if (path.resolve(dir) === path.resolve(workspace)) return workspace; // unchanged
    workspace = dir;
    ensureServer(dir); // spawn alongside the previous folder's server
    return workspace;
  });

  handle('open-external', async url => {
    if (typeof url !== 'string' || !/^https?:\/\//i.test(url)) {
      throw new Error('only http/https links can be opened');
    }
    await shell.openExternal(url);
    return { ok: true };
  });

  handle('open-path', async dir => {
    if (typeof dir !== 'string' || !dir) throw new Error('invalid path');
    const err = await shell.openPath(dir); // '' on success
    if (err) throw new Error(err);
    return { ok: true };
  });

  handle('open-in-vscode', async dir => {
    if (typeof dir !== 'string' || !dir) throw new Error('invalid path');
    // `code` is a shim script (code.cmd on Windows), so it needs a shell;
    // it hands off to the running instance and exits quickly, so awaiting
    // close tells us whether the command actually exists on PATH.
    await new Promise((resolve, reject) => {
      const fail = () => reject(new Error('failed to launch VSCode: the code command is not on PATH'));
      const child = spawn('code', [dir], { shell: true, stdio: 'ignore' });
      child.on('error', fail);
      child.on('close', code => (code === 0 ? resolve() : fail()));
    });
    return { ok: true };
  });

  ensureServer(workspace);
});

// Guest pages (the browser panel's <webview>) get their own webContents.
// Keep their target=_blank / window.open navigations inside the guest
// rather than spawning a system-browser window.
app.on('web-contents-created', (_e, contents) => {
  if (contents.getType() !== 'webview') return;
  contents.setWindowOpenHandler(({ url }) => {
    if (/^https?:\/\//i.test(url)) contents.loadURL(url);
    return { action: 'deny' };
  });
});

app.on('window-all-closed', () => {
  stopAll();
  app.quit();
});
