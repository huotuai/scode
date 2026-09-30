import { create } from 'zustand';
import { normalizeFolder } from '../lib/paths';

// ---------------------------------------------------------------------------
// recent working directories
//
// Every folder the user has worked in is remembered here (localStorage, so
// it survives restarts) and offered on the new-session page, next to the
// "pick a new folder" escape hatch. Most-recent first, de-duplicated by
// normalized path (separator style / case), keeping the newest spelling.
// ---------------------------------------------------------------------------

const KEY = 'scode.recentDirs';
const MAX_DIRS = 12;

function load(): string[] {
  try {
    const v = JSON.parse(localStorage.getItem(KEY) || '[]');
    return Array.isArray(v) ? v.filter(d => typeof d === 'string' && d) : [];
  } catch {
    return [];
  }
}

function save(dirs: string[]) {
  try {
    localStorage.setItem(KEY, JSON.stringify(dirs));
  } catch {
    /* storage full/blocked: memory-only is fine */
  }
}

interface WorkspacesState {
  recent: string[];
  /** Move dir to the front (or insert it), capping the list. */
  remember(dir: string): void;
  /** Drop dir from the list (stale entry removed by the user). */
  forget(dir: string): void;
}

export const useWorkspacesStore = create<WorkspacesState>(set => ({
  recent: load(),

  remember(dir) {
    if (!dir) return;
    set(s => {
      const key = normalizeFolder(dir);
      const recent = [dir, ...s.recent.filter(d => normalizeFolder(d) !== key)].slice(
        0,
        MAX_DIRS,
      );
      save(recent);
      return { recent };
    });
  },

  forget(dir) {
    set(s => {
      const key = normalizeFolder(dir);
      const recent = s.recent.filter(d => normalizeFolder(d) !== key);
      save(recent);
      return { recent };
    });
  },
}));
