import { useState } from 'react';
import { Check, Copy, GitFork } from 'lucide-react';

/** One chat row's hover rail. Lives under assistant replies (the end of
 *  a turn): copy + fork-from-here buttons and the reply's completion
 *  time. See .msg-actions in chat.css. */
export default function RowActions({
  text,
  ts,
  onFork,
}: {
  text: string;
  ts?: number;
  onFork?: () => void;
}) {
  const [copied, setCopied] = useState(false);

  const copy = async () => {
    try {
      await navigator.clipboard.writeText(text);
    } catch {
      // execCommand fallback for contexts without the async clipboard
      const ta = document.createElement('textarea');
      ta.value = text;
      document.body.appendChild(ta);
      ta.select();
      document.execCommand('copy');
      ta.remove();
    }
    setCopied(true);
    setTimeout(() => setCopied(false), 1200);
  };

  return (
    <>
      <button className="icon-btn row-action" onClick={copy} title="复制文本">
        {copied ? <Check size={14} /> : <Copy size={14} />}
      </button>
      {onFork && (
        <button className="icon-btn row-action" onClick={onFork} title="从此轮分叉出新会话">
          <GitFork size={14} />
        </button>
      )}
      {ts !== undefined && <span className="row-time">{fmtTime(ts)}</span>}
    </>
  );
}

/** HH:MM, zero-padded, 24h. */
function fmtTime(ts: number): string {
  const d = new Date(ts);
  const p = (n: number) => String(n).padStart(2, '0');
  return `${p(d.getHours())}:${p(d.getMinutes())}`;
}
