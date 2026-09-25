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
- [ ] M4 JSONL 会话 + 投影 + 系统提示词 + 压缩
- [ ] M5 print 模式 + REPL
- [ ] M6 TUI(bubbletea)
- [ ] M7 MCP + 更多 provider

## 开发

```sh
go build ./...
go test ./...
```

架构参考:pi v0.87.1 快照(`E:\ai\harness\pi`),关键对照见各源文件注释。
