import { create } from 'zustand';
import type { BackgroundTask, TaskListResult } from '../types';
import { rpc, errText } from '../lib/rpc';
import { useSessionStore } from './session';

// The background-task panel's state. Tasks are detached bash commands
// (timeout or run_in_background); the list is polled from the session's
// owning server, and killing one goes straight to session/task-kill.
//
// Polling (not push) is deliberate: the registry changes on human-scale
// events, and the RPC bridge is already generic, so a 2s poll costs one
// cheap IPC round trip and needs no new event plumbing.

const POLL_MS = 2000;

interface TasksState {
  open: boolean;
  tasks: BackgroundTask[];
  error: string | null;

  start(): void;
  stop(): void;
  setOpen(open: boolean): void;
  toggle(): void;
  refresh(): Promise<void>;
  kill(id: number): Promise<void>;
}

let timer: number | null = null;

export const useTasksStore = create<TasksState>((set, get) => ({
  open: false,
  tasks: [],
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
      // A draft has no server handle yet: nothing can be running under it.
      if (get().tasks.length > 0) set({ tasks: [] });
      return;
    }
    try {
      const r = await rpc<TaskListResult>('session/tasks', { sessionId });
      // Newest first: the panel reads top-down.
      const tasks = (r.tasks || []).slice().reverse();
      set({ tasks, error: null });
    } catch (err) {
      set({ error: errText(err) });
    }
  },

  async kill(id) {
    const sessionId = useSessionStore.getState().sessionId;
    if (!sessionId) return;
    try {
      await rpc('session/task-kill', { sessionId, taskId: id });
      set({ error: null });
    } catch (err) {
      set({ error: errText(err) });
    }
    await get().refresh();
  },
}));

/** refreshTasks is the event hook: refresh on bash-related tool events so a
 *  just-detached task shows up without waiting for the next poll. */
export function refreshTasks() {
  void useTasksStore.getState().refresh();
}

/** runningTaskCount counts live tasks (the TopBar badge). */
export function runningTaskCount(tasks: BackgroundTask[]): number {
  return tasks.reduce((n, t) => (t.state === 'running' ? n + 1 : n), 0);
}
