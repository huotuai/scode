import { create } from 'zustand';
import type { GitChangesResult } from '../types';
import { errText, getGitChanges } from '../lib/rpc';
import { useSessionStore } from './session';

// The changes panel's state: the workspace's uncommitted git changes.
// Unlike the tasks/plan panels (which poll the agent's server), this data
// comes from git itself via the main process, so polling only runs while
// the panel is open; edit/write tool events also trigger a refresh so the
// badge catches up after the agent touches files.

const POLL_MS = 3000;

interface ChangesState {
  open: boolean;
  loading: boolean;
  result: GitChangesResult | null;
  error: string | null;
  /** The workspace is not a git repository (a state, not a failure). */
  notRepo: boolean;

  setOpen(open: boolean): void;
  toggle(): void;
  refresh(): Promise<void>;
}

let timer: number | null = null;

function startPolling(refresh: () => Promise<void>) {
  if (timer !== null) return;
  timer = window.setInterval(() => void refresh(), POLL_MS);
}

function stopPolling() {
  if (timer !== null) {
    window.clearInterval(timer);
    timer = null;
  }
}

export const useChangesStore = create<ChangesState>((set, get) => ({
  open: false,
  loading: false,
  result: null,
  error: null,
  notRepo: false,

  setOpen(open) {
    set({ open });
    if (open) {
      void get().refresh();
      startPolling(get().refresh);
    } else {
      stopPolling();
    }
  },

  toggle() {
    get().setOpen(!get().open);
  },

  async refresh() {
    const workspace = useSessionStore.getState().workspace;
    if (!workspace) return;
    // Keep the previous result visible while refetching: the list never
    // flickers empty between polls.
    if (!get().result) set({ loading: true });
    try {
      const result = await getGitChanges(workspace);
      set({ result, error: null, notRepo: false, loading: false });
    } catch (err) {
      const msg = errText(err);
      // A non-repo workspace is expected (any folder can be opened): show
      // a quiet empty state, keep any stale result for the badge. The
      // main process throws stable English markers.
      const notRepo = msg.includes('not a git repository') || msg.includes('git command not found');
      set({
        error: notRepo ? null : msg,
        notRepo,
        loading: false,
        result: notRepo ? { files: [], diff: '', truncated: false } : get().result,
      });
    }
  },
}));

/** refreshChanges is the event hook: a completed edit/write tool call
 *  almost always changed the working tree, so refresh the panel/badge. */
export function refreshChanges() {
  void useChangesStore.getState().refresh();
}
