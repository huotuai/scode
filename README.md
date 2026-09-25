# scode

基于 [pi](https://pi.dev) 架构的精简 Go 编程助手。独立迭代,不追随上游。

## 设计支柱

1. **前缀字节级稳定**(缓存命中率的根本):transcript 只追加;系统提示词与初始工具声明在首条
   system 消息(`ts=0`);工具变更/提示词分节变更一律走增量消息;易变内容(日期、git 状态)
   禁止进提示词,通过 bash 工具环境变量传递。
2. **双层消息模型**:agent 全程使用 provider 中立的 `Message`,只在 provider 边界转换 wire format。
3. **错误进流**:LLM 调用失败编码为流内 `error` 事件 + `stopReason=error` 终止消息,不 panic。
4. **存储与视图分离**(M4):JSONL 会话只追加,LLM 上下文是读取时构建的投影。

## 里程碑

- [x] M0 消息模型 + 前缀稳定性测试
- [x] M1 双协议 provider(Anthropic + OpenAI 兼容)
- [x] M2 agent 循环 + 工具接口/钩子
- [x] M3 核心工具集(read/write/edit/bash/grep/find/ls)
- [x] M4 JSONL 会话 + 系统提示词 + 压缩 + 读时投影
- [x] M5 print 模式 + REPL
- [ ] M6 TUI(bubbletea)
- [ ] M7 MCP + 更多 provider

## 上下文管理(压缩)

长会话自动压缩:投影上下文超过阈值(settings.json `compactionTokens`,默认 80k,负数关闭)时,
后台发起一次性摘要调用(禁用缓存,不污染主对话缓存),落一条 compaction 标记到会话文件,
并从**读时投影**重建 LLM 上下文——存储的历史永不改写,read/modified 文件清单跨代传递。
`--resume` 从投影恢复。

## 使用

```sh
# OpenAI 兼容端点(GLM 等)
export OPENAI_COMPAT_API_KEY=...
export SCODE_OPENAI_BASE_URL=https://open.bigmodel.cn/api/paas/v4
scode -p --provider openai-compat --model glm-4.6 "任务描述"

# Anthropic 协议(GLM Anthropic 兼容端点 / Claude)
export ANTHROPIC_API_KEY=...
export SCODE_ANTHROPIC_BASE_URL=...   # 可选
scode            # REPL

scode --sessions # 列出当前目录的会话
scode --resume <id> -p "继续"
```

诊断:`SCODE_DEBUG_REQ=1` 转储请求体,`SCODE_DEBUG_SSE=1` 转储流式响应。

## 开发

```sh
go build ./...
go test ./...
```

架构参考:pi v0.87.1 快照(`E:\ai\harness\pi`),关键对照见各源文件注释。
