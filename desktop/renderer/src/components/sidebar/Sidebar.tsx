import { useMemo, useState } from 'react';
import {
  Plus,
  Settings,
  FolderOpen,
  Folder,
  MessageSquare,
  PanelLeftClose,
  PanelLeftOpen,
  Sparkles,
  Trash2,
  Eraser,
  Loader2,
  ShieldAlert,
} from 'lucide-react';
import type { SessionHistoryEntry } from '../../types';
import { useSessionStore } from '../../store/session';
import { useUiStore } from '../../store/ui';
import { useChatStore } from '../../store/chat';
import { folderName, normalizeFolder, sameFolder } from '../../lib/paths';

interface FolderGroup {
  key: string;
  cwd: string;
  label: string;
  sessions: SessionHistoryEntry[];
}

// Terse relative time for the history rows, matching the reference look:
// 刚刚 / 5分钟 / 3小时 / 2天 / 9/28 (older than ~a month becomes a date).
function fmtRelative(ms: number): string {
  const diff = Date.now() - ms;
  if (diff < 60_000) return '刚刚';
  const h = Math.floor(diff / 3_600_000);
  if (h < 1) return `${Math.floor(diff / 60_000)}分钟`;
  if (h < 24) return `${h}小时`;
  const d = Math.floor(h / 24);
  if (d < 30) return `${d}天`;
  const dt = new Date(ms);
  return `${dt.getMonth() + 1}/${dt.getDate()}`;
}

// History is grouped by project folder (the cwd recorded in each session
// header). Sessions whose folder cannot be determined cannot be grouped
// and are dropped entirely.
function groupByFolder(entries: SessionHistoryEntry[], active: string): FolderGroup[] {
  const byKey = new Map<string, FolderGroup>();
  for (const e of entries) {
    if (!e.cwd) continue; // ungroupable → dropped
    const key = normalizeFolder(e.cwd);
    let g = byKey.get(key);
    if (!g) {
      g = { key, cwd: e.cwd, label: folderName(e.cwd), sessions: [] };
      byKey.set(key, g);
    }
    g.sessions.push(e);
  }
  const groups = [...byKey.values()];
  // Active project first, then alphabetically by folder name.
  groups.sort((a, b) => {
    const aa = sameFolder(a.cwd, active) ? 0 : 1;
    const bb = sameFolder(b.cwd, active) ? 0 : 1;
    if (aa !== bb) return aa - bb;
    return a.label.localeCompare(b.label, 'zh');
  });
  return groups;
}

export default function Sidebar() {
  const collapsed = useUiStore(s => s.sidebarCollapsed);
  const toggleSidebar = useUiStore(s => s.toggleSidebar);
  const setSettingsOpen = useUiStore(s => s.setSettingsOpen);

  const sessions = useSessionStore(s => s.sessions);
  const sessionId = useSessionStore(s => s.sessionId);
  const runtimes = useSessionStore(s => s.runtimes);
  const newSession = useSessionStore(s => s.newSession);
  const resumeSession = useSessionStore(s => s.resumeSession);
  const deleteSession = useSessionStore(s => s.deleteSession);
  const pruneEmpty = useSessionStore(s => s.pruneEmpty);
  const workspace = useSessionStore(s => s.workspace);

  const [collapsedFolders, setCollapsedFolders] = useState<Set<string>>(new Set());

  const groups = useMemo(() => groupByFolder(sessions, workspace), [sessions, workspace]);

  const toggleFolder = (key: string) => {
    setCollapsedFolders(prev => {
      const next = new Set(prev);
      if (next.has(key)) next.delete(key);
      else next.add(key);
      return next;
    });
  };

  const onDelete = (e: React.MouseEvent, id: string, cwd: string) => {
    e.stopPropagation();
    if (confirm(`删除会话「${id}」?此操作不可恢复。`)) deleteSession(id, cwd);
  };

  const onPrune = async () => {
    if (!confirm('清理当前工作区的空会话(从未发送消息的)?')) return;
    const n = await pruneEmpty();
    const key = useSessionStore.getState().sessionId ?? '';
    useChatStore.getState().addNote(key, n > 0 ? `已清理 ${n} 个空会话` : '没有需要清理的空会话');
  };

  return (
    <aside className={`sidebar${collapsed ? ' collapsed' : ''}`}>
      <div className="sidebar-head">
        <span className="brand" title="SCode">
          <span className="brand-logo">
            <Sparkles size={15} />
          </span>
          <span className="brand-name">SCode</span>
        </span>
        <button
          className="icon-btn"
          onClick={toggleSidebar}
          title={collapsed ? '展开侧栏' : '折叠侧栏'}
        >
          {collapsed ? <PanelLeftOpen size={16} /> : <PanelLeftClose size={16} />}
        </button>
      </div>

      <button className="new-chat-btn" onClick={newSession} title="新建会话">
        <Plus size={15} />
        <span>新建会话</span>
      </button>

      <div className="session-scroll">
        {groups.length === 0 && !collapsed && (
          <div className="session-empty dim">暂无历史会话</div>
        )}
        {!collapsed && groups.length > 0 && (
          <div className="session-toolbar">
            <span className="session-toolbar-label">项目</span>
            <button className="icon-btn prune-btn" onClick={onPrune} title="清理当前工作区的空会话">
              <Eraser size={12} />
            </button>
          </div>
        )}
        {groups.map(g => {
          const isCollapsed = collapsedFolders.has(g.key);
          const isActive = sameFolder(g.cwd, workspace);
          return (
            <div key={g.key} className="session-folder">
              {!collapsed && (
                <button
                  className={`session-folder-head${isActive ? ' active' : ''}`}
                  onClick={() => toggleFolder(g.key)}
                  title={g.cwd}
                >
                  {isCollapsed ? <Folder size={16} /> : <FolderOpen size={16} />}
                  <span className="folder-name">{g.label}</span>
                  <span className="folder-count">{g.sessions.length}</span>
                </button>
              )}
              {(!isCollapsed || collapsed) &&
                g.sessions.map(s => {
                  // Live state for sessions this client has open: a
                  // spinner while a run is in flight (the run keeps
                  // going in the background), a shield while the run
                  // waits on an approval answer.
                  const rt = runtimes[s.id];
                  const waiting = !rt?.running && (rt?.approvals.length ?? 0) > 0;
                  return (
                  <div
                    key={s.id}
                    className={`session-item${s.id === sessionId ? ' active' : ''}`}
                    onClick={() => s.id !== sessionId && resumeSession(s.id, s.cwd)}
                    onKeyDown={e => {
                      if (e.target !== e.currentTarget) return; // ignore the delete button
                      if (e.key !== 'Enter' && e.key !== ' ') return;
                      e.preventDefault();
                      if (s.id !== sessionId) resumeSession(s.id, s.cwd);
                    }}
                    tabIndex={0}
                    title={s.title ? `${s.title}\n${s.id}` : s.id}
                    role="button"
                  >
                    <span className="session-flag">
                      {rt?.running ? (
                        <Loader2 size={12} className="spin session-live" />
                      ) : collapsed ? (
                        <MessageSquare size={14} />
                      ) : null}
                    </span>
                    <span className="session-title">{s.title || s.id}</span>
                    {waiting && <ShieldAlert size={12} className="session-wait" />}
                    {!collapsed && s.mtime && (
                      <span className="session-time">{fmtRelative(s.mtime)}</span>
                    )}
                    {!collapsed && (
                      <button
                        className="icon-btn delete-btn"
                        onClick={e => onDelete(e, s.id, s.cwd)}
                        title="删除会话"
                      >
                        <Trash2 size={12} />
                      </button>
                    )}
                  </div>
                  );
                })}
            </div>
          );
        })}
      </div>

      <div className="sidebar-foot">
        <button className="foot-item" onClick={() => setSettingsOpen(true)} title="设置">
          <Settings size={16} />
          <span className="foot-label">设置</span>
        </button>
      </div>
    </aside>
  );
}
