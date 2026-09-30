import { create } from 'zustand';
import { t } from '../i18n';
import type {
  ApprovalRequest,
  CompactResult,
  ServerExitInfo,
  SessionHistoryEntry,
  SessionInfo,
  TranscriptResult,
  UsageReport,
} from '../types';
import {
  rpc,
  errText,
  answerApproval,
  getWorkspace,
  setWorkspace,
  pickWorkspace,
  listSessions,
  deleteSession as deleteSessionFile,
  getDefaultSandbox,
} from '../lib/rpc';
import { sameFolder } from '../lib/paths';
import { useChatStore, DRAFT_KEY } from './chat';
import { useWorkspacesStore } from './workspaces';

function fmtTok(n: number): string {
  if (n >= 1_000_000) return (n / 1_000_000).toFixed(1).replace(/\.0$/, '') + 'M';
  if (n >= 1_000) return (n / 1_000).toFixed(1).replace(/\.0$/, '') + 'k';
  return String(n);
}

// Runtime is one open session's live state. Every session the renderer
// has opened — including background ones with a run in flight — keeps an
// entry, so switching sessions only moves `sessionId`; the previous
// session's run, stream, approvals and usage keep accumulating in its own
// runtime (and on its own server, per workspace). The unsent draft lives
// under DRAFT_KEY ('').
export interface Runtime {
  cwd: string; // owning workspace folder
  mode: 'default' | 'plan';
  sandbox: string;
  running: boolean;
  compacting: boolean;
  usage: UsageReport | null;
  provider: string;
  model: string;
  /** 推理强度:'' | off | low | medium | high('' = 跟随设置默认) */
  thinking: string;
  approvals: ApprovalRequest[];
}

function defaultRuntime(sandbox: string, cwd: string): Runtime {
  return {
    cwd,
    mode: 'default',
    sandbox,
    running: false,
    compacting: false,
    usage: null,
    provider: '',
    model: '',
    thinking: '',
    approvals: [],
  };
}

interface SessionState {
  /** The session in view; null = unsent draft. */
  sessionId: string | null;
  runtimes: Record<string, Runtime>;
  sessions: SessionHistoryEntry[];
  workspace: string;
  defaultSandbox: string;
  serverExit: ServerExitInfo | null;
  booting: boolean;

  boot(): Promise<void>;
  /** Switch the view to a fresh draft. The previous session keeps
      running untouched on its server. */
  newSession(): void;
  /** The current session id, creating one on demand (first prompt,
      model switch, mode toggle). Drafts never touch the disk. */
  ensureSession(): Promise<string>;
  resumeSession(id: string, cwd?: string): Promise<void>;
  /** Fork (clone) a session into a new one and switch to it. tailTurns
   *  anchors the clone at "the k-th user turn from the end, reply
   *  included"; 0 forks the whole session. cwd routes the RPC to the
   *  owning workspace's server (sidebar history spans folders). */
  forkSession(source: { id: string; cwd?: string; tailTurns?: number }): Promise<void>;
  deleteSession(id: string, cwd?: string): Promise<void>;
  pruneEmpty(): Promise<number>;
  refreshSessions(): Promise<void>;
  refreshUsage(id?: string | null): Promise<void>;
  send(text: string, images?: string[] /* data URLs */): Promise<boolean>;
  cancel(): void;
  toggleMode(): Promise<void>;
  setSandbox(mode: string): Promise<void>;
  compact(): Promise<void>;
  chooseWorkspace(): Promise<void>;
  /** Switch the active workspace to an already-known folder (recent list). */
  selectWorkspace(dir: string): Promise<void>;
  setRunning(id: string, running: boolean): void;
  /** Mirror a server-side mode switch (e.g. the plan approval that exits
      plan mode mid-run) so the composer chip reflects it immediately. */
  setMode(id: string, mode: 'default' | 'plan'): void;
  setServerExit(info: ServerExitInfo): void;
  pushApproval(req: ApprovalRequest): void;
  dropApproval(id: number): void;
}

const FALLBACK_RUNTIME = defaultRuntime('workspace-write', '');

// useActiveRuntime selects the runtime of the session in view (draft
// included), for components that render per-session state.
export function useActiveRuntime(): Runtime {
  return useSessionStore(
    s => s.runtimes[s.sessionId ?? DRAFT_KEY] ?? s.runtimes[DRAFT_KEY] ?? FALLBACK_RUNTIME,
  );
}

// patchRuntime merges into one session's runtime from outside the store
// (models store). id null → the draft.
export function patchRuntime(id: string | null, p: Partial<Runtime>) {
  useSessionStore.setState(s => {
    const k = id ?? DRAFT_KEY;
    const cur = s.runtimes[k] ?? defaultRuntime(s.defaultSandbox, s.workspace);
    return { runtimes: { ...s.runtimes, [k]: { ...cur, ...p } } };
  });
}

export const useSessionStore = create<SessionState>((set, get) => {
  function patch(id: string | null, p: Partial<Runtime>) {
    patchRuntime(id, p);
  }

  const chat = () => useChatStore.getState();
  const chatKey = (id: string | null) => id ?? DRAFT_KEY;
  /** Error/note target: the session currently in view. */
  const activeKey = () => chatKey(get().sessionId);

  return {
    sessionId: null,
    runtimes: { [DRAFT_KEY]: defaultRuntime('workspace-write', '') },
    sessions: [],
    workspace: '',
    defaultSandbox: 'workspace-write',
    serverExit: null,
    booting: true,

    async boot() {
      try {
        // The sandbox picker shows the deployment default on a draft; a real
        // session reports its own resolved mode on create.
        const [workspace, defaultSandbox] = await Promise.all([
          getWorkspace(),
          getDefaultSandbox(),
        ]);
        useWorkspacesStore.getState().remember(workspace);
        set(s => ({
          workspace,
          defaultSandbox,
          runtimes: {
            ...s.runtimes,
            [DRAFT_KEY]: defaultRuntime(defaultSandbox, workspace),
          },
        }));
        // No session is created here: booting into a draft keeps the
        // history list free of empty sessions.
        await get().refreshSessions();
      } catch (err) {
        chat().addError(activeKey(), t('store.startFail', { err: errText(err) }));
      } finally {
        set({ booting: false });
      }
    },

    newSession() {
      // Only the view changes: a run in flight on the previous session
      // keeps going on its server, and its events keep landing in its
      // own runtime and chat log (the sidebar marks it as running).
      chat().reset(DRAFT_KEY);
      set(s => ({
        sessionId: null,
        runtimes: {
          ...s.runtimes,
          [DRAFT_KEY]: defaultRuntime(s.defaultSandbox, s.workspace),
        },
      }));
    },

    async ensureSession() {
      const existing = get().sessionId;
      if (existing) return existing;
      const r = await rpc<SessionInfo>('session/create', {});
      patch(r.sessionId, {
        cwd: get().workspace,
        mode: r.mode,
        sandbox: r.sandbox || 'danger-full-access',
        provider: r.provider || '',
        model: r.model || '',
        thinking: r.thinking || '',
      });
      // The draft's chat log (notes/errors from pre-session actions)
      // belongs to the real session now.
      chat().migrate(DRAFT_KEY, r.sessionId);
      set({ sessionId: r.sessionId, serverExit: null });
      get().refreshSessions();
      get().refreshUsage(r.sessionId);
      return r.sessionId;
    },

    async resumeSession(id, cwd) {
      const cur = get();
      if (cur.sessionId === id) return;
      const live = cur.runtimes[id];
      if (live) {
        // Already open on a pooled server: switching is free, and a run
        // in flight simply becomes the view (it never stopped).
        set({ sessionId: id, workspace: live.cwd || cur.workspace });
        return;
      }
      try {
        // History spans every project: a session from another folder is
        // served by that folder's server (spawned on demand in the main
        // process; the current folder's server stays alive).
        if (cwd && !sameFolder(cwd, get().workspace)) {
          const ws = await setWorkspace(cwd);
          useWorkspacesStore.getState().remember(ws);
          set({ workspace: ws });
        }
        const r = await rpc<SessionInfo>('session/resume', { sessionId: id }, cwd);
        patch(id, {
          cwd: cwd || get().workspace,
          mode: r.mode,
          sandbox: r.sandbox || 'danger-full-access',
          provider: r.provider || '',
          model: r.model || '',
          thinking: r.thinking || '',
          // A live handle reports its in-flight run (re-attach after a
          // renderer reload): keep the spinner and steer composer.
          running: r.running === true,
          compacting: false,
          usage: null,
          approvals: [],
        });
        set({ sessionId: id, serverExit: null });
        const tr = await rpc<TranscriptResult>('session/transcript', { sessionId: id });
        chat().loadTranscript(id, tr.messages);
        await get().refreshUsage(id);
      } catch (err) {
        chat().addError(activeKey(), t('store.resumeFail', { err: errText(err) }));
      }
    },

    async forkSession({ id, cwd, tailTurns }) {
      try {
        const r = await rpc<{ sessionId: string }>(
          'session/fork',
          { sessionId: id, tailTurns: tailTurns ?? 0 },
          cwd,
        );
        if (!r.sessionId || r.sessionId === id) return;
        // The clone belongs to the same workspace; switching to it loads
        // its transcript like any history resume.
        await get().resumeSession(r.sessionId, cwd || get().workspace);
        get().refreshSessions();
        chat().addNote(
          r.sessionId,
          tailTurns ? t('store.forkedTurn', { id: id.slice(0, 8) }) : t('store.forked', { id: id.slice(0, 8) }),
        );
      } catch (err) {
        chat().addError(get().sessionId ?? DRAFT_KEY, t('store.forkFail', { err: errText(err) }));
      }
    },

    async refreshSessions() {
      try {
        // Read straight from disk so history covers every project folder,
        // not just the cwd the server is bound to.
        const r = await listSessions();
        set({ sessions: r.sessions || [] }); // newest activity first
      } catch {
        /* list is best-effort */
      }
    },

    async deleteSession(id, cwd) {
      try {
        // Deny this session's unanswered approvals first so nothing
        // hangs if the delete itself fails.
        const rt = get().runtimes[id];
        if (rt) for (const a of rt.approvals) answerApproval(a.id, { decision: 'deny' });
        // The main process routes live sessions to their owning server
        // (which cancels any run) and unlinks orphaned files directly.
        await deleteSessionFile(id, cwd || get().workspace);
        set(s => {
          const runtimes = { ...s.runtimes };
          delete runtimes[id];
          return { runtimes };
        });
        chat().drop(id);
        if (get().sessionId === id) get().newSession(); // back to draft
        await get().refreshSessions();
      } catch (err) {
        chat().addError(activeKey(), t('store.deleteFail', { err: errText(err) }));
      }
    },

    async pruneEmpty() {
      try {
        const r = await rpc<{ deleted: number }>('session/prune', {});
        await get().refreshSessions();
        return r.deleted;
      } catch (err) {
        chat().addError(activeKey(), t('store.pruneFail', { err: errText(err) }));
        return 0;
      }
    },

    async refreshUsage(id) {
      const target = id === undefined ? get().sessionId : id;
      if (!target) return;
      try {
        const r = await rpc<UsageReport>('session/usage', { sessionId: target });
        patch(target, { usage: r });
      } catch {
        /* session gone */
      }
    },

    async send(text, images) {
      text = text.trim();
      const dataUrls = (images ?? []).filter(Boolean);
      if (!text && dataUrls.length === 0) return false;
      const cur = get().sessionId;
      const rt = get().runtimes[chatKey(cur)];
      if (rt?.compacting) return false; // wait for the compaction to finish
      if (rt?.running) {
        if (!cur) return false;
        // Steering is text-only (agent.Steer): mid-run attachments
        // would be silently dropped, so refuse and let the composer
        // restore the draft.
        if (dataUrls.length > 0) {
          chat().addNote(cur, t('store.imageWhileRunning'));
          return false;
        }
        chat().addSteer(cur, text);
        try {
          await rpc('session/steer', { sessionId: cur, text });
        } catch (err) {
          chat().addError(cur, errText(err));
        }
        // Already echoed as a steer bubble; don't restore it.
        return true;
      }
      let sessionId: string;
      try {
        sessionId = await get().ensureSession();
      } catch (err) {
        // Setup failures (e.g. defaultProvider naming a missing profile)
        // surface here instead of at boot. The text was NOT echoed, so the
        // composer restores it for a retry.
        chat().addError(activeKey(), t('store.createFail', { err: errText(err) }));
        return false;
      }
      chat().addUser(sessionId, text, dataUrls);
      patch(sessionId, { running: true });
      try {
        // The server wants base64 payloads; the composer holds data URLs.
        const b64 = dataUrls.map(u => u.slice(u.indexOf(',') + 1));
        await rpc('session/prompt', {
          sessionId,
          text,
          ...(b64.length > 0 ? { images: b64 } : {}),
        });
        return true;
      } catch (err) {
        chat().addError(sessionId, errText(err));
        patch(sessionId, { running: false });
        return false;
      }
    },

    cancel() {
      const { sessionId } = get();
      if (sessionId) rpc('session/cancel', { sessionId }).catch(() => {});
    },

    async toggleMode() {
      let sessionId: string;
      try {
        sessionId = await get().ensureSession();
      } catch (err) {
        chat().addError(activeKey(), errText(err));
        return;
      }
      const next = (get().runtimes[sessionId]?.mode ?? 'default') === 'plan' ? 'default' : 'plan';
      try {
        const r = await rpc<{ mode: 'default' | 'plan' }>('session/mode', {
          sessionId,
          mode: next,
        });
        patch(sessionId, { mode: r.mode });
        chat().addNote(
          sessionId,
          r.mode === 'plan' ? t('store.planOn') : t('store.planOff'),
        );
      } catch (err) {
        chat().addError(sessionId, errText(err));
      }
    },

    async setSandbox(mode) {
      let sessionId: string;
      try {
        sessionId = await get().ensureSession();
      } catch (err) {
        chat().addError(activeKey(), errText(err));
        return;
      }
      try {
        const r = await rpc<{ sandbox: string }>('session/sandbox', { sessionId, mode });
        patch(sessionId, { sandbox: r.sandbox });
        chat().addNote(
          sessionId,
          r.sandbox === 'danger-full-access'
            ? t('store.sandboxOff')
            : r.sandbox === 'workspace-write'
              ? t('store.sandboxWorkspace')
              : t('store.sandboxReadOnly'),
        );
      } catch (err) {
        chat().addError(sessionId, errText(err));
      }
    },

    async compact() {
      const { sessionId } = get();
      if (!sessionId || get().runtimes[sessionId]?.compacting) return;
      patch(sessionId, { compacting: true });
      try {
        const r = await rpc<CompactResult>('session/compact', { sessionId });
        if (r.ok === false) {
          chat().addNote(sessionId, t('store.nothingToCompact'));
        } else {
          const detail =
            r.tokensBefore !== undefined && r.tokensAfter !== undefined
              ? `(${fmtTok(r.tokensBefore)} → ${fmtTok(r.tokensAfter)} tokens)`
              : '';
          chat().addNote(sessionId, t('store.compacted', { detail }));
        }
      } catch (err) {
        chat().addNote(sessionId, t('store.compactFail', { err: errText(err) }));
      } finally {
        patch(sessionId, { compacting: false });
      }
    },

    async chooseWorkspace() {
      try {
        const picked = await pickWorkspace();
        // null = dialog canceled; same folder = selection unchanged. Neither
        // should discard the current draft/conversation.
        if (!picked || sameFolder(picked, get().workspace)) return;
        useWorkspacesStore.getState().remember(picked);
        // The old folder's sessions keep running on their own server;
        // the view moves to a fresh draft in the new folder.
        set({ workspace: picked, serverExit: null });
        get().newSession();
        await get().refreshSessions();
      } catch (err) {
        chat().addError(activeKey(), errText(err));
      }
    },

    async selectWorkspace(dir) {
      if (!dir || sameFolder(dir, get().workspace)) return;
      try {
        // setWorkspace validates the folder and spawns its server alongside
        // the current one; a folder deleted since it was remembered fails
        // here and is dropped from the recent list.
        const ws = await setWorkspace(dir);
        useWorkspacesStore.getState().remember(ws);
        set({ workspace: ws, serverExit: null });
        get().newSession();
        await get().refreshSessions();
      } catch (err) {
        useWorkspacesStore.getState().forget(dir);
        chat().addError(activeKey(), errText(err));
      }
    },

    setRunning: (id, running) => patch(id, { running }),
    setMode: (id, mode) => patch(id, { mode }),

    setServerExit(info) {
      set(s => {
        const runtimes = { ...s.runtimes };
        // The dead server took its runs and pending approvals with it.
        for (const [k, rt] of Object.entries(runtimes)) {
          if (!k || !sameFolder(rt.cwd, info.workspace)) continue;
          if (rt.running || rt.compacting || rt.approvals.length > 0) {
            runtimes[k] = { ...rt, running: false, compacting: false, approvals: [] };
          }
        }
        return { serverExit: info, runtimes };
      });
    },

    pushApproval(req) {
      const sid = req.params.sessionId || DRAFT_KEY;
      set(s => {
        const cur = s.runtimes[sid] ?? defaultRuntime(s.defaultSandbox, s.workspace);
        // An approval implies a run is in flight on that session.
        return {
          runtimes: {
            ...s.runtimes,
            [sid]: { ...cur, running: true, approvals: [...cur.approvals, req] },
          },
        };
      });
    },

    dropApproval(id) {
      set(s => {
        for (const [k, rt] of Object.entries(s.runtimes)) {
          if (rt.approvals.some(a => a.id === id)) {
            return {
              runtimes: {
                ...s.runtimes,
                [k]: { ...rt, approvals: rt.approvals.filter(a => a.id !== id) },
              },
            };
          }
        }
        return {};
      });
    },
  };
});
