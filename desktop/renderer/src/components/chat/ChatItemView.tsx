import { memo, useEffect, useMemo, useRef, useState, isValidElement, type ReactNode } from 'react';
import ReactMarkdown, { type Components } from 'react-markdown';
import remarkGfm from 'remark-gfm';
import { AlertCircle, CornerUpRight, Brain } from 'lucide-react';
import type { ChatItem } from '../../types';
import ToolCard from './ToolCard';
import { useUiStore } from '../../store/ui';
import { openExternal } from '../../lib/rpc';
import { highlightLines, langFromFence, type Token } from '../../lib/highlight';

// Clicking a link must never navigate the chat UI away: http(s) links
// open in the right-side browser panel, anything else (mailto:, custom
// schemes) goes to the OS.
function openLink(href: string) {
  if (/^https?:\/\//i.test(href)) {
    useUiStore.getState().openBrowser(href);
  } else {
    openExternal(href).catch(() => {});
  }
}

// codeInfo pulls the raw source string and fence language out of a fenced
// block's <pre><code>…</code></pre> children. Returns null for anything
// richer (the renderer emits plain strings here — highlighting is ours).
function codeInfo(children: ReactNode): { text: string; lang: string } | null {
  if (!isValidElement(children)) return null;
  const props = children.props as { children?: unknown; className?: string };
  const c = props.children;
  const text =
    typeof c === 'string'
      ? c
      : Array.isArray(c) && c.every(x => typeof x === 'string')
        ? c.join('')
        : null;
  if (text === null) return null;
  const m = /language-([\w#+-]+)/.exec(props.className ?? '');
  return { text, lang: m ? langFromFence(m[1]) : '' };
}

function renderTokens(tokens: Token[] | undefined, fallback: string): ReactNode {
  if (!tokens || tokens.length === 0) return fallback || ' ';
  return tokens.map((tok, i) =>
    tok.t ? (
      <span key={i} className={`tok-${tok.t}`}>
        {tok.s}
      </span>
    ) : (
      tok.s
    ),
  );
}

// MdPre renders fenced code blocks: syntax highlighting via the shared
// table-driven highlighter, and a line-number gutter when the appearance
// setting is on (设置 → 外观 → 显示行号).
function MdPre({ children }: { children?: ReactNode }) {
  const lineNums = useUiStore(s => s.codeLineNumbers);
  const info = codeInfo(children);
  const text = info?.text;
  const lang = info?.lang;
  // Tokenization reruns only when the block's source/language changes,
  // not on every streaming repaint of the surrounding message.
  const { lines, tokens } = useMemo(() => {
    if (text === undefined) return { lines: [], tokens: null };
    const ls = text.replace(/\n$/, '').split('\n');
    return { lines: ls, tokens: lang ? highlightLines(ls, lang) : null };
  }, [text, lang]);
  if (!info) return <pre>{children}</pre>;
  return (
    <pre className="md-pre-code">
      <code>
        {lines.map((l, i) => (
          <div key={i} className="md-ln">
            {lineNums && <span className="md-ln-no">{i + 1}</span>}
            <span className="md-ln-code">{renderTokens(tokens?.[i], l)}</span>
          </div>
        ))}
      </code>
    </pre>
  );
}

const mdComponents: Components = {
  pre: MdPre,
  a: ({ href, children }) => (
    <a
      href={href}
      onClick={e => {
        e.preventDefault();
        if (href) openLink(href);
      }}
    >
      {children}
    </a>
  ),
};

// One chat item. Memoized: streaming updates replace only the affected
// item object, so untouched siblings skip re-render. `actions` is the
// hover rail (copy / fork, RowActions) anchored UNDER the row — it is
// absolutely positioned, so the bubble itself keeps its exact original
// flex layout.
const ChatItemView = memo(function ChatItemView({
  item,
  actions,
}: {
  item: ChatItem;
  actions?: ReactNode;
}) {
  switch (item.kind) {
    case 'user':
      return (
        <div className="msg-row user turn-start">
          <div className="msg user">
            {item.images && item.images.length > 0 && (
              <div className="msg-images">
                {item.images.map((src, i) => (
                  <img key={i} src={src} alt="图片" />
                ))}
              </div>
            )}
            {item.text && <div>{item.text}</div>}
          </div>
          {actions && <div className="msg-actions">{actions}</div>}
        </div>
      );
    case 'steer':
      return (
        <div className="msg-row user">
          <div className="msg steer">
            <CornerUpRight size={13} />
            <span>{item.text}</span>
          </div>
          {actions && <div className="msg-actions">{actions}</div>}
        </div>
      );
    case 'assistant':
      return (
        <div className="msg-row assistant">
          <div className="msg assistant">
            <div className="md">
              <ReactMarkdown remarkPlugins={[remarkGfm]} components={mdComponents}>
                {item.text}
              </ReactMarkdown>
            </div>
            {item.streaming && <span className="caret" />}
          </div>
          {actions && <div className="msg-actions">{actions}</div>}
        </div>
      );
    case 'thinking':
      // Rendered as a direct child of the chat column (like tool cards) so
      // it spans the full width and left-aligns with them.
      return <ThinkingBlock item={item} />;
    case 'tool':
      return <ToolCard item={item} />;
    case 'error':
      return (
        <div className="msg error">
          <AlertCircle size={14} />
          <span>{item.text}</span>
        </div>
      );
    case 'note':
      return <div className="msg note">{item.text}</div>;
  }
});

export default ChatItemView;

// ThinkingBlock keeps its own open state so it auto-expands while the model
// is thinking but stays open afterwards — a plain `open={item.streaming}`
// collapsed the block under the reader the moment the stream ended.
function ThinkingBlock({ item }: { item: Extract<ChatItem, { kind: 'thinking' }> }) {
  const [open, setOpen] = useState(item.streaming);
  const bodyRef = useRef<HTMLDivElement>(null);
  useEffect(() => {
    if (item.streaming) setOpen(true);
  }, [item.streaming]);
  // Follow the stream inside the body's own scrollbox: thinking text is
  // capped at max-height, so without this the latest lines accumulate
  // below the fold while the block appears static.
  useEffect(() => {
    const el = bodyRef.current;
    if (el && item.streaming) el.scrollTop = el.scrollHeight;
  }, [item.text, item.streaming]);
  return (
    <details
      className="thinking"
      open={open}
      onToggle={e => setOpen((e.currentTarget as HTMLDetailsElement).open)}
    >
      <summary>
        <Brain size={13} />
        <span>{item.streaming ? '正在思考…' : '思考过程'}</span>
      </summary>
      <div className="thinking-body" ref={bodyRef}>{item.text}</div>
    </details>
  );
}
