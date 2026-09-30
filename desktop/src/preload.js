// Preload: the renderer's entire surface to the host.
const { contextBridge, ipcRenderer } = require('electron');

// Non-server channels share the server channel's {ok, result|error} envelope
// so failures rethrow a clean message rather than Electron's handler prefix.
async function unwrap(channel, ...args) {
  const r = await ipcRenderer.invoke(channel, ...args);
  if (r && r.ok === false) throw new Error(r.error);
  return r.result;
}

contextBridge.exposeInMainWorld('scode', {
  // The main process returns {ok, result|error} so renderer errors are
  // clean messages (no Electron handler prefix).
  rpc: async (method, params, hint) => {
    const r = await ipcRenderer.invoke('rpc', { method, params, hint });
    if (!r.ok) throw new Error(r.error);
    return r.result;
  },
  answerApproval: (id, result) => ipcRenderer.invoke('approval-answer', { id, result }),
  onEvent: cb => ipcRenderer.on('event', (_e, m) => cb(m)),
  onApproval: cb => ipcRenderer.on('approval', (_e, m) => cb(m)),
  onServerExit: cb => ipcRenderer.on('server-exit', (_e, code) => cb(code)),
  getWorkspace: () => ipcRenderer.invoke('get-workspace'),
  setWorkspace: dir => unwrap('set-workspace', dir),
  pickWorkspace: () => unwrap('pick-workspace'),
  listSessions: () => unwrap('list-sessions'),
  deleteSession: (id, cwd) => unwrap('delete-session', { id, cwd }),
  getDefaultSandbox: () => unwrap('get-default-sandbox'),
  getGitChanges: dir => unwrap('git-diff', dir),
  openExternal: url => unwrap('open-external', url),
  openPath: dir => unwrap('open-path', dir),
  openInVSCode: dir => unwrap('open-in-vscode', dir),
  // Main forwards external navigations it blocked (clicked links,
  // window.open) so the renderer opens them in the browser panel.
  onOpenUrl: cb => ipcRenderer.on('open-url', (_e, url) => cb(url)),
});
