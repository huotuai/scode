# scode

基于 [pi](https://pi.dev) 架构的精简 Go 编程助手。独立迭代,不追随上游。

## 设计支柱

1. **前缀字节级稳定**(缓存命中率的根本):transcript 只追加;系统提示词与初始工具声明在首条
   system 消息(`ts=0`);易变内容(日期、git 状态)禁止进提示词,通过 bash 工具环境变量传递。
2. **双层消息模型**:agent 全程使用 provider 中立的 `Message`,只在 provider 边界转换 wire format。
3. **错误进流**:LLM 调用失败编码为流内 `error` 事件 + `stopReason=error` 终止消息,不 panic。
4. **存储与视图分离**(M4):JSONL 会话只追加、**只存对话**。会话文件采用 pi 的 v2 树形格式:
   header(`{"type":"session",...}`)+ 逐行 entry(`{"type":"message","id","parentId","timestamp","message":{...}}`
   / `{"type":"compaction",...,"firstKeptEntryId"}`),entry 以 id/parentId 成树,上下文取叶到根的 PATH;
   系统提示词和工具声明不落盘,每次加载时从当前构建重建,在内存中作为 transcript 首条 system
   消息——AGENTS.md 编辑与工具 schema 变更在 resume 时自然生效。旧版(v1)文件读取时自动迁移。
   Agent 钩子面与 pi 的 AgentLoopConfig 对齐:before/afterToolCall、transformContext、finishTurn
   (end/continue 决策)、prepareRequest、prepareNextTurn。

## 里程碑

- [x] M0 消息模型 + 前缀稳定性测试
- [x] M1 双协议 provider(Anthropic + OpenAI 兼容)
- [x] M2 agent 循环 + 工具接口/钩子
- [x] M3 核心工具集(read/write/edit/bash/grep/find/ls)
- [x] M4 JSONL 会话 + 系统提示词 + 压缩 + 读时投影
- [x] M5 print 模式 + REPL
- [x] M6 TUI(bubbletea)
- [x] M7 MCP(dsh mcp-client 设计 + 官方 Go SDK;Bedrock 暂缓:传输层选择未定)
- [x] skills(pi 的 Agent Skills 机制)

## 上下文管理(压缩)

与 pi 对齐的部分压缩:投影上下文超过阈值(settings.json `compactionTokens`,默认 80k,负数关闭;
已知 `contextWindow` 时为窗口−16384)时,发起一次性摘要调用,
落一条 compaction 标记到会话文件。摘要调用的缓存按协议分:自动缓存 provider
(DeepSeek/GLM/OpenAI/Gemini)**复用主对话前缀**(折叠系统提示词+当前工具集+切点前投影,
KV 直接命中,dsh 的 summarize 形态);Anthropic 显式断点协议维持 pi 的禁用缓存策略。**只摘要切点之前的旧历史,最近 ~`keepRecentTokens`(默认
20000,负数=全量压缩)的对话原文保留**——标记记录 `firstKeptIndex` 与 `tokensBefore`,投影 =
摘要 + 保留段 + 标记后消息。第二次起压缩走增量更新流(旧摘要参与合并:前缀复用路径下
它已在投影上下文中,序列化路径下经 `<previous-summary>` 标签)。
触发估算 = 最后一条有效 usage(跳过 error/aborted/零 usage)+ 其后消息的 chars/4 估算。

溢出恢复:provider 返回 context-overflow 错误时,自动压缩一次并重试当轮(仅一次)。

触发估算基于读时投影——存储的历史永不改写,read/modified 文件清单跨代传递。
`--resume` 从投影恢复,提示词与工具声明按当前构建重新生成(旧版会话文件中的 system 行会被忽略)。

## 沙箱(dsh 方案移植)

文件效果沙箱，模式词汇与 dsh 对齐:`read-only`(禁写)/ `workspace-write`(仅工作区+临时目录可写)/
`danger-full-access`(不限制)。默认收敛在 **`workspace-write`**(与 dsh 默认档一致:工作区+临时区可写，
越界写需人工审批;settings.json 可改)。

- **策略层** `internal/sandbox`:配置默认(settings.json `sandbox.mode`,拼错即报错)、会话级切换
  (`sandbox` 会话事件，回放恢复)、模型提示注入(受限模式写入 system section 增量)
- **文件工具围栏**:write/edit 的变异目标先做 canonical 化(realpath **最深已存在祖先**再回拼缺失后缀,
  Windows 用 handle final path 以覆盖 junction/reparse point,与 dsh 的 `fsio.resolve` 同语义)
  + 包含性检查
  (词法快路径 + os.SameFile 身份兑底,覆盖 Windows 8.3/大小写别名),拒绝时返回
  `[sandbox: file access denied under <mode> mode]` + 升权提示;读取永不限制
- **bash fail-closed**:受限模式下无 runner 后端时返回 `SANDBOX_UNAVAILABLE`,绝不裸跑;
  **策略缺失/模式未解析同样拒绝**(`ResolvePolicy` 对 `nil` 或无效模式直接报错,只有显式
  `danger-full-access` 才不设限——缺省即放行是不成立的)
- **升权流程**:bash/write/edit schema 固定携带 `sandbox_permissions` + `justification`(无条件注册——
  模式切换不改变工具声明字节,缓存前缀稳定);受限拒绝返回 `[sandbox: …]` 标记对,模型按提示带理由
  重试一次,经 `approval/request`(kind `sandbox`)人工审批后单次放宽;`allow_session` 同时切换会话模式
- **Windows runner**(内核级):WRITE_RESTRICTED 受限令牌 + 能力 SID ACL + Low 完整性标签 +
  FILE_DELETE_CHILD 拒绝 + KILL_ON_JOB_CLOSE Job;孙进程继承约束;授权幂等(精确 ACE 跳过)。
  工作区授权是**常驻**的(设计同 dsh,会留在目录上)但本身惰性——只有持有派生能力 SID 的
  受限令牌能用它;运维可用 `sandbox.RevokeWorkspaceGrant(workspace)` 显式回收
  (Everyone 的 FILE_DELETE_CHILD 拒绝 ACE 属**收紧**项,有意保留)
- **Linux runner**:优先 `bwrap`(mount profile:整根只读绑定 +私有 tmpfs `/tmp` +工作区 rw 绑定,
  `--die-with-parent`),不可用时回退到内置 **landlock 启动器**(本二进制自我 re-exec:
  `scode __sandbox_landlock --ro … --rw … -- 命令`,裸系统调用建 allow-list ruleset +`no_new_privs`,
  按运行内核 ABI 降级并上报 partial);两者皆不可用则 fail-closed
- **macOS runner**:`sandbox-exec -p <SBPL>` —— 默认放行、`deny file-write*`,仅按 subpath 放行
  可写根(与文件围栏共享同一套根推导,避免“工具能写而 bash 不能”的不对称)
- **desktop**:composer 沙箱切换器(三档),`session/sandbox` RPC,升权审批卡(批准一次/本会话生效/拒绝)

```jsonc
// settings.json — 部署默认(会话可用 RPC 覆盖)
{ "sandbox": { "mode": "workspace-write" } }
```

runner 后端(OS 级强制)均已落地:Windows ACL 受限令牌 / Linux bwrap→landlock / macOS seatbelt。
无可用后端的平台 fail-closed(绝不裸跑)。

## 桌面客户端(Electron)

`desktop/`:主进程 spawn `scode serve`(stdio JSON-RPC),渲染进程为 React + TypeScript +
Vite(`desktop/renderer/`),经 preload 桥访问全部能力——会话管理、流式对话(Markdown 渲染)、
思考折叠块、工具卡片(diff/结果可展开)、审批弹窗(允许一次/拒绝/本会话/本项目)、计划评审卡片、
plan 模式切换、compact、插话纠偏、模型切换(composer 下拉/自定义 ID)、模型设置(左下 ⚙,
增删改查 settings.providers,即时持久化到 settings.json)、项目目录选择(切目录=重启子进程,
会话绑定 cwd)、会话删除/空会话清理、顶栏上下文占用百分比与缓存命中率统计。界面为左右布局:左侧历史会话列表(可折叠)+ 模型设置/主题切换/工作区,
右侧顶栏(状态/成本)+ 消息流 + composer;亮/暗双主题(CSS 变量,默认亮色,localStorage 持久化)。

```sh
cd desktop && npm install
npm run dev    # Vite 热更新 + Electron 开发模式
npm run build && npm start   # 生产模式(加载 dist-renderer/)
```

后端二进制解析顺序:`SCODE_BIN` 环境变量 > `dist/scode.exe` > PATH。渲染进程是
纯展示层:状态真相全在 serve 进程与会话文件。

## serve 模式(桌面客户端后端)

`scode serve` 把 agent 暴露为无头会话服务:**stdio NDJSON JSON-RPC 2.0 唯一传输**
(stdout 只走帧,日志全 stderr),首条 `initialize` 握手交换 `protocolVersion`。
Electron 等桌面壳 spawn 子进程即得全部能力:

- **client→server 方法**:`session/create|resume|list|fork|delete|prune|prompt|steer|cancel|compact|cost|usage|mode|state|sandbox`
- **server→client 通知**:`session/event`(llm delta / tool_start|tool_end / message / turn_end /
  agent_end / run_error;事件 schema 与 provider 中立 Message 对齐,系统提示词不过协议)
- **server→client 反向请求**:`approval/request`(`kind: tool|plan`,带工具名/参数/命中规则/
  精确规则/计划全文)→ 客户端应答 `{decision: allow|deny|allow_session|allow_project|approve|revise,
  feedback?}`(ACP 的 session/request_permission 同款形态)
- 会话状态视图 `{mode, pending, running, cost}`;会话文件仍是唯一真相源;resume 已活会话
  会先释放旧句柄(会话独占)

## REPL 命令

| 命令 | 作用 |
|---|---|
| `/compact [指令]` | 立即压缩(可带自定义摘要焦点) |
| `/plan [off]` | 进入/退出计划模式(只读调研 → 计划评审门 → 批准后执行) |
| `/cost` | 本会话累计 token 与花费(配置了 pricing 时) |
| `/fork [N]` | 克隆会话(可只保留前 N 条目)——实验不污染原会话 |
| `/model` | 显示当前模型(desktop 头部选择器可实时切换 provider/model) |
| `/sessions` | 列出本目录会话 |
| `/skill:名字 [参数]` | 显式调用 skill(pi 的 _expandSkillCommand):skill 文件正文以 `<skill>` 块注入用户消息;运行中输入也会展开 |
| `/exit` | 退出 |

花费核算:settings.json 里给 provider 配 `pricing`(USD/百万 token)后,每条消息写入 costUSD、`/cost` 显示累计。

## 图片输入

三种方式把图片带进对话(TUI;路径方式对 REPL/print 同样生效):

1. **路径**:prompt 里直接写图片文件路径(绝对/相对/`~`,含空格的路径加引号;拖拽文件进终端也会产生带引号路径)。提交时按扩展名初筛、按内容嗅探(同 read 工具的 DetectImageMIME),命中即作为图片块附加,文本原样保留。
2. **截图 / 粘贴**:`ctrl+v` 先探剪贴板——Windows 上支持截图工具留下的位图(CF_DIBV5/CF_DIB,重编码为 PNG)、原始 PNG 格式、以及资源管理器里复制的图片文件(CF_HDROP);Linux 用 `wl-paste`/`xclip`、macOS 用 `pngpaste` 兑底。命中后输入框出现占位符 `[图片#N]`,随下一条消息发送;**删除占位符即撤销附件**。剪贴板无图片时 `ctrl+v` 退化为普通文本粘贴。
3. 运行中提交:steer 只带文本,图片保留到下一次提问(界面有提示)。

限制:单张原始 ≤10MB;支持 PNG/JPEG/GIF/WebP/BMP(与 provider 网关一致);模型 `imageInput=false` 时按既有约定降级为占位文本。

## skills(Agent Skills)

与 pi 的 skills 机制对齐:**发现而不预载**——skill 是带 YAML frontmatter(`name`/`description`/
`disable-model-invocation`)的 markdown 文件;系统提示词只列 `<available_skills>`(名字/描述/路径),
模型按需用 read 工具加载正文。

发现位置(pi 的优先级,先占胜,项目级压用户级):

1. `{cwd}/.scode/skills`(项目,根级裸 .md 也计)
2. `{cwd}/.agents/skills` 及祖先至 git 根(仅子目录内容)
3. `~/.scode/skills`(用户)
4. `~/.agents/skills`(用户)
5. settings.json `skills` 数组(额外路径)
6. `--skill <路径>`(可重复;文件或目录)

规则(pi 对齐):目录含 `SKILL.md` 即 skill 根并停止递归;忽略 `.` 开头与 `node_modules`;
`.gitignore`/`.ignore`/`.fdignore` 逐级生效;name 缺省回退父目录名(规范:≤64 字符、小写 a-z0-9-、
无首末/连续连字符);description 必填 ≤1024 字符;`disable-model-invocation: true` 的不进提示词,只能
`/skill:名字` 显式调用。`--no-skills` 关闭默认发现(显式 `--skill` 仍加载)。

## 计划模式(plan mode)

语义对齐 deepseek-harness 的 plan-mode，唯一偏离=**写操作硬拒绝**(Claude Code 同款;
dsh 只靠提示词引导)。`--plan` 启动或 `/plan` 进入:

- write/edit 拒绝,唯 `.scode/plan/**` 可写(模型可按需存计划草稿);
- bash 可执行但写特征命令拒绝(重定向/rm|mv|cp/tee/dd/chmod/管道入 shell/git 变更子命令——
  壳级启发式,防君子不防小人,硬保证等 OS 沙箱);
- 其余走权限规则(MCP 工具仍受 server defaultPolicy 约束,默认 ask);
- 调研完成后模型调 `exit_plan_mode` 提交完整计划(# 标题开头的 markdown,常驻工具,
  工具目录全程不变)。评审:`y` 批准(退出计划模式,下步开执行);其他任何输入作为
  反馈打回(模型带着反馈继续规划);`/plan off` 直接退出。

机制:模式切换落 append-only `mode` entry(末值胜,resume/fork 自动恢复;审批挂起状态
不跨进程恢复,resume 后模型重新提交),提示词经 transcript section delta 注入/移除,
前缀缓存无损。REPL 提示符在计划模式下显示 `[plan]> `。

## MCP(外部工具服务器)

与 deepseek-harness 的 mcp-client 设计对齐(协议层委托官方 Go SDK):在 `~/.scode/mcp.json` 配置
server,其工具以 `mcp__<server>__<tool>` 名注册进工具表,模型像调内置工具一样调用。

```json
{
  "mcpServers": {
    "github": {"transport": "stdio", "command": "npx", "args": ["-y", "@modelcontextprotocol/server-github"], "env": {"GITHUB_TOKEN": "..."}},
    "web": {"transport": "streamable-http", "url": "http://localhost:3000/mcp", "headers": {"Authorization": "Bearer ..."}}
  }
}
```

| 字段 | 默认 | 说明 |
|---|---|---|
| `transport` | 必填 | `stdio`(本地子进程)或 `streamable-http` |
| `command`/`args`/`env`/`cwd` | — | stdio:可执行文件、参数、追加环境变量、工作目录。子进程环境会清洗凭证形状(`KEY\|PASSWORD\|SECRET\|TOKEN`)与 `SCODE_*` 变量,显式 `env` 在清洗后合并 |
| `url`/`headers` | — | streamable-http:端点与附加请求头 |
| `toolCallTimeoutMs` | 60000 | 单次工具调用超时 |
| `failOnStartupError` | false | true 时初始连接失败直接拒绝启动;false 时后台重连 |
| `maxInstructionBytes` | 32768 | server instructions 注入提示词的字节上限 |
| `reconnect` | {enabled:true, initialDelayMs:500, maxDelayMs:30000, maxAttempts:10} | 断线指数退避;稳定运行超 maxDelayMs 重置预算;连续失败 maxAttempts 次后放弃并注销该 server 全部工具 |
| `defaultPolicy` | `ask` | 权限引擎兜底规则(`allow`/`ask`/`deny`),作用于 `mcp__<server>__*` 全部工具 |

行为(dsh 对齐):启动时并行连接并在首次尝试后注册发现的工具(进初始声明);运行中工具列表变化
(重连/list_changed 通知)以 transcript system delta 表达,前缀缓存不受损;server instructions 进系统
提示词 `mcp` 节;断线期间工具调用报“server is disconnected”。不支持:resources/prompts 订阅、
task-required 工具(dsh 同样不支持)。

## settings.json 的 permissions 字段(权限引擎)

每个工具调用过唯一咽喉点，按 **deny > ask > allow** 求值(Claude Code 式规则语法);
未命中走 `defaultMode`(`default`=放行，保持现状行为;`bypass`=忽略全部规则)。
项目级 `{cwd}/.scode/settings.json` 的 `permissions` 节追加在用户级之后。

```json
{
  "permissions": {
    "defaultMode": "default",
    "allow": ["bash(git status)", "bash(git diff:*)", "read"],
    "ask":   ["bash(git commit:*)"],
    "deny":  ["bash(rm -rf /)", "write(.git/**)"]
  }
}
```

| 规则形态 | 语义 |
|---|---|
| `read` / `bash` | 名称规则：覆盖该工具全部调用 |
| `bash(git status)` | 精确命令，或词边界前缀(`git status -s` 命中,`git statuses` 不) |
| `bash(git diff:*)` | 前缀后任意续接 |
| `write(.scode/plan/**)` | 路径 glob(doublestar,`**` 跨目录);cwd 相对与绝对形式都试 |
| `mcp__github__*` | 工具名 glob(MCP server 维度) |

`ask` 命中的处置:REPL 弹出审批(`y` 一次 / `n` 拒绝 / `a` 本会话总是允许 /
`p` 写入项目 `.scode/settings.json` 永久允许——落盘为**精确规则，不泛化**;不提供写用户级的
快捷项);print 等非交互运行自动拒绝并在工具结果里说明。审批期间其他输入照常 steer。
deny 命中不执行，返回可读错误结果(模型可自我纠正);拦截在权限层，工具保持注册。MCP server
的信任级在 mcp.json 里按 server 配 `defaultPolicy`(默认 `ask`),展开为 `mcp__<server>__*`
兜底规则，显式 settings 规则优先。

## settings.json 的 provider 字段

| 字段 | 作用 |
|---|---|
| `baseUrl` / `apiKey` / `model` | 端点与默认模型(环境变量可覆盖 key/baseUrl) |
| `contextWindow` | 模型上下文窗口,决定压缩阈值 |
| `maxTokens` | 模型输出上限(pi 模型目录的 maxTokens):默认 max_tokens,并钳制 thinking 预算 |
| `reasoning` | 模型是否支持思考(pi 的 model.reasoning);responses/google 适配器据此决定是否发送 reasoning/thinkingConfig 参数,默认 false |
| `imageInput` | 模型是否接受图片输入(pi 的 model.input);默认 anthropic=true、其他=false。为 false 时图片降级为占位文本(pi 的 downgradeUnsupportedImages) |
| `pricing` | USD/百万 token 费率,用于花费核算 |
| `protocol` | 自定义 provider 键的协议(anthropic / openai-compat / openai-responses / azure-openai-responses / google);缺省 openai-compat |

`providers` 下除内置协议名外的任意键都是**自定义 provider 别名**(如 `"kimi"`)。别名默认按
openai-compat 协议发起请求,可用 `protocol` 指定其它协议;`/model` 切换、desktop 头部模型选择器
以及会话恢复都按别名记忆。

`defaultProvider` / `defaultModel`:**新会话**未指定 provider/model 时的回退。`defaultProvider`
必须是 `providers` 下存在的键(内置协议名或自定义别名,如 `"kimi"`);对自定义别名而言,该档位
自己的 `model` 优先于 `defaultModel`。手工修改时注意别把 `defaultProvider` 指向不存在的键
(会在创建会话时报 `unknown provider`),desktop 的"模型设置"里点**设为默认**可直接写这两个字段。

## provider 一览

| provider | 协议 | key 环境变量 | 默认模型 |
|---|---|---|---|
| `anthropic` | Anthropic Messages | `ANTHROPIC_API_KEY` | claude-sonnet-4-5 |
| `openai-compat` | chat/completions(GLM/DeepSeek/relay 等) | `OPENAI_COMPAT_API_KEY` / `OPENAI_API_KEY` | gpt-5.2 |
| `openai-responses` | OpenAI Responses API | `OPENAI_API_KEY` | gpt-5.5 |
| `azure-openai-responses`(别名 `azure`) | Azure OpenAI Responses | `AZURE_OPENAI_API_KEY` | gpt-5.4 |
| `google` | Gemini generateContent | `GEMINI_API_KEY` | gemini-3.1-pro-preview |

Azure 端点解析(pi 的 resolveAzureConfig):`baseUrl` 设置 > `AZURE_OPENAI_BASE_URL` >
`AZURE_OPENAI_RESOURCE_NAME`(拼 `https://<name>.openai.azure.com/openai/v1`);Azure 域名自动归一到
`/openai/v1`。部署名默认 = 模型 id,可用 `AZURE_OPENAI_DEPLOYMENT_NAME_MAP="model=deployment,..."` 映射;
`AZURE_OPENAI_API_VERSION` 默认 `v1`。

顶层 `retry` 节(pi 的 settings.retry):`enabled`(默认 true)、`maxRetries`(默认 3)、
`baseDelayMs`(默认 2000,指数退避 2s/4s/8s)、`maxAgentDelayMs`(默认 60000 封顶)。
仅白名单内的瞬时错误(过载/429/5xx/网络/流中断)会重试,quota/billing 类与未识别错误 fail-fast。
`retry.provider` 节(pi 的传输层):`timeoutMs`(请求超时,默认 600000)、`maxRetries`(传输重试次数,
默认 3)、`maxRetryDelayMs`(退避封顶,默认 60000)。

## 使用

```sh
# OpenAI 兼容端点(GLM 等)
export OPENAI_COMPAT_API_KEY=...
export SCODE_OPENAI_BASE_URL=https://open.bigmodel.cn/api/paas/v4
scode -p --provider openai-compat --model glm-4.6 "任务描述"

# Anthropic 协议(GLM Anthropic 兼容端点 / Claude)
export ANTHROPIC_API_KEY=...
export SCODE_ANTHROPIC_BASE_URL=...   # 可选
scode            # TUI(默认交互模式;bubbletea v2)
scode --repl     # 旧行式 REPL(运行中输入普通文本=插话纠偏,输入 /命令=本轮结束后执行)

scode --sessions # 列出当前目录的会话
scode --resume <id> -p "继续"
```

TUI 键位:Enter 发送,ctrl+j/shift+enter 换行;运行中输入自动转为 steering;esc 中断当前运行(第一次 ctrl+c 同),空闲时 ctrl+c 退出;pgup/pgdn 滚动;审批/plan review/沙箱升权以输入框上方的决策框呈现(y/n/a/p 直答,plan review 支持自由文本反馈)。stderr 诊断(MCP/沙箱/压缩通知)渲染进消息流而非破坏 alt screen。

诊断:`SCODE_DEBUG_REQ=1` 转储请求体,`SCODE_DEBUG_SSE=1` 转储流式响应。

## 开发

```sh
go build ./...
go test ./...
```

架构参考:pi v0.87.1 快照(`E:\ai\harness\pi`),关键对照见各源文件注释。
