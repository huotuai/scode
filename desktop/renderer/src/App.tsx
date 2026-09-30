import { useEffect } from 'react';
import { onEvent, onApproval, onServerExit, onOpenUrl } from './lib/rpc';
import { useSessionStore, useActiveRuntime } from './store/session';
import { useChatStore } from './store/chat';
import { useModelsStore } from './store/models';
import { useUiStore } from './store/ui';
import Sidebar from './components/sidebar/Sidebar';
import TopBar from './components/layout/TopBar';
import MessageList from './components/chat/MessageList';
import NewSessionPage from './components/chat/NewSessionPage';
import Composer from './components/composer/Composer';
import ApprovalHost from './components/approvals/ApprovalHost';
import SettingsDialog from './components/settings/SettingsDialog';
import TaskPanel from './components/tasks/TaskPanel';
import PlanPanel from './components/plan/PlanPanel';
import ChangesPanel from './components/changes/ChangesPanel';
import BrowserPanel from './components/browser/BrowserPanel';
import { useTasksStore, refreshTasks } from './store/tasks';
import { usePlanStore, refreshPlan } from './store/plan';
import { refreshChanges } from './store/changes';
import { DRAFT_KEY } from './store/chat';
import './styles/app.css';

export default function App() {
  const boot = useSessionStore(s => s.boot);
  // A fresh draft with nothing in its log gets the landing page (greeting
  // + workspace picker + composer). Once the draft holds an error/note or
  // a real session is in view, the regular chat layout takes over.
  const landing = useSessionStore(s => s.sessionId === null);
  const draftEmpty = useChatStore(s => (s.bySession[DRAFT_KEY]?.length ?? 0) === 0);
  const showLanding = landing && draftEmpty;

  useEffect(() => {
    // Single subscription point for the server event stream. Every
    // session's events land in that session's own chat log and runtime
    // (routed by sessionId), so background runs keep making progress
    // while another session is in view.
    // The unsubscribe handles are returned so a dev/HMR remount does not
    // stack listeners and deliver every event two or more times (which
    // showed up as duplicated streamed text).
    const offEvent = onEvent(ev => {
      const s = useSessionStore.getState();
      useChatStore.getState().handleEvent(ev);
      if (ev.type === 'agent_end') {
        s.setRunning(ev.sessionId, false);
        s.refreshUsage(ev.sessionId);
        // The first turn persists the session's first message, which is
        // what makes it eligible for the history list; refresh so it and
        // its updated ordering show up without another action.
        s.refreshSessions();
      } else if (ev.type === 'agent_error' || ev.type === 'run_error') {
        s.setRunning(ev.sessionId, false);
      }
      // Approving a plan exits plan mode on the server mid-run
      // (exit_plan_mode is the only backend-initiated mode flip): a
      // successful tool_end means the chip should clear without waiting
      // for the next state poll. Rejections/validation errors are
      // isError, so plan mode correctly stays on.
      if (ev.type === 'tool_end' && ev.call?.name === 'exit_plan_mode' && !ev.result?.isError) {
        s.setMode(ev.sessionId, 'default');
      }
      // A session becomes history-eligible when its first message lands
      // on disk — turn_end/user events are forwarded only after that
      // write is durable. Refresh once (only while the session is still
      // absent from the list) so a new session shows up in the sidebar
      // while its first run is in flight, not just after agent_end.
      if (
        (ev.type === 'turn_end' || ev.type === 'user' || ev.type === 'agent_error') &&
        !s.sessions.some(x => x.id === ev.sessionId)
      ) {
        s.refreshSessions();
      }
      // A bash call that detached carries a fresh task id: refresh so the
      // panel and its badge reflect it without waiting for the next poll.
      if (ev.type === 'tool_end' && ['bash', 'bash_status', 'bash_kill'].includes(ev.call?.name ?? '')) {
        refreshTasks();
      }
      // update_plan commits a new checklist server-side: refresh so the
      // panel and its badge reflect it without waiting for the next poll.
      if (ev.type === 'tool_end' && ev.call?.name === 'update_plan' && !ev.result?.isError) {
        refreshPlan();
      }
      // A finished edit/write almost always moved the working tree:
      // refresh the changes panel and its TopBar badge.
      if (ev.type === 'tool_end' && ['edit', 'write'].includes(ev.call?.name ?? '')) {
        refreshChanges();
      }
    });
    const offApproval = onApproval(req => useSessionStore.getState().pushApproval(req));
    const offServerExit = onServerExit(info => useSessionStore.getState().setServerExit(info));
    // Main blocked an external navigation: open it in the browser panel.
    const offOpenUrl = onOpenUrl(url => useUiStore.getState().openBrowser(url));

    useModelsStore.getState().refresh();
    boot();
    useTasksStore.getState().start();
    usePlanStore.getState().start();
    return () => {
      useTasksStore.getState().stop();
      usePlanStore.getState().stop();
      offEvent();
      offApproval();
      offServerExit();
      offOpenUrl();
    };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  // The changes panel tracks the workspace: switching folders (boot,
  // picker, history resume) refetches the badge count.
  const workspace = useSessionStore(s => s.workspace);
  useEffect(() => {
    if (workspace) refreshChanges();
  }, [workspace]);

  // A pending approval REPLACES the composer: the confirmation card takes
  // the input's spot; answering it brings the input back.
  const pendingApprovals = useActiveRuntime().approvals.length > 0;

  return (
    <div className="app-shell">
      <Sidebar />
      <div className="app-main">
        <TopBar />
        <TaskPanel />
        <PlanPanel />
        {showLanding ? <NewSessionPage /> : <MessageList />}
        {!showLanding && (pendingApprovals ? <ApprovalHost /> : <Composer />)}
      </div>
      {/* Docked right pane (like the browser panel), not a floating card:
          opening changes must not cover the chat. */}
      <ChangesPanel />
      <SettingsDialog />
      <BrowserPanel />
    </div>
  );
}
