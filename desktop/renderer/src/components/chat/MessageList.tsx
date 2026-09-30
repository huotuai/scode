import { memo, useEffect, useRef } from 'react';
import { Bot, Loader2 } from 'lucide-react';
import type { ChatItem } from '../../types';
import { useChatStore } from '../../store/chat';
import { useSessionStore, useActiveRuntime } from '../../store/session';
import ChatItemView from './ChatItemView';
import RunGroup from './RunGroup';
import RowActions from './RowActions';
import { useT } from '../../i18n';
import '../../styles/chat.css';

// groupItems folds each finished run's process (thinking, tool calls,
// interim assistant text) into one collapsed RunGroup, leaving the final
// assistant answer — or the terminal error — as the visible result. The
// in-flight run and top-level rows (user/steer/note) render as-is.
// Each finished reply carries the turn's hover rail (copy / fork / time)
// under it. forkTurn maps a reply to its turn anchor: user/steer rows
// counted from the reply to the end, PLUS its own turn (whose user
// message precedes the reply) — the server counts disk user messages
// tail-first the same way, so compaction trimming the front of the view
// never mismatches the anchor.
function groupItems(
  items: ChatItem[],
  running: boolean,
  forkTurn: ((tailTurns: number) => void) | null,
): React.ReactNode[] {
  // userTurnsFromEnd[i] = user/steer rows in items[i..end].
  const turnsFromEnd: number[] = new Array(items.length).fill(0);
  let acc = 0;
  for (let i = items.length - 1; i >= 0; i--) {
    if (items[i].kind === 'user' || items[i].kind === 'steer') acc++;
    turnsFromEnd[i] = acc;
  }
  const nodes: React.ReactNode[] = [];
  let i = 0;
  while (i < items.length) {
    const head = items[i];
    if (head.kind === 'user' || head.kind === 'steer' || head.kind === 'note') {
      nodes.push(<ChatItemView key={head.key} item={head} />);
      i++;
      continue;
    }
    // A run segment spans everything up to the next top-level row.
    const start = i;
    while (i < items.length) {
      const k = items[i].kind;
      if (k === 'user' || k === 'steer' || k === 'note') break;
      i++;
    }
    const seg = items.slice(start, i);
    if (running && i >= items.length) {
      // Live run: keep every step visible (thinking streams, tools spin).
      for (const it of seg) nodes.push(<ChatItemView key={it.key} item={it} />);
      continue;
    }
    let process = seg;
    let tail: ChatItem | null = null;
    const last = seg[seg.length - 1];
    if (last && (last.kind === 'assistant' || last.kind === 'error')) {
      tail = last;
      process = seg.slice(0, -1);
    }
    if (process.length) nodes.push(<RunGroup key={process[0].key} items={process} />);
    if (tail && tail.kind === 'assistant') {
      const replyIdx = i - 1; // tail's index in items
      nodes.push(
        <ChatItemView
          key={tail.key}
          item={tail}
          actions={
            <RowActions
              text={tail.text}
              ts={tail.ts}
              onFork={forkTurn ? () => forkTurn(turnsFromEnd[replyIdx] + 1) : undefined}
            />
          }
        />,
      );
    } else if (tail) {
      nodes.push(<ChatItemView key={tail.key} item={tail} />);
    }
  }
  return nodes;
}

const EMPTY_ITEMS: ChatItem[] = [];

export default function MessageList() {
  const t = useT();
  const sessionId = useSessionStore(s => s.sessionId);
  // The session in view selects its own log; background sessions keep
  // accumulating theirs off-screen.
  const items = useChatStore(s => s.bySession[sessionId ?? ''] ?? EMPTY_ITEMS);
  const booting = useSessionStore(s => s.booting);
  const running = useActiveRuntime().running;
  const compacting = useActiveRuntime().compacting;
  const forkSession = useSessionStore(s => s.forkSession);
  const scrollRef = useRef<HTMLDivElement>(null);
  // Follow the stream only while the user is pinned to the bottom;
  // scrolling up pauses auto-scroll (standard chat UX).
  const pinnedRef = useRef(true);

  // Forking mid-run would anchor against a disk state that lags the
  // live view (persistence settles at message boundaries); drafts have
  // no session to fork.
  const forkTurn =
    sessionId && !running
      ? (tailTurns: number) => void forkSession({ id: sessionId, tailTurns })
      : null;

  // Opening a different conversation always starts pinned to the bottom,
  // even if the previous one was scrolled up. Declared before the items
  // effect so it resets the flag first when both change in one commit.
  useEffect(() => {
    pinnedRef.current = true;
  }, [sessionId]);

  useEffect(() => {
    const el = scrollRef.current;
    if (el && pinnedRef.current) el.scrollTop = el.scrollHeight;
  }, [items, running, compacting]);

  const onScroll = () => {
    const el = scrollRef.current;
    if (!el) return;
    pinnedRef.current = el.scrollHeight - el.scrollTop - el.clientHeight < 48;
  };

  return (
    <div className="chat-scroll" ref={scrollRef} onScroll={onScroll}>
      <div className="chat-column">
        {items.length === 0 && !booting && <EmptyState />}
        {groupItems(items, running, forkTurn)}
        {running && (
          <div className="run-indicator">
            <Loader2 size={13} className="spin" />
          </div>
        )}
        {compacting && (
          <div className="run-indicator">
            <Loader2 size={13} className="spin" />
            <span>{t('chat.compacting')}</span>
          </div>
        )}
      </div>
    </div>
  );
}

const EmptyState = memo(function EmptyState() {
  const t = useT();
  return (
    <div className="empty-state">
      <div className="empty-logo">
        <Bot size={26} />
      </div>
      <div className="empty-title">{t('chat.emptyTitle')}</div>
      <div className="empty-sub">
        {t('chat.emptyDesc')}
      </div>
    </div>
  );
});
