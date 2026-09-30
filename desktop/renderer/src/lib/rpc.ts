import type {
  ApprovalDecision,
  ApprovalRequest,
  ServerExitInfo,
  SessionEvent,
} from '../types';

// Typed, promise-based wrapper over the preload bridge (window.scode).
// All methods go through the same structured {ok, result|error} IPC.

export function rpc<T = unknown>(method: string, params?: unknown, hint?: string): Promise<T> {
  return window.scode.rpc<T>(method, params, hint);
}

export function answerApproval(id: number, result: ApprovalDecision): void {
  window.scode.answerApproval(id, result);
}

// fanout installs the underlying IPC listener exactly once and hands each
// renderer subscriber its own unsubscribe. This keeps the listener count
// at one no matter how often a component mounts (React StrictMode in dev
// runs effects twice, Vite HMR remounts) — stacking listeners is what made
// every streamed delta arrive twice, so text appeared duplicated.
function fanout<T>(register: (dispatch: (value: T) => void) => void) {
  let subscribers: ((value: T) => void)[] | null = null;
  return (cb: (value: T) => void): (() => void) => {
    if (!subscribers) {
      subscribers = [];
      register(value => {
        for (const sub of subscribers!.slice()) sub(value);
      });
    }
    subscribers.push(cb);
    return () => {
      subscribers = (subscribers || []).filter(s => s !== cb);
    };
  };
}

const subscribeEvent = fanout<SessionEvent>(dispatch => window.scode.onEvent(dispatch));
const subscribeApproval = fanout<ApprovalRequest>(dispatch => window.scode.onApproval(dispatch));
const subscribeServerExit = fanout<ServerExitInfo>(dispatch => window.scode.onServerExit(dispatch));
const subscribeOpenUrl = fanout<string>(dispatch => window.scode.onOpenUrl(dispatch));

export function onEvent(cb: (ev: SessionEvent) => void): () => void {
  return subscribeEvent(cb);
}

export function onApproval(cb: (req: ApprovalRequest) => void): () => void {
  return subscribeApproval(cb);
}

export function onServerExit(cb: (info: ServerExitInfo) => void): () => void {
  return subscribeServerExit(cb);
}

// Main blocked an external navigation (clicked link / window.open) and
// forwarded the URL: open it in the browser panel, never in the chat UI.
export function onOpenUrl(cb: (url: string) => void): () => void {
  return subscribeOpenUrl(cb);
}

export const getWorkspace = () => window.scode.getWorkspace();
export const setWorkspace = (dir: string) => window.scode.setWorkspace(dir);
export const pickWorkspace = () => window.scode.pickWorkspace();
export const listSessions = () => window.scode.listSessions();
export const deleteSession = (id: string, cwd: string) => window.scode.deleteSession(id, cwd);
export const getDefaultSandbox = () => window.scode.getDefaultSandbox();
export const getGitChanges = (dir: string) => window.scode.getGitChanges(dir);
export const openExternal = (url: string) => window.scode.openExternal(url);
export const openPath = (dir: string) => window.scode.openPath(dir);
export const openInVSCode = (dir: string) => window.scode.openInVSCode(dir);

export function errText(err: unknown): string {
  if (err instanceof Error) return err.message;
  return String(err);
}
