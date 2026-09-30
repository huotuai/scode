import { create } from 'zustand';
import type { PlanResult, PlanState } from '../types';
import { rpc, errText } from '../lib/rpc';
import { useSessionStore } from './session';

// The plan-progress panel's state. The plan is the session's update_plan
// checklist, persisted server-side as plan entries (resume/fork replay
// it); the list is polled from the session's owning server, exactly like
// the background-task panel.
//
// Polling (not push) matches the tasks panel: the plan changes on
// human-scale events, and the RPC bridge is already generic, so a 2s
// poll costs one cheap IPC round trip and needs no new event plumbing.

const POLL_MS = 2000;

interface PlanStoreState {
  open: boolean;
  plan: PlanState | null;
  error: string | null;

  start(): void;
  stop(): void;
  setOpen(open: boolean): void;
  toggle(): void;
  refresh(): Promise<void>;
}

let timer: number | null = null;

export const usePlanStore = create<PlanStoreState>((set, get) => ({
  open: false,
  plan: null,
  error: null,

  start() {
    if (timer !== null) return;
    void get().refresh();
    timer = window.setInterval(() => void get().refresh(), POLL_MS);
  },

  stop() {
    if (timer !== null) {
      window.clearInterval(timer);
      timer = null;
    }
  },

  setOpen(open) {
    set({ open });
    if (open) void get().refresh();
  },

  toggle() {
    get().setOpen(!get().open);
  },

  async refresh() {
    const sessionId = useSessionStore.getState().sessionId;
    if (!sessionId) {
      // A draft has no server handle yet: no plan can exist under it.
      if (get().plan !== null) set({ plan: null });
      return;
    }
    try {
      const r = await rpc<PlanResult>('session/plan', { sessionId });
      set({ plan: r.plan ?? null, error: null });
    } catch (err) {
      set({ error: errText(err) });
    }
  },
}));

/** refreshPlan is the event hook: refresh on update_plan tool events so a
 *  just-updated step shows up without waiting for the next poll. */
export function refreshPlan() {
  void usePlanStore.getState().refresh();
}

/** planProgress counts done/total (the TopBar badge and panel header). */
export function planProgress(plan: PlanState | null): { done: number; total: number } {
  const items = plan?.items ?? [];
  return { done: items.filter(i => i.status === 'completed').length, total: items.length };
}
