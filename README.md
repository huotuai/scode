# scode

**scode** 是一个用 Go 编写的 AI 编程助手：一个二进制，四种形态 —— 交互式 TUI、一次性命令（print 模式）、行式 REPL、以及驱动桌面客户端的无头服务（`scode serve`）。Windows 为主力平台，同时支持 Linux / macOS。

仓库：https://github.com/huotuai/scode

## 特性一览

- **多协议模型接入**：Anthropic Messages、OpenAI chat/completions（GLM/DeepSeek/各类中转）、OpenAI Responses、Azure OpenAI Responses、Google Gemini，五种协议自由切换，支持自定义 provider 别名
- **交互式 TUI**：bubbletea 构建，流式渲染、工具调用卡片、运行中插话纠偏（steering）、审批决策框、计划评审
- **桌面客户端**：Electron + React 19 + TypeScript，会话管理、Markdown 渲染、diff 展开、模型设置面板、亮/暗双主题
- **计划模式**：只读调研 → 计划评审门 → 批准后才执行，写操作硬拒绝
- **文件效果沙箱**：`read-only` / `workspace-write` / `danger-full-access` 三档，OS 级强制（Windows 受限令牌 ACL / Linux bwrap→landlock / macOS seatbelt），无后端时 fail-closed 绝不裸跑
- **权限引擎**：Claude Code 式规则语法，`deny > ask > allow`，支持命令前缀与路径 glob
- **MCP 外部工具**：接入任意 MCP server，工具以 `mcp__<server>__<tool>` 注册，模型像调内置工具一样调用
- **Agent Skills**：发现而不预载，模型按需加载 skill 正文
- **图片输入**：路径引用、剪贴板截图粘贴（`ctrl+v`）、拖拽，自动降级为占位文本当模型不支持
- **会话管理**：JSONL 只追加存储、`--resume` 恢复、`/fork` 克隆实验、`/compact` 上下文压缩、跨会话记忆
- **自更新**：`scode update` 一键升级，TUI 启动时自动提示新版本

## 架构优势

### 1. 前缀字节级稳定 → KV 缓存命中率最大化

transcript 只追加，系统提示词与工具声明固定在首条 system 消息；日期、git 状态等易变内容一律走 bash 工具环境变量，绝不进提示词。上下文压缩、工具列表变化（MCP 重连）、计划模式切换全部以"增量 section"表达而非改写历史 —— 长会话的每一次请求都能命中 provider 侧 KV 缓存，直接转化为**速度与成本优势**。

### 2. 双层消息模型 → provider 中立

agent 循环全程使用统一的中立 `Message`/`Event` 模型，wire format 转换只发生在 provider 边界的协议适配层。新增一个模型协议不需要触碰 agent 逻辑。

### 3. 错误进流 → 永不 panic

LLM 调用失败编码为流内 `error` 事件 + `stopReason=error` 终止消息，与会话历史同样落盘。任何失败现场都可 resume、可审计。

### 4. 存储与视图分离

JSONL 会话文件**只存对话**（v2 树形格式，entry 以 id/parentId 成树，上下文取叶到根路径）；系统提示词和工具声明不落盘，加载时从当前构建重建 —— 升级二进制、编辑 AGENTS.md、变更工具 schema 后 resume 旧会话，自然生效。压缩是"读时投影"：历史永不改写，摘要与保留段在加载时合成。

### 5. 安全纵深：权限规则 + 沙箱双层

- **权限引擎**是第一道：每个工具调用过唯一咽喉点，按 `deny > ask > allow` 求值，`ask` 命中弹人工审批（允许一次/本会话/写入项目配置）
- **沙箱**是第二道（OS 级强制，不依赖模型自觉）：write/edit 目标先做路径 canonical 化再 containment 检查；bash 在受限模式下由 OS runner 执行，无 runner 或策略未解析时**直接拒绝**；越界操作返回升权提示，模型带理由重试一次，经人工审批后单次放宽

### 6. serve 模式：一个后端，多种前端

`scode serve` 把 agent 暴露为 stdio NDJSON JSON-RPC 2.0 会话服务（stdout 只走帧，日志全 stderr），桌面客户端、IDE 插件或任何能 spawn 子进程的程序都能获得全部能力：会话 CRUD、流式事件、审批反向请求、沙箱切换。**会话文件是唯一真相源**，前端只是展示层。

## 安装

### 下载预编译二进制（Windows）

从 [Releases](https://github.com/huotuai/scode/releases) 下载 `scode-windows-amd64.zip`（或 arm64），解压后将目录加入 PATH。

之后升级只需：

```powershell
scode update          # 检查并自更新到最新 release（自动校验 sha256）
scode update -check   # 只检查不更新
scode version         # 显示版本号 + 是否最新
```

### 源码构建

```sh
git clone https://github.com/huotuai/scode.git
cd scode
go build -ldflags "-X main.version=$(git rev-parse --short HEAD)" -o dist/scode.exe ./cmd/scode   # Windows
go build -ldflags "-X main.version=$(git rev-parse --short HEAD)" -o dist/scode ./cmd/scode        # Linux / macOS
```

## 快速上手

配置模型（二选一）：

```powershell
# 方式一：环境变量（以 OpenAI 兼容端点为例）
$env:OPENAI_COMPAT_API_KEY = "..."
$env:SCODE_OPENAI_BASE_URL = "https://open.bigmodel.cn/api/paas/v4"

# 方式二：~/.scode/settings.json（支持多 provider 档案、自定义别名）
# { "providers": { "glm": { "protocol": "openai-compat", "baseUrl": "...", "apiKey": "...", "model": "glm-4.6" } },
#   "defaultProvider": "glm" }
```

运行：

```sh
scode                        # TUI（默认交互模式）
scode -p "任务描述"           # 一次性执行并退出
scode --repl                 # 行式 REPL
scode --resume <id>          # 恢复会话
scode --plan                 # 计划模式启动（只读调研，批准后执行）
scode --sessions             # 列出当前目录的会话
```

TUI 键位：`Enter` 发送，`ctrl+j` 换行；运行中输入自动转为插话纠偏；`esc` 中断当前运行；`pgup/pgdn` 滚动。

## 常用命令（TUI / REPL 内）

| 命令 | 作用 |
|---|---|
| `/compact [指令]` | 立即压缩上下文（可带自定义摘要焦点） |
| `/plan [off]` | 进入/退出计划模式 |
| `/cost` | 本会话累计 token 与花费 |
| `/fork [N]` | 克隆会话（可只保留前 N 条），实验不污染原会话 |
| `/model` | 显示/切换当前模型 |
| `/sessions` | 列出本目录会话 |
| `/skill:名字 [参数]` | 显式调用 skill |
| `/exit` | 退出 |

## 配置（~/.scode/settings.json）

```jsonc
{
  "defaultProvider": "glm",          // 新会话默认 provider
  "defaultModel": "glm-4.6",
  "providers": {
    "glm": {
      "protocol": "openai-compat",   // anthropic | openai-compat | openai-responses | azure-openai-responses | google
      "baseUrl": "https://open.bigmodel.cn/api/paas/v4",
      "apiKey": "...",
      "model": "glm-4.6",
      "contextWindow": 200000,       // 决定压缩阈值
      "reasoning": true,             // 思考模型
      "imageInput": false,           // 不接受图片时自动降级
      "pricing": { "input": 0.6, "output": 2.2, "cacheRead": 0.06 }  // USD/百万 token，用于 /cost
    }
  },
  "permissions": {                    // 权限引擎：deny > ask > allow
    "allow": ["bash(git status)", "bash(git diff:*)", "read"],
    "ask":   ["bash(git commit:*)"],
    "deny":  ["write(.git/**)"]
  },
  "sandbox": { "mode": "workspace-write" },  // read-only | workspace-write | danger-full-access
  "language": "zh",                   // 界面语言：zh | en（缺省跟随系统；SCODE_LANG 环境变量优先）
  "updateCheck": true,                // TUI 启动时的新版本提示（默认开）
  "updateRepo": "huotuai/scode"       // 更新源（默认已内置）
}
```

MCP server 配置在 `~/.scode/mcp.json`：

```json
{
  "mcpServers": {
    "github": { "transport": "stdio", "command": "npx", "args": ["-y", "@modelcontextprotocol/server-github"] },
    "web":    { "transport": "streamable-http", "url": "http://localhost:3000/mcp" }
  }
}
```

内置 provider 与默认模型：

| provider | 协议 | key 环境变量 | 默认模型 |
|---|---|---|---|
| `anthropic` | Anthropic Messages | `ANTHROPIC_API_KEY` | claude-sonnet-4-5 |
| `openai-compat` | chat/completions | `OPENAI_COMPAT_API_KEY` / `OPENAI_API_KEY` | gpt-5.2 |
| `openai-responses` | OpenAI Responses | `OPENAI_API_KEY` | gpt-5.5 |
| `azure-openai-responses` | Azure OpenAI Responses | `AZURE_OPENAI_API_KEY` | gpt-5.4 |
| `google` | Gemini generateContent | `GEMINI_API_KEY` | gemini-3.1-pro-preview |

## 桌面客户端

```sh
cd desktop && npm install
npm run dev                  # Vite 热更新 + Electron 开发模式
npm run build && npm start   # 生产模式
```

主进程 spawn `scode serve` 作为后端（二进制解析顺序：`SCODE_BIN` 环境变量 > `dist/scode.exe` > PATH），渲染进程经 preload 桥访问全部能力：会话列表、流式对话、工具 diff 卡片、审批弹窗、模型设置、沙箱切换、主题切换。

## 项目结构

```
cmd/scode        入口：flag 解析与 serve / TUI / REPL / print 分发
internal/llm     provider 中立消息模型 + 五种协议适配层
internal/agent   agent 循环、钩子、上下文压缩、steering
internal/tools   内置工具：read/write/edit/bash/grep/find/ls + 后台任务
internal/session JSONL 会话存储（只追加，唯一真相源）
internal/sandbox 文件效果沙箱策略 + Windows/Linux/macOS runner
internal/permission  权限规则引擎
internal/mcp     MCP 客户端桥
internal/server  scode serve：stdio JSON-RPC 会话服务
internal/update  版本检查与自更新
internal/tui     bubbletea 交互界面
desktop/         Electron + React 桌面客户端
```

## 开发

```sh
go build ./...     # 编译检查
go test ./...      # 全部测试
go vet ./...       # 静态检查
```

诊断：`SCODE_DEBUG_REQ=1` 转储请求体，`SCODE_DEBUG_SSE=1` 转储流式响应。

## 致谢

架构设计参考了 [pi](https://pi.dev) 的 agent 循环与消息模型，在此基础上独立迭代。
