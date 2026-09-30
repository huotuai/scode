import { useState } from 'react';
import { ShieldAlert, ClipboardCheck, Check, Ban, Shield } from 'lucide-react';
import type { ApprovalRequest } from '../../types';
import { answerApproval } from '../../lib/rpc';
import { useSessionStore, useActiveRuntime, patchRuntime } from '../../store/session';
import '../../styles/approvals.css';

// Approval cards take the composer's place while a request is pending
// (App swaps the two). Each card answers exactly one server→client
// reverse request, then removes itself — the input comes back. Cards
// belong to a session: only the session in view shows its cards; a
// background session's pending approvals wait (the sidebar flags it)
// instead of being denied on switch.
export default function ApprovalHost() {
  const approvals = useActiveRuntime().approvals;
  if (approvals.length === 0) return null;
  return (
    <div className="approval-host">
      {approvals.map(a =>
        a.params.kind === 'plan' ? (
          <PlanApproval key={a.id} req={a} />
        ) : a.params.kind === 'sandbox' ? (
          <SandboxApproval key={a.id} req={a} />
        ) : (
          <PermissionApproval key={a.id} req={a} />
        ),
      )}
    </div>
  );
}

function PlanApproval({ req }: { req: ApprovalRequest }) {
  const dropApproval = useSessionStore(s => s.dropApproval);
  const [feedback, setFeedback] = useState('');

  const answer = (decision: 'approve' | 'revise') => {
    answerApproval(req.id, decision === 'approve' ? { decision } : { decision, feedback });
    dropApproval(req.id);
  };

  return (
    <div className="approval-card">
      <div className="approval-title">
        <span className="approval-icon">
          <ClipboardCheck size={15} />
        </span>
        计划评审
      </div>
      <pre className="approval-plan">{req.params.plan}</pre>
      <div className="approval-row">
        <button className="btn primary" onClick={() => answer('approve')}>
          <Check size={13} />
          批准并执行
        </button>
        <input
          value={feedback}
          onChange={e => setFeedback(e.target.value)}
          placeholder="驳回并附反馈(直接回车=无理由打回)"
          onKeyDown={e => {
            if (e.key === 'Enter') answer('revise');
          }}
        />
        <button className="btn danger-ghost" onClick={() => answer('revise')}>
          <Ban size={13} />
          驳回
        </button>
      </div>
    </div>
  );
}

// Sandbox escalation: the model asks to widen this call's file scope.
// 批准一次 = this call only; 本会话生效 = switch the session's standing mode.
function SandboxApproval({ req }: { req: ApprovalRequest }) {
  const dropApproval = useSessionStore(s => s.dropApproval);
  const { tool, detail, currentMode, requestedMode, justification } = req.params;

  const answer = (decision: 'allow' | 'deny' | 'allow_session') => {
    answerApproval(req.id, { decision });
    dropApproval(req.id);
    // 'allow_session' widens the session's standing mode on the server to
    // exactly the requested mode (cli SetSandboxMode); mirror it so the
    // picker reflects it at once. 'allow' (this call only) and 'deny' leave
    // the standing mode untouched.
    if (decision === 'allow_session' && req.params.sessionId && requestedMode) {
      patchRuntime(req.params.sessionId, { sandbox: requestedMode });
    }
  };

  return (
    <div className="approval-card">
      <div className="approval-title">
        <span className="approval-icon">
          <Shield size={15} />
        </span>
        沙箱升权:{currentMode} → {requestedMode}
      </div>
      <div className="approval-detail">{tool}  {detail}</div>
      {justification && <div className="approval-justification">理由:{justification}</div>}
      <div className="approval-row">
        <button className="btn primary" onClick={() => answer('allow')}>
          批准一次
        </button>
        <button className="btn" onClick={() => answer('allow_session')}>
          本会话生效
        </button>
        <button className="btn danger-ghost" onClick={() => answer('deny')}>
          拒绝
        </button>
      </div>
    </div>
  );
}

function PermissionApproval({ req }: { req: ApprovalRequest }) {
  const dropApproval = useSessionStore(s => s.dropApproval);
  const { rule, tool, argKind, argValue } = req.params;
  const detail = `${tool ?? ''}  ${argKind ? argKind + ': ' : ''}${argValue ?? ''}`;

  const answer = (decision: 'allow' | 'deny' | 'allow_session' | 'allow_project') => {
    answerApproval(req.id, { decision });
    dropApproval(req.id);
  };

  return (
    <div className="approval-card">
      <div className="approval-title">
        <span className="approval-icon">
          <ShieldAlert size={15} />
        </span>
        审批:{rule}
      </div>
      <div className="approval-detail">{detail}</div>
      <div className="approval-row">
        <button className="btn primary" onClick={() => answer('allow')}>
          允许一次
        </button>
        <button className="btn danger-ghost" onClick={() => answer('deny')}>
          拒绝
        </button>
        <button className="btn" onClick={() => answer('allow_session')}>
          本会话总是
        </button>
        <button className="btn" onClick={() => answer('allow_project')}>
          本项目总是
        </button>
      </div>
    </div>
  );
}
