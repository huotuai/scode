// Bilingual (en/zh) display strings for the renderer.
//
// Resolution: localStorage "scode-lang" override ('zh' | 'en') >
// navigator.language (zh-* → zh) > 'en'. Components use useT() (reactive
// — re-renders on a language switch); stores and callbacks use t()
// (reads the language at call time).
//
// Placeholders are named: t('sidebar.minutes', { n: 5 }) on "{n}分钟".
// Backend/main-process errors stay stable English — the renderer
// localizes what it generates, not what it receives.

import { create } from 'zustand';

export type Lang = 'en' | 'zh';

type Vars = Record<string, string | number>;

const LANG_KEY = 'scode-lang';

function detect(): Lang {
  try {
    const override = localStorage.getItem(LANG_KEY);
    if (override === 'en' || override === 'zh') return override;
  } catch {
    /* private mode */
  }
  return typeof navigator !== 'undefined' && navigator.language.toLowerCase().startsWith('zh') ? 'zh' : 'en';
}

interface I18nState {
  lang: Lang;
  /** setLang persists an override; 'system' removes it (auto-detect). */
  setLang: (l: Lang | 'system') => void;
}

export const useI18n = create<I18nState>((set) => ({
  lang: detect(),
  setLang: (l) => {
    try {
      if (l === 'system') localStorage.removeItem(LANG_KEY);
      else localStorage.setItem(LANG_KEY, l);
    } catch {
      /* private mode */
    }
    set({ lang: l === 'system' ? detect() : l });
  },
}));

// ---------------------------------------------------------------------------
// catalog
// ---------------------------------------------------------------------------

const catalog: Record<string, { en: string; zh: string }> = {
  // ---- shared ----
  'common.refresh': { en: 'Refresh', zh: '刷新' },
  'common.close': { en: 'Close', zh: '关闭' },
  'common.delete': { en: 'Delete', zh: '删除' },
  'common.edit': { en: 'Edit', zh: '编辑' },
  'common.save': { en: 'Save', zh: '保存' },
  'common.clear': { en: 'Clear', zh: '清空' },
  'common.new': { en: 'New', zh: '新建' },
  'common.thinking.default': { en: 'default', zh: '默认' },
  'common.thinking.off': { en: 'off', zh: '关闭' },
  'common.thinking.low': { en: 'low', zh: '低' },
  'common.thinking.medium': { en: 'medium', zh: '中' },
  'common.thinking.high': { en: 'high', zh: '高' },

  // ---- approvals ----
  'approval.planTitle': { en: 'Plan review', zh: '计划评审' },
  'approval.approveRun': { en: 'Approve & run', zh: '批准并执行' },
  'approval.rejectPlaceholder': { en: 'Reject with feedback (plain Enter = send back without a note)', zh: '驳回并附反馈(直接回车=无理由打回)' },
  'approval.reject': { en: 'Reject', zh: '驳回' },
  'approval.sandboxTitle': { en: 'Sandbox escalation: {current} → {requested}', zh: '沙箱升权:{current} → {requested}' },
  'approval.reason': { en: 'Reason: {text}', zh: '理由:{text}' },
  'approval.approveOnce': { en: 'Approve once', zh: '批准一次' },
  'approval.applySession': { en: 'Apply to session', zh: '本会话生效' },
  'approval.deny': { en: 'Deny', zh: '拒绝' },
  'approval.toolTitle': { en: 'Approval: {rule}', zh: '审批:{rule}' },
  'approval.allowOnce': { en: 'Allow once', zh: '允许一次' },
  'approval.allowSession': { en: 'Always for session', zh: '本会话总是' },
  'approval.allowProject': { en: 'Always for project', zh: '本项目总是' },

  // ---- browser panel ----
  'browser.back': { en: 'Back', zh: '后退' },
  'browser.forward': { en: 'Forward', zh: '前进' },
  'browser.address': { en: 'Address', zh: '地址' },
  'browser.openExternal': { en: 'Open in system browser', zh: '在系统浏览器中打开' },

  // ---- changes panel ----
  'changes.statusM': { en: 'Modified', zh: '修改' },
  'changes.statusA': { en: 'Added', zh: '新增' },
  'changes.statusD': { en: 'Deleted', zh: '删除' },
  'changes.statusR': { en: 'Renamed', zh: '重命名' },
  'changes.statusUntracked': { en: 'Untracked', zh: '未跟踪' },
  'changes.untrackedNote': { en: 'New untracked file, not in git diff', zh: '未跟踪的新文件,不在 git diff 中' },
  'changes.noDiff': { en: 'No textual diff (possibly a binary file)', zh: '无文本差异(可能为二进制文件)' },
  'changes.title': { en: 'Code changes', zh: '代码更改' },
  'changes.fileCount': { en: '{n} files', zh: '{n} 个文件' },
  'changes.truncated': { en: 'The diff is large; only the first part is shown', zh: '差异内容过大,仅显示前一部分' },
  'changes.notRepo': { en: 'The workspace is not a git repository; no changes to show.', zh: '当前工作区不是 git 仓库,无法显示更改。' },
  'changes.clean': { en: 'The workspace has no uncommitted changes.', zh: '工作区没有未提交的更改。' },
  'changes.buttonStats': { en: 'Code changes: {files} files (+{add} −{del})', zh: '代码更改:{files} 个文件(+{add} −{del})' },
  'changes.button': { en: 'Code changes (git diff)', zh: '代码更改(git diff)' },
  'changes.label': { en: 'Changes', zh: '更改' },

  // ---- chat items ----
  'chat.imageAlt': { en: 'image', zh: '图片' },
  'chat.thinking': { en: 'Thinking', zh: '思考过程' },
  'chat.thinkingStreaming': { en: 'Thinking…', zh: '正在思考…' },
  'chat.compacting': { en: 'Compacting context…', zh: '正在压缩上下文…' },
  'chat.emptyTitle': { en: 'Start a new task', zh: '开始一个新任务' },
  'chat.emptyDesc': { en: 'Describe the task and SCode will drive its tools to complete it. Try plan mode for read-only research first.', zh: '输入任务描述,SCode 将调用工具完成它。试试开启 Plan 模式先做只读调研。' },
  'chat.copyText': { en: 'Copy text', zh: '复制文本' },
  'chat.forkHere': { en: 'Fork a new session from this turn', zh: '从此轮分叉出新会话' },
  'chat.runGroupTools': { en: 'Run · {n} tool calls', zh: '运行过程 · {n} 个工具调用' },
  'chat.runGroup': { en: 'Run', zh: '运行过程' },
  'chat.editLabel': { en: 'Edit {path}', zh: '编辑 {path}' },
  'chat.editCounter': { en: 'Edit {i}/{n}', zh: '修改 {i}/{n}' },
  'chat.contentUnchanged': { en: 'Content unchanged', zh: '内容无变化' },
  'chat.writeLabel': { en: 'Write {path}', zh: '写入 {path}' },
  'chat.writeTruncated': { en: '… {total} lines total, showing the first {shown}', zh: '… 共 {total} 行,仅显示前 {shown} 行' },

  // ---- workspace picker ----
  'workspace.label': { en: 'Workspace', zh: '工作目录' },
  'workspace.select': { en: 'Choose workspace', zh: '选择工作目录' },
  'workspace.titleWith': { en: 'Workspace: {ws}', zh: '工作目录:{ws}' },
  'workspace.recentTitle': { en: 'Recent folders', zh: '最近使用的目录' },
  'workspace.empty': { en: 'No records', zh: '暂无记录' },
  'workspace.remove': { en: 'Remove from list', zh: '从列表移除' },
  'workspace.browse': { en: 'Choose a new folder…', zh: '选择新文件夹…' },

  // ---- composer ----
  'composer.tooManyImages': { en: 'At most {max} images', zh: '最多附加 {max} 张图片' },
  'composer.imageTooBig': { en: 'Image too large (>{mb}MB): {name}', zh: '图片过大(>{mb}MB):{name}' },
  'composer.clipboardImage': { en: 'clipboard image', zh: '剪贴板图片' },
  'composer.imageReadFail': { en: 'Failed to read the image', zh: '图片读取失败' },
  'composer.imageAlt': { en: 'image to send', zh: '待发送图片' },
  'composer.removeImage': { en: 'Remove image', zh: '移除图片' },
  'composer.placeholderRunning': { en: 'Running… text sent now steers the run', zh: '运行中…输入内容发送=插话纠偏' },
  'composer.placeholderIdle': { en: 'Describe the task… (Enter send, Shift+Enter newline, Ctrl+V paste image)', zh: '输入任务…(Enter 发送,Shift+Enter 换行,Ctrl+V 粘贴图片)' },
  'composer.stop': { en: 'Stop the current run', zh: '停止当前运行' },
  'composer.compactingTitle': { en: 'Compacting context…', zh: '正在压缩上下文…' },
  'composer.steer': { en: 'Steer', zh: '插话纠偏' },
  'composer.send': { en: 'Send (Enter)', zh: '发送(Enter)' },

  // ---- model picker ----
  'modelPicker.selectModel': { en: 'Choose model', zh: '选择模型' },
  'modelPicker.runningNoSwitch': { en: 'Cannot switch the model while running', zh: '运行中无法切换模型' },
  'modelPicker.switchTitle': { en: 'Switch model / reasoning effort', zh: '切换模型 / 推理强度' },
  'modelPicker.empty': { en: 'No configured models', zh: '暂无已配置模型' },
  'modelPicker.notReasoning': { en: 'The current model is not marked as reasoning-capable', zh: '当前模型未标记为支持推理' },
  'modelPicker.levelsTitle': { en: 'Reasoning effort', zh: '推理强度' },
  'modelPicker.manage': { en: 'Manage models…', zh: '管理模型…' },

  // ---- plus menu ----
  'plusMenu.title': { en: 'Image / Plan / Compact context', zh: '图片 / Plan / 压缩上下文' },
  'plusMenu.addImage': { en: 'Add image', zh: '添加图片' },
  'plusMenu.pasteHint': { en: 'or Ctrl+V to paste', zh: '或 Ctrl+V 粘贴' },
  'plusMenu.planMode': { en: 'Plan mode', zh: 'Plan 模式' },
  'plusMenu.planOn': { en: 'on', zh: '已开启' },
  'plusMenu.compact': { en: 'Compact context', zh: '压缩上下文' },
  'plusMenu.compacting': { en: 'Compacting…', zh: '压缩中…' },

  // ---- sandbox picker ----
  'sandbox.readOnlyLabel': { en: 'Read-only', zh: '只读' },
  'sandbox.readOnlyDesc': { en: 'No file modifications allowed', zh: '禁止一切文件修改' },
  'sandbox.workspaceLabel': { en: 'Workspace write', zh: '工作区可写' },
  'sandbox.workspaceDesc': { en: 'Only the workspace and temp directories are writable', zh: '仅工作区与临时目录可写' },
  'sandbox.fullLabel': { en: 'Unrestricted', zh: '不限制' },
  'sandbox.fullDesc': { en: 'File modifications are not sandboxed', zh: '文件修改不受沙箱限制' },
  'sandbox.runningNoSwitch': { en: 'Cannot switch the sandbox while running', zh: '运行中无法切换沙箱' },
  'sandbox.tooltip': { en: 'Sandbox: {label} — {desc}', zh: '沙箱:{label} — {desc}' },
  'sandbox.button': { en: 'sandbox·{label}', zh: '沙箱·{label}' },

  // ---- diff view ----
  'diff.new': { en: 'New', zh: '新增' },
  'diff.deleted': { en: 'Deleted', zh: '删除' },
  'diff.renamed': { en: 'Renamed', zh: '重命名' },
  'diff.binary': { en: 'Binary file or empty content; no textual diff', zh: '二进制文件或内容为空,无文本差异' },

  // ---- top bar ----
  'topbar.serverExited': { en: 'Server exited ({code})', zh: '服务已退出({code})' },
  'topbar.running': { en: 'Running', zh: '运行中' },
  'topbar.ready': { en: 'Ready', zh: '就绪' },
  'topbar.exitedNote': { en: 'scode serve ({workspace}) exited ({code}); sessions for this workspace have stopped', zh: 'scode serve({workspace}) 已退出({code}),该工作区的会话已停止' },
  'topbar.newSession': { en: 'New session', zh: '新会话' },
  'topbar.openLocal': { en: 'Open local folder: {ws}', zh: '打开本地目录:{ws}' },
  'topbar.moreOpen': { en: 'More ways to open', zh: '更多打开方式' },
  'topbar.openLocalEntry': { en: 'Open local folder', zh: '打开本地目录' },
  'topbar.openVscode': { en: 'Open with VSCode', zh: '通过 VSCode 打开' },
  'topbar.ctxTitle': { en: 'Context capacity', zh: '上下文容量' },
  'topbar.ctxUnknown': { en: '{tokens} tokens (window unknown)', zh: '{tokens} tokens(窗口未知)' },
  'topbar.cacheHit': { en: 'Average cache hit rate', zh: '平均缓存命中率' },
  'topbar.bdMessages': { en: 'Messages', zh: '消息' },
  'topbar.bdSysTools': { en: 'System tools', zh: '系统工具' },
  'topbar.bdSysPrompt': { en: 'System prompt', zh: '系统提示词' },
  'topbar.bdSkills': { en: 'Skills', zh: '技能' },
  'topbar.bdMcpTools': { en: 'MCP tools', zh: 'MCP 工具' },
  'topbar.bdOther': { en: 'Other', zh: '其他' },

  // ---- plan panel ----
  'plan.done': { en: 'Completed', zh: '已完成' },
  'plan.inProgress': { en: 'In progress', zh: '进行中' },
  'plan.pending': { en: 'Pending', zh: '待办' },
  'plan.title': { en: 'Plan progress', zh: '计划进度' },
  'plan.doneCount': { en: '{done}/{total} completed', zh: '{done}/{total} 已完成' },
  'plan.empty': { en: 'No plan for this session yet. In multi-step tasks the agent reports progress via update_plan, persisted with the session.', zh: '当前会话还没有计划。多步任务中 Agent 会通过 update_plan 汇报进度,并随会话持久化。' },
  'plan.buttonTooltip': { en: 'Plan progress: {done}/{total} completed', zh: '计划进度:{done}/{total} 已完成' },

  // ---- appearance settings ----
  'appearance.light': { en: 'Light', zh: '浅色' },
  'appearance.dark': { en: 'Dark', zh: '深色' },
  'appearance.system': { en: 'System', zh: '跟随系统' },
  'appearance.themeRow': { en: 'Interface theme', zh: '界面主题' },
  'appearance.themeDesc': { en: 'Light, dark, or follow the system theme.', zh: '选择浅色、深色或跟随系统主题。' },
  'appearance.fontRow': { en: 'Interface font size', zh: '界面字号' },
  'appearance.fontDesc': { en: 'Adjust the UI text size; icons and layout dimensions are unaffected.', zh: '调整应用界面的文字大小,图标和布局尺寸不受影响。' },
  'appearance.langRow': { en: 'Interface language', zh: '界面语言' },
  'appearance.langDesc': { en: 'Chinese or English; System follows the OS language.', zh: '中文或英文;跟随系统按操作系统语言显示。' },
  'appearance.langZh': { en: '中文', zh: '中文' },
  'appearance.langEn': { en: 'English', zh: 'English' },
  'appearance.codeGroup': { en: 'Code settings', zh: '代码设置' },
  'appearance.codeGroupDesc': { en: 'Theme, font size and display of code content, independent of the interface font size.', zh: '设置代码内容的主题、字号和显示方式,不受界面字号影响。' },
  'appearance.lightCode': { en: 'Light code theme', zh: '浅色代码主题' },
  'appearance.lightCodeDesc': { en: 'The highlight theme for code under a light interface.', zh: '浅色界面下代码内容使用的高亮主题。' },
  'appearance.darkCode': { en: 'Dark code theme', zh: '深色代码主题' },
  'appearance.darkCodeDesc': { en: 'The highlight theme for code under a dark interface.', zh: '深色界面下代码内容使用的高亮主题。' },
  'appearance.lineNumbers': { en: 'Show line numbers', zh: '显示行号' },
  'appearance.lineNumbersDesc': { en: 'Show line numbers in code content and diff views.', zh: '在代码内容和差异视图中显示行号。' },
  'appearance.wrap': { en: 'Wrap long lines', zh: '长行自动换行' },
  'appearance.wrapDesc': { en: 'Wrap code content when it is too long.', zh: '代码内容过长时自动换行。' },
  'appearance.codeFontSize': { en: 'Code font size', zh: '代码字号' },
  'appearance.codeFontSizeDesc': { en: 'Default font size for code blocks, file previews and diff views.', zh: '调整代码块、文件预览和差异视图的默认字号。' },

  // ---- model settings ----
  'modelSettings.newProfile': { en: 'New model profile', zh: '新建模型配置' },
  'modelSettings.editing': { en: 'Editing: {name}', zh: '编辑中:{name}' },
  'modelSettings.nameRequired': { en: 'Name is required', zh: '名称必填' },
  'modelSettings.saved': { en: 'Saved: {name}', zh: '已保存:{name}' },
  'modelSettings.confirmDelete': { en: 'Delete model profile "{name}"?', zh: '删除模型配置「{name}」?' },
  'modelSettings.deleted': { en: 'Deleted: {name}', zh: '已删除:{name}' },
  'modelSettings.setDefaultDone': { en: 'Set as default: {name}', zh: '已设为默认:{name}' },
  'modelSettings.title': { en: 'Model profiles', zh: '模型配置' },
  'modelSettings.empty': { en: 'No model profiles yet — click "New" to add one.', zh: '暂无模型配置,点击"新建"添加。' },
  'modelSettings.defaultBadge': { en: 'default', zh: '默认' },
  'modelSettings.defaultModelMark': { en: '(default model)', zh: '(默认模型)' },
  'modelSettings.setDefault': { en: 'Set as default', zh: '设为默认' },
  'modelSettings.nameLabel': { en: 'Name', zh: '名称' },
  'modelSettings.namePlaceholder': { en: 'e.g. kimi', zh: '如 kimi' },
  'modelSettings.protocolLabel': { en: 'Protocol', zh: '协议' },
  'modelSettings.protocolDefault': { en: 'default (openai-compat)', zh: '默认(openai-compat)' },
  'modelSettings.modelIdLabel': { en: 'Model ID', zh: '模型 ID' },
  'modelSettings.imageInput': { en: 'Image input', zh: '图片输入' },
  'modelSettings.optionDefault': { en: 'default', zh: '默认' },
  'modelSettings.optionYes': { en: 'supported', zh: '支持' },
  'modelSettings.optionNo': { en: 'not supported', zh: '不支持' },
  'modelSettings.reasoning': { en: 'Reasoning-capable (thinking)', zh: '支持思考(reasoning)' },

  // ---- settings dialog ----
  'settings.appearance': { en: 'Appearance', zh: '外观' },
  'settings.models': { en: 'Models', zh: '模型' },
  'settings.title': { en: 'Settings', zh: '设置' },
  'settings.unsavedConfirm': { en: 'There are unsaved changes. Close anyway?', zh: '有未保存的修改,确定关闭?' },

  // ---- sidebar ----
  'sidebar.justNow': { en: 'just now', zh: '刚刚' },
  'sidebar.minutes': { en: '{n}m', zh: '{n}分钟' },
  'sidebar.hours': { en: '{n}h', zh: '{n}小时' },
  'sidebar.days': { en: '{n}d', zh: '{n}天' },
  'sidebar.confirmDelete': { en: 'Delete session "{id}"? This cannot be undone.', zh: '删除会话「{id}」?此操作不可恢复。' },
  'sidebar.confirmPrune': { en: 'Prune empty sessions (no message ever sent) in this workspace?', zh: '清理当前工作区的空会话(从未发送消息的)?' },
  'sidebar.pruned': { en: 'Pruned {n} empty sessions', zh: '已清理 {n} 个空会话' },
  'sidebar.nothingToPrune': { en: 'No empty sessions to prune', zh: '没有需要清理的空会话' },
  'sidebar.expand': { en: 'Expand sidebar', zh: '展开侧栏' },
  'sidebar.collapse': { en: 'Collapse sidebar', zh: '折叠侧栏' },
  'sidebar.newChat': { en: 'New session', zh: '新建会话' },
  'sidebar.empty': { en: 'No past sessions', zh: '暂无历史会话' },
  'sidebar.project': { en: 'Project', zh: '项目' },
  'sidebar.pruneTitle': { en: 'Prune empty sessions in this workspace', zh: '清理当前工作区的空会话' },
  'sidebar.deleteSession': { en: 'Delete session', zh: '删除会话' },
  'sidebar.settings': { en: 'Settings', zh: '设置' },

  // ---- task panel ----
  'taskPanel.running': { en: 'Running', zh: '运行中' },
  'taskPanel.killed': { en: 'Killed', zh: '已终止' },
  'taskPanel.exitCode': { en: 'exit code {code}', zh: '退出码 {code}' },
  'taskPanel.done': { en: 'Completed', zh: '已完成' },
  'taskPanel.title': { en: 'Background tasks', zh: '后台任务' },
  'taskPanel.runningCount': { en: '{n} running', zh: '{n} 运行中' },
  'taskPanel.empty': { en: 'No background tasks. Commands that time out or use run_in_background appear here.', zh: '没有后台任务。命令超时或使用 run_in_background 后会出现在这里。' },
  'taskPanel.kill': { en: 'Kill', zh: '终止' },
  'taskPanel.killTitle': { en: "Kill the task's process tree", zh: '终止该任务的进程树' },
  'taskPanel.collapseOutput': { en: 'Collapse output', zh: '收起输出' },
  'taskPanel.expandOutput': { en: 'View output', zh: '查看输出' },
  'taskPanel.buttonTooltip': { en: '{n} background tasks running', zh: '{n} 个后台任务运行中' },

  // ---- stores ----
  'store.toolAborted': { en: '(session interrupted; tool incomplete)', zh: '(会话中断,工具未完成)' },
  'store.modelSwitched': { en: 'Model switched: {provider} / {model}', zh: '模型已切换:{provider} / {model}' },
  'store.thinkingSwitched': { en: 'Reasoning effort switched: {label}', zh: '推理强度已切换:{label}' },
  'store.startFail': { en: 'Failed to start: {err}', zh: '启动失败:{err}' },
  'store.resumeFail': { en: 'Failed to resume the session: {err}', zh: '恢复会话失败:{err}' },
  'store.forkedTurn': { en: 'Forked from that turn of session {id}', zh: '已从会话 {id} 的该轮分叉' },
  'store.forked': { en: 'Forked from session {id}', zh: '已从会话 {id} 分叉' },
  'store.forkFail': { en: 'Fork failed: {err}', zh: '分叉失败:{err}' },
  'store.deleteFail': { en: 'Failed to delete the session: {err}', zh: '删除会话失败:{err}' },
  'store.pruneFail': { en: 'Prune failed: {err}', zh: '清理失败:{err}' },
  'store.imageWhileRunning': { en: 'Images cannot be sent while running — wait for the run to finish or stop it first', zh: '运行中暂不支持发送图片，请等待运行结束或停止后再发送' },
  'store.createFail': { en: 'Failed to create the session: {err}', zh: '创建会话失败:{err}' },
  'store.planOn': { en: 'Plan mode on (read-only research; executes after the plan review)', zh: '已进入计划模式(只读调研,计划评审后执行)' },
  'store.planOff': { en: 'Plan mode off', zh: '已退出计划模式' },
  'store.sandboxOff': { en: 'Sandbox off (file modifications unrestricted)', zh: '沙箱已关闭(不限制文件修改)' },
  'store.sandboxWorkspace': { en: 'Sandbox: workspace-write only', zh: '沙箱:仅工作区可写' },
  'store.sandboxReadOnly': { en: 'Sandbox: read-only (no file modifications)', zh: '沙箱:只读(禁止文件修改)' },
  'store.nothingToCompact': { en: 'History is short; nothing to compact', zh: '历史很短,无需压缩' },
  'store.compacted': { en: 'Context compacted{detail}', zh: '上下文已压缩{detail}' },
  'store.compactFail': { en: 'Compaction failed: {err}', zh: '压缩失败:{err}' },
};

// ---------------------------------------------------------------------------
// lookup
// ---------------------------------------------------------------------------

function translate(lang: Lang, key: string, vars?: Vars): string {
  const entry = catalog[key];
  let s = entry ? (lang === 'zh' ? entry.zh : entry.en) : key; // unknown keys echo
  if (vars) {
    for (const [k, v] of Object.entries(vars)) {
      s = s.split('{' + k + '}').join(String(v));
    }
  }
  return s;
}

/** t translates key at call time (stores, callbacks, non-React code). */
export function t(key: string, vars?: Vars): string {
  return translate(useI18n.getState().lang, key, vars);
}

/** useT is the reactive component form: re-renders on a language switch. */
export function useT() {
  const lang = useI18n((s) => s.lang);
  return (key: string, vars?: Vars) => translate(lang, key, vars);
}

/** thinkingLabel maps a reasoning effort to its display label. */
export function thinkingLabel(level: string): string {
  switch (level) {
    case 'off':
      return t('common.thinking.off');
    case 'low':
      return t('common.thinking.low');
    case 'medium':
      return t('common.thinking.medium');
    case 'high':
      return t('common.thinking.high');
  }
  return t('common.thinking.default');
}

/** fmtTokens compacts a token count for the hover card (zh 万/亿, en K/M). */
export function fmtTokens(n: number): string {
  if (useI18n.getState().lang === 'zh') {
    if (n >= 100_000_000) return (n / 100_000_000).toFixed(1).replace(/\.0$/, '') + '亿';
    if (n >= 10_000) return (n / 10_000).toFixed(1).replace(/\.0$/, '') + '万';
    return String(n);
  }
  if (n >= 1_000_000) return (n / 1_000_000).toFixed(1).replace(/\.0$/, '') + 'M';
  if (n >= 1_000) return (n / 1_000).toFixed(1).replace(/\.0$/, '') + 'K';
  return String(n);
}
