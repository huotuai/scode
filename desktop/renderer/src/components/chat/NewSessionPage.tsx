import { Bot } from 'lucide-react';
import Composer from '../composer/Composer';
import WorkspacePicker from './WorkspacePicker';
import '../../styles/new-session.css';

// Landing page for a fresh draft: the greeting, the working-directory
// picker and the composer stacked in one centered column — the input box
// itself is the regular Composer, unchanged.
export default function NewSessionPage() {
  return (
    <div className="new-session-page">
      <div className="new-session-hero">
        <div className="empty-logo">
          <Bot size={26} />
        </div>
        <div className="empty-title">开始一个新任务</div>
        <div className="empty-sub">
          输入任务描述,SCode 将调用工具完成它。试试开启 Plan 模式先做只读调研。
        </div>
      </div>
      <div className="new-session-workspace">
        <WorkspacePicker />
      </div>
      <Composer />
    </div>
  );
}
