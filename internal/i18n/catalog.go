package i18n

// catalog maps message keys to their translations, dot-namespaced by
// surface: update.* / version.* (CLI notices), tui.<area>.* (interactive
// UI), cli.* (REPL/print). Keep entries grouped and sorted by prefix.
var catalog = map[string]Message{
	// ---- CLI: update / version ----
	"update.notice": {
		En: "A new scode release %s is available (current %s) — run `scode update` to upgrade (%s)",
		Zh: "scode 有新版本 %s（当前 %s）— 运行 `scode update` 升级（%s）",
	},
	"update.checking": {
		En: "Checking %s for updates...",
		Zh: "检查更新（%s）...",
	},
	"update.uptodate": {
		En: "scode %s is up to date (latest release: %s)",
		Zh: "scode %s 已是最新（最新 release: %s）",
	},
	"update.done": {
		En: "Updated to %s — restart scode for it to take effect.",
		Zh: "已更新到 %s，重新运行 scode 生效。",
	},
	"version.uptodate": {
		En: "Already on the latest version.",
		Zh: "已是最新版本",
	},

	// ---- TUI: /models manager ----
	"tui.models.catalogFail": {
		En: "Failed to read the preset model catalog: %s",
		Zh: "预设模型目录读取失败: %s",
	},
	"tui.models.noReasoning": {
		En: "No reasoning (confirm directly)",
		Zh: "无推理(直接确认)",
	},
	"tui.models.writeFail": {
		En: "Failed to write the configuration: %s",
		Zh: "配置写入失败: %s",
	},
	"tui.models.switchFail": {
		En: "Model configured, but switching failed: %s",
		Zh: "模型已配置,但切换失败: %s",
	},
	"tui.models.configured": {
		En: "Model configured and switched → %s / %s",
		Zh: "模型已配置并切换 → %s / %s",
	},
	"tui.models.effort": {
		En: "Reasoning effort → %s",
		Zh: "推理强度 → %s",
	},
	"tui.models.defaultModelFail": {
		En: "(failed to save the default model: %s)",
		Zh: "(默认模型保存失败: %s)",
	},
	"tui.models.defaultEffortFail": {
		En: "(failed to save the default reasoning effort: %s)",
		Zh: "(默认推理强度保存失败: %s)",
	},
	"tui.models.savedDefault": {
		En: "Saved as default",
		Zh: "已存为默认",
	},
	"tui.models.vendorRow": {
		En: " · %s · %d models",
		Zh: " · %s · %d 个模型",
	},
	"tui.models.configuredMark": {
		En: " · configured",
		Zh: " · 已配置",
	},
	"tui.models.vendorHint": {
		En: "  ↑/↓ select · Enter configure key · Esc cancel",
		Zh: "  ↑/↓ 选择 · Enter 配置 Key · Esc 取消",
	},
	"tui.models.vendorTitle": {
		En: "Model manager · Choose vendor",
		Zh: "模型管理 · 选择厂商",
	},
	"tui.models.savedKey": {
		En: "  Saved key: %s · Enter keeps it, typing replaces it",
		Zh: "  已保存 Key: %s · 直接 Enter 保留,输入则替换",
	},
	"tui.models.keyHint": {
		En: "  Enter next (choose model) · Esc back · empty = use environment variable",
		Zh: "  Enter 下一步(选择模型) · Esc 返回 · 留空则使用环境变量",
	},
	"tui.models.keyTitle": {
		En: "Configure API key",
		Zh: "配置 API Key",
	},
	"tui.models.modelTitle": {
		En: "Choose model",
		Zh: "选择模型",
	},
	"tui.models.reasoningMark": {
		En: " · reasoning",
		Zh: " · 推理",
	},
	"tui.models.modelHint": {
		En: "  ↑/↓ select · Enter confirm · Esc back",
		Zh: "  ↑/↓ 选择 · Enter 确认信息 · Esc 返回",
	},
	"tui.models.confirmTitle": {
		En: "Confirm configuration",
		Zh: "确认配置",
	},
	"tui.models.keyEmpty": {
		En: "not set (environment variable will be used)",
		Zh: "未填写(使用环境变量)",
	},
	"tui.models.vendorLabel": {
		En: "  Vendor: ",
		Zh: "  厂商:   ",
	},
	"tui.models.modelLabel": {
		En: "  Model:  ",
		Zh: "  模型:   ",
	},
	"tui.models.protocolLabel": {
		En: "  Protocol: ",
		Zh: "  协议:   ",
	},
	"tui.models.specLabel": {
		En: "  Params: ",
		Zh: "  参数:   ",
	},
	"tui.models.writeLabel": {
		En: "  Write: ",
		Zh: "  写入:   ",
	},
	"tui.models.presetMark": {
		En: " · preset",
		Zh: " · 预设",
	},
	"tui.models.effortHint": {
		En: "  ↑/↓ reasoning effort · Enter finish · Esc back",
		Zh: "  ↑/↓ 选择推理强度 · Enter 完成配置 · Esc 返回",
	},
	"tui.models.confirmHint": {
		En: "  Enter finish configuration · Esc back",
		Zh: "  Enter 完成配置 · Esc 返回",
	},
	"tui.models.spec": {
		En: "context %s · max output %s",
		Zh: "上下文 %s · 最大输出 %s",
	},

	// ---- TUI: shared overlay bits ----
	"tui.common.confirmDelete": {
		En: "  press d again to confirm deleting %s",
		Zh: "  再按 d 确认删除 %s",
	},
	"tui.common.cycleHint": {
		En: "  (enter to cycle)",
		Zh: "  (enter 切换)",
	},
	"tui.common.formHint": {
		En: "  ↑/↓ fields · enter cycle option/save · type to edit · ctrl+u clear · esc back",
		Zh: "  ↑/↓ 字段 · enter 切换选项/保存 · 直接输入文字 · ctrl+u 清空 · esc 返回",
	},
	"tui.agents.toolCount": {
		En: "%d tools",
		Zh: "%d 个工具",
	},

	// ---- TUI: sub-agent manager ----
	"tui.agents.effortDefault": {
		En: "default",
		Zh: "默认",
	},
	"tui.agents.inheritModel": {
		En: "(inherit session model)",
		Zh: "(继承会话模型)",
	},
	"tui.agents.nameLabel": {
		En: "Name",
		Zh: "名称",
	},
	"tui.agents.nameLabelEdit": {
		En: "Name (renaming creates a new entry)",
		Zh: "名称(重命名会新建一条)",
	},
	"tui.agents.descLabel": {
		En: "Description · when to delegate to it",
		Zh: "描述 · 何时委派给它",
	},
	"tui.agents.modelLabel": {
		En: "Model (enter cycles active models)",
		Zh: "模型(enter 在已激活模型间切换)",
	},
	"tui.agents.effortLabel": {
		En: "Reasoning effort",
		Zh: "推理强度",
	},
	"tui.agents.saveLabel": {
		En: "[Save and apply]",
		Zh: "[保存并生效]",
	},
	"tui.agents.builtinNoDelete": {
		En: "Built-in agents cannot be deleted — editing and saving creates a custom override",
		Zh: "内置代理不可删除 — enter 编辑保存后即为自定义覆盖",
	},
	"tui.agents.deleted": {
		En: "Sub-agent %s deleted",
		Zh: "子代理 %s 已删除",
	},
	"tui.agents.saved": {
		En: "Sub-agent %s saved · takes effect on next delegation",
		Zh: "子代理 %s 已保存 · 下次委派生效",
	},
	"tui.agents.row": {
		En: " · %s · effort %s",
		Zh: " · %s · 推理 %s",
	},
	"tui.agents.builtinMark": {
		En: " · built-in",
		Zh: " · 内置",
	},
	"tui.agents.empty": {
		En: "  (no sub-agents — press n to create)",
		Zh: "  (无子代理 — 按 n 新建)",
	},
	"tui.agents.listHint": {
		En: "  ↑/↓ select · enter edit · n new · d delete · esc close",
		Zh: "  ↑/↓ 选择 · enter 编辑 · n 新建 · d 删除 · esc 关闭",
	},
	"tui.agents.delegateNote": {
		En: "  Delegation: ask the model to use the task tool (sub-agents are read-only with isolated contexts)",
		Zh: "  委派:会话中让模型用 task 工具(子代理独立上下文,只读,互不影响)",
	},
	"tui.agents.title": {
		En: "Sub-agent manager",
		Zh: "子代理管理",
	},
	"tui.agents.newTitle": {
		En: "New sub-agent",
		Zh: "子代理新建",
	},
	"tui.agents.editTitle": {
		En: "Edit sub-agent: %s",
		Zh: "子代理编辑: %s",
	},

	// ---- TUI: MCP server manager ----
	"tui.mcp.nameLabel": {
		En: "Name [A-Za-z0-9_-]",
		Zh: "名称 [A-Za-z0-9_-]",
	},
	"tui.mcp.transportLabel": {
		En: "Transport",
		Zh: "传输",
	},
	"tui.mcp.commandLabel": {
		En: "Command line (command + args)",
		Zh: "命令行 (命令 + 参数)",
	},
	"tui.mcp.saveLabel": {
		En: "Save and connect",
		Zh: "保存并连接",
	},
	"tui.mcp.enabled": {
		En: "mcp(%s) enabled, connecting",
		Zh: "mcp(%s) 已启用,连接中",
	},
	"tui.mcp.disabled": {
		En: "mcp(%s) disabled",
		Zh: "mcp(%s) 已停用",
	},
	"tui.mcp.deleted": {
		En: "mcp(%s) deleted",
		Zh: "mcp(%s) 已删除",
	},
	"tui.mcp.saved": {
		En: "mcp(%s) saved, connecting",
		Zh: "mcp(%s) 已保存,连接中",
	},
	"tui.mcp.restarted": {
		En: "mcp(%s) restarted",
		Zh: "mcp(%s) 已重启",
	},
	"tui.mcp.statusDisabled": {
		En: "disabled",
		Zh: "已停用",
	},
	"tui.mcp.statusNotRunning": {
		En: "not running",
		Zh: "未运行",
	},
	"tui.mcp.statusConnected": {
		En: "connected · %d tools",
		Zh: "已连接 · %d 工具",
	},
	"tui.mcp.statusReconnecting": {
		En: "reconnecting %d/%d",
		Zh: "重连中 %d/%d",
	},
	"tui.mcp.statusGaveUp": {
		En: "gave up (reconnect attempts exhausted)",
		Zh: "已放弃(重连耗尽)",
	},
	"tui.mcp.statusConnecting": {
		En: "connecting…",
		Zh: "连接中…",
	},
	"tui.mcp.empty": {
		En: "  (no servers — press n to create)",
		Zh: "  (无服务器 — 按 n 新建)",
	},
	"tui.mcp.listHint": {
		En: "  ↑/↓ select · enter edit · n new · t toggle · d delete · esc close",
		Zh: "  ↑/↓ 选择 · enter 编辑 · n 新建 · t 启停 · d 删除 · esc 关闭",
	},
	"tui.mcp.title": {
		En: "MCP servers",
		Zh: "MCP 服务器",
	},
	"tui.mcp.newTitle": {
		En: "New MCP server",
		Zh: "MCP 新建",
	},
	"tui.mcp.editTitle": {
		En: "Edit MCP server: %s",
		Zh: "MCP 编辑: %s",
	},

	// ---- TUI: render (banner / status bar / approvals) ----
	"tui.render.session": {
		En: "session %s",
		Zh: "会话 %s",
	},
	"tui.render.hintLine1": {
		En: "Enter send · ctrl+j newline · typing while running=steer · esc interrupt · ctrl+c quit",
		Zh: "Enter 发送 · ctrl+j 换行 · 运行中输入=steer · esc 中断 · ctrl+c 退出",
	},
	"tui.render.hintLine2": {
		En: "ctrl+o expand thinking · @ pick file · alt+v paste image · / commands",
		Zh: "ctrl+o 展开思考 · @ 选文件 · alt+v 贴图 · / 命令",
	},
	"tui.render.thinkingFold": {
		En: "… %d lines of thinking (ctrl+o to expand)",
		Zh: "… 思考共 %d 行 (ctrl+o 展开)",
	},
	"tui.render.planFeedbackLabel": {
		En: " ❯ plan feedback ",
		Zh: " ❯ 计划反馈 ",
	},
	"tui.render.approvalHint": {
		En: "  ←/→ switch · Enter confirm · Esc deny · %s",
		Zh: "  ←/→ 切换 · Enter 确认 · Esc 拒绝 · %s",
	},
	"tui.render.planApprove": {
		En: "Approve & run",
		Zh: "批准执行",
	},
	"tui.render.planRevise": {
		En: "Send back",
		Zh: "打回修订",
	},
	"tui.render.planReject": {
		En: "Reject",
		Zh: "拒绝",
	},
	"tui.render.planLines": {
		En: "  (lines %d–%d of %d)",
		Zh: "  (第 %d–%d 行,共 %d 行)",
	},
	"tui.render.planHint": {
		En: "  ↑/↓ scroll · ←/→ switch button · Enter confirm · type then Enter=send back · Esc reject",
		Zh: "  ↑/↓ 滚动 · ←/→ 切换按钮 · Enter 确认 · 输入文字后 Enter=打回修订 · Esc 拒绝",
	},
	"tui.render.planTitle": {
		En: "Plan review",
		Zh: "计划确认",
	},
	"tui.render.planDefaultFeedback": {
		En: "Please revise the plan",
		Zh: "请修订该计划",
	},
	"tui.render.planEcho": {
		En: "(plan review: %s)",
		Zh: "(计划确认: %s)",
	},

	// ---- TUI: main model (composer / toasts / guards) ----
	"tui.main.placeholder": {
		En: "Type a message… (Enter send · ctrl+j newline · alt+v image · / commands)",
		Zh: "输入消息… (Enter 发送 · ctrl+j 换行 · alt+v 图片 · / 命令)",
	},
	"tui.main.notSelectable": {
		En: "Not selectable: %s",
		Zh: "不可选择: %s",
	},
	"tui.main.clipWatch": {
		En: "(watching the clipboard: screenshots or copied images within %ds attach automatically; alt+v / ctrl+v again restarts the timer)",
		Zh: "(剪贴板图片监控中：%d 秒内截屏或复制图片将自动附加；再次按 alt+v / ctrl+v 重新计时)",
	},
	"tui.main.noNewSessionWhileRunning": {
		En: "Cannot start a new session while running",
		Zh: "运行中无法开始新会话",
	},
	"tui.main.noSandboxWhileRunning": {
		En: "Cannot switch the sandbox while running",
		Zh: "运行中无法切换沙箱",
	},
	"tui.main.noModelWhileRunning": {
		En: "Cannot switch the model while running",
		Zh: "运行中无法切换模型",
	},
	"tui.main.noModelCfgWhileRunning": {
		En: "Cannot configure models while running",
		Zh: "运行中无法配置模型",
	},
	"tui.main.newSessionStarted": {
		En: "New session started — resume the previous one: scode --resume %s",
		Zh: "新会话已开始 — 原会话恢复: scode --resume %s",
	},
	"tui.main.copyFail": {
		En: "Copy failed: could not write to the clipboard",
		Zh: "复制失败：无法写入剪贴板",
	},
	"tui.main.copiedLines": {
		En: "Copied %d lines",
		Zh: "已复制 %d 行",
	},
	"tui.main.toolFailed": {
		En: "%s failed",
		Zh: "%s 失败",
	},

	// ---- TUI: /config panel ----
	"tui.config.on": {
		En: "on",
		Zh: "开",
	},
	"tui.config.off": {
		En: "off",
		Zh: "关",
	},
	"tui.config.never": {
		En: "never",
		Zh: "永不清理",
	},
	"tui.config.days": {
		En: "%d days",
		Zh: "%d 天",
	},
	"tui.config.hint": {
		En: "  ↑/↓ select · enter/t toggle · esc close",
		Zh: "  ↑/↓ 选择 · enter/t 切换 · esc 关闭",
	},
	"tui.config.title": {
		En: "Basic configuration",
		Zh: "基本配置",
	},
	"tui.config.autoCompact.label": {
		En: "Auto-compact context",
		Zh: "上下文自动压缩",
	},
	"tui.config.autoCompact.desc": {
		En: "Summarize and compact history when approaching the model window (effective immediately)",
		Zh: "接近模型窗口时自动总结压缩历史上下文(即时生效)",
	},
	"tui.config.retention.label": {
		En: "Log retention period",
		Zh: "日志清理周期",
	},
	"tui.config.retention.desc": {
		En: "Delete session log files older than this many days at startup (0 = never)",
		Zh: "启动时删除超过该天数的会话日志文件(0 = 永不清理)",
	},
	"tui.config.autoMemory.label": {
		En: "Auto Memory",
		Zh: "Auto Memory · 自动记忆",
	},
	"tui.config.autoMemory.desc": {
		En: "The AI automatically remembers key facts from conversations (persisted; read by the memory subsystem)",
		Zh: "AI 自动记住对话中的关键信息(配置已持久化,记忆子系统读取)",
	},
	"tui.config.typedMemory.label": {
		En: "Typed Memory",
		Zh: "Typed Memory · 分类记忆",
	},
	"tui.config.typedMemory.desc": {
		En: "Memories are stored by category (persisted; read by the memory subsystem)",
		Zh: "记忆按分类存储(配置已持久化,记忆子系统读取)",
	},
	"tui.config.memoryRelevance.label": {
		En: "Memory Relevance",
		Zh: "Memory Relevance · 相关性选择",
	},
	"tui.config.memoryRelevance.desc": {
		En: "Off = the AI picks relevant memories automatically, no manual selection (persisted)",
		Zh: "关闭 = 由 AI 自动筛选相关记忆,不手动选择(配置已持久化)",
	},
	"tui.config.memoryAutoExtract.label": {
		En: "Memory Auto Extraction",
		Zh: "Memory Auto Extraction · 自动提取",
	},
	"tui.config.memoryAutoExtract.desc": {
		En: "Off = memories are not extracted at session end; trigger manually (persisted)",
		Zh: "关闭 = 会话结束不自动抽取记忆,需手动触发(配置已持久化)",
	},
	"tui.config.rewind.label": {
		En: "Rewind code · checkpoint rollback",
		Zh: "Rewind code · 检查点回滚",
	},
	"tui.config.rewind.desc": {
		En: "Allow rolling code back to earlier AI checkpoints (persisted; read by the checkpoint subsystem)",
		Zh: "允许把代码回滚到之前的 AI 检查点(配置已持久化,检查点子系统读取)",
	},
	"tui.config.clipWatch.label": {
		En: "Clipboard image reading (alt+v / ctrl+v)",
		Zh: "剪贴板图片读取 (alt+v / ctrl+v)",
	},
	"tui.config.clipWatch.desc": {
		En: "alt+v / ctrl+v probe the clipboard once (ctrl+v is often swallowed by Windows terminals; alt+v is reliable); on a miss the watch loops ~15s, attaches a detected image, then stops. Zero calls at idle; disable if your antivirus complains",
		Zh: "alt+v / ctrl+v 探测一次剪贴板(ctrl+v 常被 Windows 终端拦截,alt+v 更可靠);未读到图片则循环监控约 15 秒,检测到图片自动附加后停止,再次触发重新激活。空闲时零调用,担心杀毒告警可关闭",
	},
	"tui.config.language.label": {
		En: "Interface language",
		Zh: "界面语言",
	},
	"tui.config.language.desc": {
		En: "UI display language (SCODE_LANG overrides this setting).",
		Zh: "界面显示语言(SCODE_LANG 环境变量优先于此设置)。",
	},
	"tui.config.searchProvider.label": {
		En: "Web search engine",
		Zh: "网络搜索引擎",
	},
	"tui.config.searchProvider.desc": {
		En: "Backend for the web_search tool: duckduckgo needs no API key (scraping-based, may be unstable); brave/tavily are higher quality but need the BRAVE_API_KEY / TAVILY_API_KEY environment variable.",
		Zh: "web_search 工具的后端:duckduckgo 无需 API key(网页抓取,可能不稳定);brave/tavily 质量更高,但需要 BRAVE_API_KEY / TAVILY_API_KEY 环境变量。",
	},
	"tui.config.langAuto": {
		En: "system (%s)",
		Zh: "跟随系统(%s)",
	},
	"tui.config.langZh": {
		En: "中文 (Chinese)",
		Zh: "中文",
	},
	"tui.config.langEn": {
		En: "English",
		Zh: "English (英文)",
	},

	// ---- TUI: /sandbox picker ----
	"tui.sandbox.readOnlyDesc": {
		En: "No file modifications allowed",
		Zh: "禁止一切文件修改",
	},
	"tui.sandbox.workspaceWriteDesc": {
		En: "Only the workspace and temp directories are writable",
		Zh: "仅工作区与临时目录可写",
	},
	"tui.sandbox.fullAccessDesc": {
		En: "File modifications are not sandboxed",
		Zh: "文件修改不受沙箱限制",
	},
	"tui.sandbox.badgeReadOnly": {
		En: "read-only",
		Zh: "只读",
	},
	"tui.sandbox.badgeWritable": {
		En: "writable",
		Zh: "可写",
	},
	"tui.sandbox.badgeUnrestricted": {
		En: "unrestricted",
		Zh: "不限制",
	},
	"tui.sandbox.noteOff": {
		En: "Sandbox off (file modifications unrestricted)",
		Zh: "沙箱已关闭(不限制文件修改)",
	},
	"tui.sandbox.noteWorkspace": {
		En: "Sandbox: workspace-write only",
		Zh: "沙箱:仅工作区可写",
	},
	"tui.sandbox.noteReadOnly": {
		En: "Sandbox: read-only (no file modifications)",
		Zh: "沙箱:只读(禁止文件修改)",
	},
	"tui.sandbox.noteMode": {
		En: "Sandbox: %s",
		Zh: "沙箱:%s",
	},
	"tui.sandbox.currentMark": {
		En: " · current",
		Zh: " · 当前",
	},
	"tui.sandbox.hint": {
		En: "  ↑/↓ select · Enter apply · Esc cancel",
		Zh: "  ↑/↓ 选择 · Enter 应用 · Esc 取消",
	},
	"tui.sandbox.title": {
		En: "Sandbox mode",
		Zh: "沙箱模式",
	},

	// ---- TUI: tool rows ----
	"tui.tool.subagent": {
		En: "sub-agent",
		Zh: "子代理",
	},
	"tui.tool.subagentNamed": {
		En: "sub-agent %s",
		Zh: "子代理 %s",
	},
	"tui.tool.writeSummary": {
		En: "%s (%d lines written)",
		Zh: "%s (写入 %d 行)",
	},
	"tui.tool.editSummary": {
		En: "%s (%d edits)",
		Zh: "%s (%d 处修改)",
	},
	"tui.tool.moreLines": {
		En: "… %d more lines",
		Zh: "… 还有 %d 行",
	},
	"tui.tool.noErrOutput": {
		En: "(no error output)",
		Zh: "(无错误输出)",
	},
	"tui.tool.progressStart": {
		En: "starting · model %s",
		Zh: "启动 · 模型 %s",
	},
	"tui.tool.progressStep": {
		En: "step %d · %s",
		Zh: "第 %d 步 · %s",
	},

	// ---- TUI: /model picker ----
	"tui.modelpicker.none": {
		En: "No configured models (settings.json providers)",
		Zh: "没有已配置的模型 (settings.json 的 providers)",
	},
	"tui.modelpicker.switched": {
		En: "Model → %s / %s",
		Zh: "模型 → %s / %s",
	},
	"tui.modelpicker.listHint": {
		En: "  ↑/↓ select · Enter next (reasoning effort) · Esc cancel",
		Zh: "  ↑/↓ 选择 · Enter 下一步(推理强度) · Esc 取消",
	},
	"tui.modelpicker.listTitle": {
		En: "Switch model",
		Zh: "切换模型",
	},
	"tui.modelpicker.effortHint": {
		En: "  ↑/↓ select · Enter apply (model + reasoning effort) · Esc back",
		Zh: "  ↑/↓ 选择 · Enter 应用(模型+推理强度) · Esc 返回",
	},
	"tui.modelpicker.effortTitle": {
		En: "Reasoning effort",
		Zh: "推理强度",
	},
	"tui.render.cacheNone": {
		En: "cache —",
		Zh: "缓存 —",
	},
	"tui.render.cache": {
		En: "cache %d%%",
		Zh: "缓存 %d%%",
	},
	"tui.render.planProgress": {
		En: "plan %d/%d",
		Zh: "计划 %d/%d",
	},
	"tui.render.effortOff": {
		En: "off",
		Zh: "关闭",
	},
	"tui.render.effortLow": {
		En: "low",
		Zh: "低",
	},
	"tui.render.effortMedium": {
		En: "medium",
		Zh: "中",
	},
	"tui.render.effortHigh": {
		En: "high",
		Zh: "高",
	},
	"tui.render.effortDefault": {
		En: "default",
		Zh: "默认",
	},

	// ---- TUI: approvals ----
	"tui.approval.allowOnce": {
		En: "Allow once",
		Zh: "允许一次",
	},
	"tui.approval.allowSession": {
		En: "Allow for session",
		Zh: "本会话允许",
	},
	"tui.approval.allowProject": {
		En: "Allow for project",
		Zh: "本项目允许",
	},
	"tui.approval.deny": {
		En: "Deny",
		Zh: "拒绝",
	},
	"tui.approval.thisTimeOnly": {
		En: "This time only",
		Zh: "仅本次",
	},
	"tui.approval.forSession": {
		En: "For this session",
		Zh: "本会话生效",
	},
	"tui.approval.toolTitle": {
		En: "Tool approval · rule %q",
		Zh: "工具审批 · 规则 %q",
	},
	"tui.approval.toolHint": {
		En: "Keys: [y] allow once [a] allow for session [p] allow for project [n] deny",
		Zh: "快捷键 [y] 允许一次 [a] 本会话允许 [p] 本项目允许 [n] 拒绝",
	},
	"tui.approval.planHint": {
		En: "[y] approve & run  [n] reject  any other input=revision feedback  [esc] reject",
		Zh: "[y] 批准并执行  [n] 拒绝  其他输入=反馈意见,打回修订  [esc] 拒绝",
	},
	"tui.approval.sandboxTitle": {
		En: "Sandbox escalation approval",
		Zh: "沙箱提权审批",
	},
	"tui.approval.sandboxBody": {
		En: "%s  %s → %s\n%s\nReason: %s",
		Zh: "%s  %s → %s\n%s\n理由: %s",
	},
	"tui.approval.sandboxHint": {
		En: "Keys: [y] this time only [a] for this session [n] deny",
		Zh: "快捷键 [y] 仅本次 [a] 本会话生效 [n] 拒绝",
	},

	// ---- TUI: clipboard images ----
	"tui.clip.attached": {
		En: "(attached %s %s, %d bytes — sent with the next message; delete the placeholder to cancel)",
		Zh: "(已附加 %s %s，%d 字节 — 随下条消息发送；删除占位符可取消)",
	},
	"tui.clip.imageName": {
		En: "clipboard image",
		Zh: "剪贴板图片",
	},
	"tui.clip.watchTimeout": {
		En: "(clipboard watch timed out: no image detected; press alt+v / ctrl+v to watch again)",
		Zh: "(剪贴板监控超时：未检测到图片，按 alt+v / ctrl+v 重新监控)",
	},

	// ---- TUI: /memory ----
	"tui.memory.deleted": {
		En: "Memory deleted",
		Zh: "记忆已删除",
	},
	"tui.memory.extracting": {
		En: "Extracting memories…",
		Zh: "记忆提取中…",
	},
	"tui.memory.empty": {
		En: "  (no memories yet — e extracts now, or enable session-end auto-extraction in /config)",
		Zh: "  (暂无记忆 — e 立即提取,或在 /config 开启会话结束自动提取)",
	},
	"tui.memory.hint": {
		En: "  ↑/↓ select · s select (with Relevance on, only selected memories inject) · d delete · e extract · esc close",
		Zh: "  ↑/↓ 选择 · s 已选(Relevance 开启时仅注入已选) · d 删除 · e 提取 · esc 关闭",
	},
	"tui.memory.confirmDelete": {
		En: "  press d again to confirm",
		Zh: "  再按 d 确认删除",
	},
	"tui.memory.title": {
		En: "Long-term memory",
		Zh: "长期记忆",
	},

	// ---- TUI: palette / file picker ----
	"tui.palette.sandboxDesc": {
		En: "Choose the sandbox mode (popup picker)",
		Zh: "选择沙箱模式（弹出选择框）",
	},
	"tui.palette.title": {
		En: "Commands",
		Zh: "命令",
	},
	"tui.picker.attachFail": {
		En: "Cannot attach %s (unsupported image type or over the size limit) — path inserted instead",
		Zh: "无法附加 %s（不是受支持的图片或超过大小上限）— 已插入路径",
	},
	"tui.picker.noImageModel": {
		En: "(the current model does not accept images — path inserted; the model can read the file itself)",
		Zh: "(当前模型不支持图片输入 — 已插入路径，模型可自行读取文件)",
	},
	"tui.picker.hints": {
		En: "↑↓ browse · enter enter/select · c clipboard · esc cancel",
		Zh: "↑↓ 浏览 · enter 进入/选择 · c 剪贴板 · esc 取消",
	},
	"tui.picker.hintAttach": {
		En: "  · images→attachment · others→path",
		Zh: "  · 图片→附件 · 其它→路径",
	},
	"tui.picker.hintNoImage": {
		En: "  · the model has no image input; paths only",
		Zh: "  · 模型不支持图片，一律插路径",
	},
	"tui.picker.title": {
		En: "Choose file",
		Zh: "选择文件",
	},

	// ---- TUI: /rewind ----
	"tui.rewind.restored": {
		En: "Rolled back to %s · restored %d files",
		Zh: "已回滚到 %s · 恢复 %d 个文件",
	},
	"tui.rewind.oneFile": {
		En: "1 file",
		Zh: "1 个文件",
	},
	"tui.rewind.nFiles": {
		En: "%d files",
		Zh: "%d 个文件",
	},
	"tui.rewind.tooLarge": {
		En: ", %d too large to archive",
		Zh: ",%d 个过大未留档",
	},
	"tui.rewind.empty": {
		En: "  (no checkpoints yet — AI edit/write changes are archived automatically)",
		Zh: "  (暂无检查点 — AI 的 edit/write 修改会自动留档)",
	},
	"tui.rewind.hint": {
		En: "  ↑/↓ select · enter twice to confirm restore · esc back",
		Zh: "  ↑/↓ 选择 · enter 两次确认恢复 · esc 返回",
	},
	"tui.rewind.confirm": {
		En: "  press enter again to roll files back to %s (the conversation is unaffected)",
		Zh: "  再按 enter 把文件回滚到 %s (对话不受影响)",
	},
	"tui.rewind.title": {
		En: "Rewind code · checkpoints",
		Zh: "回滚代码 · 检查点",
	},

	// ---- CLI (REPL / print) ----
	"cli.approval.sandboxPrompt": {
		En: "\n── sandbox escalation ──\n%s  %s → %s\n%s\nReason: %s\n[y] this time only  [a] for this session  [n] deny\n> ",
		Zh: "\n── sandbox escalation ──\n%s  %s → %s\n%s\n理由:%s\n[y] 仅本次  [a] 本会话生效  [n] 拒绝\n> ",
	},
	"cli.checkpoint.none": {
		En: "No checkpoints yet — AI edit/write changes are archived automatically (can be disabled in /config)",
		Zh: "暂无检查点 — AI 的 edit/write 修改会自动留档(可用 /config 关闭)",
	},
	"cli.checkpoint.list": {
		En: "Checkpoints (new→old):\n%s\n/rewind <id> or /rewind last restores files to that point (files only, the conversation is unaffected)",
		Zh: "检查点 (新→旧):\n%s\n/rewind <id> 或 /rewind last 恢复文件到该时点(仅回滚文件,不回滚对话)",
	},
	"cli.checkpoint.noneShort": {
		En: "No checkpoints",
		Zh: "暂无检查点",
	},
	"cli.checkpoint.restored": {
		En: "Rolled back to checkpoint %s · restored %d files (the conversation is unaffected)",
		Zh: "已回滚到检查点 %s · 恢复 %d 个文件(对话不受影响)",
	},
	"cli.memory.autoOff": {
		En: "Auto Memory is off (/config)",
		Zh: "Auto Memory 已关闭 (/config)",
	},
	"cli.memory.nothingToExtract": {
		En: "(no conversation to extract from)",
		Zh: "(会话没有可提取的对话)",
	},
	"cli.memory.extracted": {
		En: "Memory extraction done: %d added · %d updated · %d deleted",
		Zh: "记忆提取完成: 新增 %d · 更新 %d · 删除 %d",
	},
	"cli.memory.none": {
		En: "No memories — enable Memory Auto Extraction (/config) to extract at session end, or trigger manually with /memory extract",
		Zh: "暂无记忆 — 开启 Memory Auto Extraction (/config) 会在会话结束时自动提取,或 /memory extract 手动触发",
	},
	"cli.memory.selectedMark": {
		En: " · selected",
		Zh: " · 已选",
	},
	"cli.memory.list": {
		En: "Memories (%d):\n%s\nWith Memory Relevance on, only “selected” entries inject · manage with /memory in the TUI",
		Zh: "记忆 (%d):\n%s\n开启 Memory Relevance 后仅注入“已选”条目 · TUI 里 /memory 管理",
	},
	"cli.print.sessionSaved": {
		En: "Session saved %s · resume: scode --resume %s",
		Zh: "会话已保存 %s · 恢复: scode --resume %s",
	},
	"cli.print.modelLine": {
		En: "%s / %s — switch: /model <name> (in the TUI, bare /model opens the picker)",
		Zh: "%s / %s — 切换: /model <名称> (TUI 中直接 /model 弹出选择框)",
	},
	"cli.print.catalogHeader": {
		En: "Preset model catalog (in the TUI, /models opens the interactive flow: vendor → key → model → confirm):",
		Zh: "预设模型目录 (TUI 中 /models 弹出交互配置: 厂商 → Key → 模型 → 确认):",
	},
	"cli.print.spec": {
		En: "context %d · max output %d",
		Zh: "上下文 %d · 最大输出 %d",
	},
	"cli.print.specReasoning": {
		En: " · reasoning %s",
		Zh: " · 推理 %s",
	},
	"cli.print.newSession": {
		En: "New session %s started — resume the previous one: scode --resume %s",
		Zh: "新会话 %s 已开始 — 原会话恢复: scode --resume %s",
	},
	"cli.run.pruned": {
		En: "sessions: pruned %d session log(s) older than %d days\n",
		Zh: "sessions: 已清理 %d 个超过 %d 天的会话日志\n",
	},
	"cli.run.builtinMark": {
		En: " (built-in)",
		Zh: " (内置)",
	},
	"cli.run.agentRow": {
		En: "  %s · %s · effort %s",
		Zh: "  %s · %s · 推理 %s",
	},
	"cli.run.agentsList": {
		En: "Sub-agents (%d):\n%s\nModel format provider:model (empty = inherit the session model) · delegate via the task tool; sub-agents are read-only and fully isolated",
		Zh: "子代理 (%d):\n%s\n模型格式 provider:model(留空继承会话模型)· task 工具委派,子代理只读且完全隔离",
	},
	"cli.run.configList": {
		En: "Configuration (in the TUI, /config opens the interactive panel):\n  Auto-compact context: %s\n  Log retention period: %s\n  Auto Memory: %s\n  Typed Memory: %s\n  Memory Relevance: %s\n  Memory Auto Extraction: %s\n  Rewind code (checkpoint rollback): %s\n  Clipboard image reading (ctrl+v): %s (single read on demand, zero calls at idle; when off the clipboard is never touched)\n  Interface language: %s\n  Web search engine: %s",
		Zh: "配置 (TUI 里 /config 打开面板交互修改):\n  上下文自动压缩: %s\n  日志清理周期: %s\n  Auto Memory(自动记忆): %s\n  Typed Memory(分类记忆): %s\n  Memory Relevance(记忆相关性选择): %s\n  Memory Auto Extraction(自动提取记忆): %s\n  Rewind code(检查点回滚): %s\n  剪贴板图片读取(ctrl+v): %s (按需单次读取,空闲时零调用;关闭后完全不触剪贴板)\n  界面语言: %s\n  网络搜索引擎: %s",
	},
	"cli.run.mcpState": {
		En: "%s · %d tools",
		Zh: "%s · %d 工具",
	},
	"cli.run.usage": {
		En: "usage: input %s · output %s · cache read %s · cache write %s",
		Zh: "用量: 输入 %s · 输出 %s · 缓存读 %s · 缓存写 %s",
	},
	"cli.run.usageCache1h": {
		En: " · cache write(1h) %s",
		Zh: " · 缓存写(1h) %s",
	},
	"cli.run.usageCost": {
		En: " | cost $%.4f",
		Zh: " | 成本 $%.4f",
	},
	"cli.run.usageCtx": {
		En: "\ncontext: %s / %s (%.1f%%)",
		Zh: "\n上下文: %s / %s (%.1f%%)",
	},
	"cli.run.usageCtxUnknown": {
		En: "\ncontext: %s (window unknown)",
		Zh: "\n上下文: %s (窗口未知)",
	},
	"cli.run.usageCacheHit": {
		En: " · cache hit %d%%",
		Zh: " · 缓存命中 %d%%",
	},
	"cli.run.usageDetail": {
		En: "\nbreakdown: messages %s · system prompt %s · system tools %s · skills %s · MCP tools %s · other %s",
		Zh: "\n明细: 消息 %s · 系统提示词 %s · 系统工具 %s · 技能 %s · MCP 工具 %s · 其他 %s",
	},
}
