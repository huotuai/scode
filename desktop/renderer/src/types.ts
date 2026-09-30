// Wire types for the `scode serve` JSON-RPC protocol. These mirror the
// shapes produced by internal/server (mapEvent, handlers).

export interface InitializeResult {
  protocolVersion: number;
  server: string;
  capabilities: string[];
}

export interface SessionInfo {
  sessionId: string;
  mode: 'default' | 'plan';
  provider: string;
  model: string;
  thinking?: string; // '' | off | low | medium | high ('' = provider default)
  sandbox?: string; // read-only | workspace-write | danger-full-access
  pending?: boolean;
  // Set when the resume returned an already-live handle (stateOf): the
  // session has a run in flight we are re-attaching to.
  running?: boolean;
}

// One history entry: a session id plus the project folder (cwd) recorded
// in its header. cwd === '' means the header was unreadable, so the
// session cannot be grouped and the sidebar drops it. title is derived
// from the first user message ('' when there is none).
export interface SessionHistoryEntry {
  id: string;
  cwd: string;
  mtime?: number;
  title: string;
}

export interface SessionListResult {
  sessions: SessionHistoryEntry[];
}

export interface CostResult {
  text: string;
}

// UsageReport mirrors cli.UsageReport (session/usage).
export interface UsageReport {
  contextTokens: number;
  contextWindow: number; // 0 = unknown
  input: number;
  output: number;
  cacheRead: number;
  cacheWrite: number;
  costUSD: number;
  /** 按类别的上下文占用(chars/4 估算,"other" 兜底吸收差额,各行总和
   * 约等于 contextTokens);驱动悬停卡片的占比行。 */
  breakdown?: ContextBreakdown;
}

/** 上下文占用分类:消息/系统工具/系统提示词/技能/MCP 工具/其他。 */
export interface ContextBreakdown {
  messages: number;
  sysTools: number;
  sysPrompt: number;
  skills: number;
  mcpTools: number;
  other: number;
}

export interface CompactResult {
  ok: boolean;
  reason?: string;
  tokensBefore?: number;
  tokensAfter?: number;
}

export interface ModelInfo {
  label: string;
  provider: string;
  model: string;
  /** thinking-capable (provider 配置的 reasoning 标记):gates the
   * reasoning-effort selector; absent = unknown, assume capable. */
  reasoning?: boolean;
}

export interface Pricing {
  input?: number;
  output?: number;
  cacheRead?: number;
  cacheWrite?: number;
}

export interface ModelProfile {
  name: string;
  protocol?: string;
  model?: string;
  baseUrl?: string;
  apiKey?: string;
  contextWindow?: number;
  maxTokens?: number;
  imageInput?: boolean;
  reasoning?: boolean;
  pricing?: Pricing;
}

export interface ModelsListResult {
  models: ModelInfo[];
  profiles: ModelProfile[];
  default: { provider: string; model: string };
  /** settings 级别的默认推理强度('' = provider 默认) */
  thinking?: string;
}

export interface ModelSaveParams extends ModelProfile {
  original?: string;
}

// ---------------------------------------------------------------------------
// transcript (session/transcript)
// ---------------------------------------------------------------------------

export type ContentBlock =
  | { kind: 'text'; text: string }
  | { kind: 'thinking'; text: string }
  | { kind: 'image'; mimeType?: string; data?: string } // base64 payload
  | { kind: 'toolCall'; id: string; name: string; arguments: unknown }
  | { kind: 'toolResult'; id: string; content: { text?: string }[]; isError?: boolean };

export interface TranscriptMessage {
  // RoleTool is "toolResult" on the wire (see internal/llm/message.go).
  role: 'user' | 'assistant' | 'toolResult';
  content: ContentBlock[];
  /** unix milliseconds (llm.Message.TS); drives the per-reply time. */
  ts?: number;
}

export interface TranscriptResult {
  messages: TranscriptMessage[];
}

// ---------------------------------------------------------------------------
// session/event notifications
// ---------------------------------------------------------------------------

export interface SessionEvent {
  sessionId: string;
  type:
    | 'llm'
    | 'tool_start'
    | 'tool_end'
    | 'user'
    | 'turn_start'
    | 'turn_end'
    | 'assistant'
    | 'agent_start'
    | 'agent_end'
    | 'agent_error'
    | 'run_error';
  llm?: {
    type:
      | 'text_start'
      | 'text_delta'
      | 'text_end'
      | 'thinking_start'
      | 'thinking_delta'
      | 'thinking_end';
    delta?: string;
    reason?: string;
  };
  call?: { id: string; name: string; arguments: unknown };
  result?: { text: string; isError?: boolean };
  error?: string;
}

// ---------------------------------------------------------------------------
// plan progress (session/plan)
// ---------------------------------------------------------------------------

// PlanItem mirrors session.PlanItem: one step of the agent's working plan
// (the update_plan tool's whole-list replace, persisted as plan entries).
export interface PlanItem {
  step: string;
  status: 'pending' | 'in_progress' | 'completed';
}

// PlanState mirrors session.PlanEntry (whole-value replace, last wins).
export interface PlanState {
  explanation?: string;
  items: PlanItem[];
}

export interface PlanResult {
  plan: PlanState | null;
}

// ---------------------------------------------------------------------------
// background tasks (session/tasks, session/task-kill)
// ---------------------------------------------------------------------------

// BackgroundTask mirrors tools.TaskInfo: a bash command detached by its
// timeout or by run_in_background, kept observable and killable.
export interface BackgroundTask {
  id: number;
  sessionId: string;
  command: string;
  pid: number;
  state: 'running' | 'exited' | 'killed';
  exitCode: number;
  startedAt: number; // unix ms
  durationMs: number;
  output: string; // bounded tail
}

export interface TaskListResult {
  tasks: BackgroundTask[] | null;
}

// ---------------------------------------------------------------------------
// approvals (server→client reverse requests)
// ---------------------------------------------------------------------------

export interface ApprovalRequest {
  id: number;
  params: {
    sessionId?: string; // owning session, stamped by the server
    kind?: 'plan' | 'sandbox';
    plan?: string;
    rule?: string;
    tool?: string;
    argKind?: string;
    argValue?: string;
    // sandbox escalation (kind === 'sandbox')
    detail?: string;
    currentMode?: string;
    requestedMode?: string;
    justification?: string;
  };
}

export type ApprovalDecision =
  | { decision: 'approve' }
  | { decision: 'revise'; feedback: string }
  | { decision: 'allow' | 'deny' | 'allow_session' | 'allow_project' };

// ---------------------------------------------------------------------------
// chat presentation model
// ---------------------------------------------------------------------------

export type ChatItem =
  | { key: string; kind: 'user'; text: string; images?: string[] /* data URLs */ }
  | { key: string; kind: 'steer'; text: string }
  | { key: string; kind: 'assistant'; text: string; streaming: boolean; ts?: number }
  | { key: string; kind: 'thinking'; text: string; streaming: boolean }
  | { key: string; kind: 'error'; text: string }
  | { key: string; kind: 'note'; text: string }
  | {
      key: string;
      kind: 'tool';
      callId?: string;
      name: string;
      args: unknown;
      state: 'running' | 'ok' | 'error';
      result?: string;
    };

// server-exit notification (one per workspace server in the pool)
export interface ServerExitInfo {
  workspace: string;
  code: number;
}

// ---------------------------------------------------------------------------
// git changes (git-diff IPC: the changes panel)
// ---------------------------------------------------------------------------

// GitChangedFile is one `git status --porcelain` entry: path plus a coarse
// display status. untracked files never appear in `git diff HEAD`, so the
// panel lists them without a diff body.
export interface GitChangedFile {
  path: string;
  status: 'M' | 'A' | 'D' | 'R' | '??';
  untracked: boolean;
}

export interface GitChangesResult {
  files: GitChangedFile[];
  diff: string; // combined unified diff (staged + unstaged vs HEAD)
  truncated: boolean; // diff text hit the main-process size cap
}

// ---------------------------------------------------------------------------
// preload bridge
// ---------------------------------------------------------------------------

export interface ScodeBridge {
  // hint: the workspace a call belongs to when it cannot be derived from
  // params.sessionId (e.g. session/resume of another folder's session).
  rpc<T = unknown>(method: string, params?: unknown, hint?: string): Promise<T>;
  answerApproval(id: number, result: ApprovalDecision): void;
  onEvent(cb: (ev: SessionEvent) => void): void;
  onApproval(cb: (req: ApprovalRequest) => void): void;
  onServerExit(cb: (info: ServerExitInfo) => void): void;
  getWorkspace(): Promise<string>;
  setWorkspace(dir: string): Promise<string>;
  pickWorkspace(): Promise<string | null>;
  listSessions(): Promise<SessionListResult>;
  deleteSession(id: string, cwd: string): Promise<{ ok: boolean }>;
  getDefaultSandbox(): Promise<string>;
  getGitChanges(dir: string): Promise<GitChangesResult>;
  openExternal(url: string): Promise<{ ok: boolean }>;
  openPath(dir: string): Promise<{ ok: boolean }>;
  openInVSCode(dir: string): Promise<{ ok: boolean }>;
  onOpenUrl(cb: (url: string) => void): void;
}

declare global {
  interface Window {
    scode: ScodeBridge;
  }
}
