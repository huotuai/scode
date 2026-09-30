import { memo } from 'react';
import { ChevronRight, History } from 'lucide-react';
import type { ChatItem } from '../../types';
import ChatItemView from './ChatItemView';
import { useT } from '../../i18n';

// A finished run's working steps (thinking, tool calls, interim text) fold
// into one collapsed row so the transcript reads as results, not process.
// Expanded on demand; a plain <details> keeps the toggle native and
// accessible. While a run is in flight MessageList renders its items
// unfolded, so this only ever wraps completed work.
const RunGroup = memo(function RunGroup({ items }: { items: ChatItem[] }) {
  const t = useT();
  const tools = items.filter(i => i.kind === 'tool').length;
  const errors = items.some(i => i.kind === 'tool' && i.state === 'error');
  const label = tools > 0 ? t('chat.runGroupTools', { n: tools }) : t('chat.runGroup');
  return (
    <details className={`run-group${errors ? ' has-error' : ''}`}>
      <summary>
        <History size={13} />
        <span>{label}</span>
        <ChevronRight size={13} className="run-caret" />
      </summary>
      <div className="run-body">
        {items.map(i => (
          <ChatItemView key={i.key} item={i} />
        ))}
      </div>
    </details>
  );
});

export default RunGroup;
