import { Bot } from 'lucide-react';
import Composer from '../composer/Composer';
import WorkspacePicker from './WorkspacePicker';
import { useT } from '../../i18n';
import '../../styles/new-session.css';

// Landing page for a fresh draft: the greeting, the working-directory
// picker and the composer stacked in one centered column — the input box
// itself is the regular Composer, unchanged.
export default function NewSessionPage() {
  const t = useT();
  return (
    <div className="new-session-page">
      <div className="new-session-hero">
        <div className="empty-logo">
          <Bot size={26} />
        </div>
        <div className="empty-title">{t('chat.emptyTitle')}</div>
        <div className="empty-sub">
          {t('chat.emptyDesc')}
        </div>
      </div>
      <div className="new-session-workspace">
        <WorkspacePicker />
      </div>
      <Composer />
    </div>
  );
}
