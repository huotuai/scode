import { useEffect, useRef, useState } from 'react';
import { ChevronDown, FolderOpen } from 'lucide-react';
import { useSessionStore, useActiveRuntime } from '../../store/session';
import { sameFolder } from '../../lib/paths';
import { errText, openInVSCode, openPath } from '../../lib/rpc';
import { TaskButton } from '../tasks/TaskPanel';
import { PlanButton } from '../plan/PlanPanel';
import { ChangesButton } from '../changes/ChangesPanel';
import type { UsageReport, ContextBreakdown } from '../../types';
import { useT, fmtTokens } from '../../i18n';

// (the hover card's token numbers go through i18n.fmtTokens)

export default function TopBar() {
  const t = useT();
  const sessionId = useSessionStore(s => s.sessionId);
  const sessions = useSessionStore(s => s.sessions);
  const rt = useActiveRuntime();
  const running = rt.running;
  const usage = rt.usage;
  const serverExit = useSessionStore(s => s.serverExit);
  const workspace = useSessionStore(s => s.workspace);

  // Prefer the derived title; fall back to the raw id for untitled sessions.
  const title = sessionId ? sessions.find(e => e.id === sessionId)?.title : '';

  // The pool runs one server per workspace; only the session in view's
  // own server dying turns the dot red.
  const dead = serverExit !== null && sameFolder(serverExit.workspace, rt.cwd || workspace);
  const dotClass = dead ? 'dead' : running ? 'running' : 'ready';
  const dotTitle = dead
    ? t('topbar.serverExited', { code: serverExit!.code })
    : running
      ? t('topbar.running')
      : t('topbar.ready');

  return (
    <>
      {serverExit !== null && (
        <div className="exit-banner">
          {t('topbar.exitedNote', { workspace: serverExit.workspace, code: serverExit.code })}
        </div>
      )}
      <div className="topbar">
        <span className={`status-dot ${dotClass}`} title={dotTitle} />
        <span className="tb-title" title={sessionId || ''}>
          {title || sessionId || t('topbar.newSession')}
        </span>
        <span className="spacer" />
        {usage && <UsageStats usage={usage} />}
        <ChangesButton />
        <PlanButton />
        <TaskButton />
        {workspace && <WorkspaceOpen workspace={workspace} />}
      </div>
    </>
  );
}

/** VSCode's brand glyph (simple-icons path); lucide ships no brand icons. */
function VSCodeIcon({ size = 14 }: { size?: number }) {
  return (
    <svg width={size} height={size} viewBox="0 0 24 24" fill="currentColor" aria-hidden>
      <path d="M23.15 2.587 18.21.21a1.494 1.494 0 0 0-1.705.29l-9.46 8.63-4.12-3.128a.999.999 0 0 0-1.276.057L.327 7.261A1 1 0 0 0 .326 8.74L3.899 12 .326 15.26a1 1 0 0 0 .001 1.479L1.65 17.94a.999.999 0 0 0 1.276.057l4.12-3.128 9.46 8.63a1.492 1.492 0 0 0 1.704.29l4.942-2.377A1.5 1.5 0 0 0 24 20.06V3.939a1.5 1.5 0 0 0-.85-1.352zm-5.146 14.861L10.826 12l7.178-5.448v10.896z" />
    </svg>
  );
}

/** Workspace quick-open: the folder icon opens the directory directly;
 *  the caret drops a menu with alternative openers (VSCode). */
function WorkspaceOpen({ workspace }: { workspace: string }) {
  const t = useT();
  const [open, setOpen] = useState(false);
  const [error, setError] = useState('');
  const ref = useRef<HTMLDivElement>(null);

  useEffect(() => {
    if (!open) return;
    const onDown = (e: MouseEvent) => {
      if (ref.current && !ref.current.contains(e.target as Node)) setOpen(false);
    };
    document.addEventListener('mousedown', onDown);
    return () => document.removeEventListener('mousedown', onDown);
  }, [open]);

  // Success closes the menu; failure keeps it open so the error is visible.
  const act = (fn: () => Promise<unknown>) => {
    setError('');
    fn()
      .then(() => setOpen(false))
      .catch(err => {
        setError(errText(err));
        setOpen(true);
      });
  };

  return (
    <div className="ws-open" ref={ref}>
      <button
        className="icon-btn"
        title={t('topbar.openLocal', { ws: workspace })}
        onClick={() => act(() => openPath(workspace))}
      >
        <FolderOpen size={15} />
      </button>
      <button
        className={`icon-btn ws-caret${open ? ' active' : ''}`}
        title={t('topbar.moreOpen')}
        onClick={() => {
          setOpen(v => !v);
          setError('');
        }}
      >
        <ChevronDown size={13} />
      </button>
      {open && (
        <div className="ws-menu">
          <div className="ws-menu-path" title={workspace}>
            {workspace}
          </div>
          <button className="ws-option" onClick={() => act(() => openPath(workspace))}>
            <FolderOpen size={14} /> {t('topbar.openLocalEntry')}
          </button>
          <button className="ws-option" onClick={() => act(() => openInVSCode(workspace))}>
            <VSCodeIcon /> {t('topbar.openVscode')}
          </button>
          {error && <div className="ws-menu-error">{error}</div>}
        </div>
      )}
    </div>
  );
}

// Hover-card numbers go through i18n.fmtTokens (zh 万/亿, en K/M).


/** Ring gauge (the topbar context icon): light track + progress arc,
 *  colored by the same occupancy tiers as the old inline meter. */
function ContextRing({ pct, level }: { pct: number | null; level: string }) {
  const r = 6.5; // 16px box, 2.5 stroke width
  const c = 2 * Math.PI * r;
  // A tiny occupancy stays visible (2%), like the meter's min fill.
  const filled = pct === null ? 0 : Math.min(100, Math.max(2, pct));
  return (
    <svg className={`ctx-ring ${level}`} width={16} height={16} viewBox="0 0 16 16">
      <circle className="ctx-ring-track" cx={8} cy={8} r={r} fill="none" strokeWidth={2.5} />
      <circle
        className="ctx-ring-arc"
        cx={8}
        cy={8}
        r={r}
        fill="none"
        strokeWidth={2.5}
        strokeDasharray={`${(c * filled) / 100} ${c}`}
        strokeLinecap="round"
        transform="rotate(-90 8 8)"
      />
    </svg>
  );
}

/** Hover-card breakdown rows: colored dot + label + share of the used
 *  context. Categories are chars/4 estimates; "other" absorbs the gap
 *  so the rows sum to ~100%. */
function breakdownRows(t: ReturnType<typeof useT>): { key: keyof ContextBreakdown; label: string }[] {
  return [
    { key: 'messages', label: t('topbar.bdMessages') },
    { key: 'sysTools', label: t('topbar.bdSysTools') },
    { key: 'sysPrompt', label: t('topbar.bdSysPrompt') },
    { key: 'skills', label: t('topbar.bdSkills') },
    { key: 'mcpTools', label: t('topbar.bdMcpTools') },
    { key: 'other', label: t('topbar.bdOther') },
  ];
}

function BreakdownRows({ breakdown, total }: { breakdown?: ContextBreakdown; total: number }) {
  const t = useT();
  if (!breakdown) return null;
  return (
    <div className="ctx-rows">
      {breakdownRows(t).map(row => {
        const v = breakdown[row.key] ?? 0;
        const pct = total > 0 ? (v / total) * 100 : 0;
        return (
          <div className="ctx-row" key={row.key}>
            <span className="ctx-dot" />
            <span className="ctx-row-label">{row.label}</span>
            <span className="ctx-row-value">{pct.toFixed(1)}%</span>
          </div>
        );
      })}
    </div>
  );
}

// Usage stats: a ring-gauge icon whose hover opens the context card
// (capacity + category shares + average cache hit rate — the cache
// number moved here from its old topbar chip). The cost chip stays
// inline.
function UsageStats({ usage }: { usage: UsageReport }) {
  const t = useT();
  const [hover, setHover] = useState(false);
  const { contextTokens, contextWindow, input, cacheRead, costUSD } = usage;

  // Context occupancy against the model window (0 = unknown window).
  const rawPct = contextWindow > 0 ? (contextTokens / contextWindow) * 100 : null;
  const pct = rawPct === null ? null : Math.min(100, Math.round(rawPct));
  const pctLabel = rawPct !== null && rawPct > 0 && pct === 0 ? '<1%' : pct !== null ? `${pct}%` : null;
  const ctxLevel = pct === null ? '' : pct >= 85 ? 'high' : pct >= 60 ? 'mid' : '';

  // Cache hit rate: cache reads over total prompt-side reads (session
  // cumulative); lives in the hover card now, not a topbar chip.
  const reads = input + cacheRead;
  const hitRate = reads > 0 ? Math.round((cacheRead / reads) * 100) : null;

  return (
    <div className="usage-stats">
      <div
        className="ctx-widget"
        onMouseEnter={() => setHover(true)}
        onMouseLeave={() => setHover(false)}
      >
        <ContextRing pct={pct} level={ctxLevel} />
        {hover && (
          <div className="ctx-pop">
            <div className="ctx-card">
              <div className="ctx-head">
                <span className="ctx-title">{t('topbar.ctxTitle')}</span>
                <span className="ctx-total">
                  {contextWindow > 0
                    ? `${fmtTokens(contextTokens)}/${fmtTokens(contextWindow)}(${pctLabel})`
                    : t('topbar.ctxUnknown', { tokens: fmtTokens(contextTokens) })}
                </span>
              </div>
              <div className={`ctx-bar ${ctxLevel}`}>
                {pct !== null && (
                  <span className="ctx-bar-fill" style={{ width: `${Math.max(2, pct)}%` }} />
                )}
              </div>
              <BreakdownRows breakdown={usage.breakdown} total={contextTokens} />
              <div className="ctx-divider" />
              <div className="ctx-summary">
                <span>{t('topbar.cacheHit')}</span>
                <span className="ctx-summary-value">{hitRate === null ? '—' : `${hitRate}%`}</span>
              </div>
            </div>
          </div>
        )}
      </div>
      {costUSD > 0 && (
        <span
          className="usage-item"
          title={`in=${usage.input} out=${usage.output} cacheRead=${usage.cacheRead} cacheWrite=${usage.cacheWrite}`}
        >
          <span className="usage-value">${costUSD.toFixed(4)}</span>
        </span>
      )}
    </div>
  );
}
