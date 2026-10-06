# harness (Go 版)

通用多模型协作 harness: 主进程调度 + agent 衍生子进程(自持 history/inbox/工具循环),资源经 JSONL IPC 回主进程裁决。

## 命令

| 命令 | 说明 |
|---|---|
| `harness ask "q" [--agent N]` | 单发(带工具循环) |
| `harness chat [--agent N]` | REPL: /use /agents /inject /send /status /quit |
| `harness brainstorm "议题" [--rounds N]` | 圆桌(agent 自持 history) |
| `harness council "q"` | 并行作答→互评→裁决 |
| `harness task "desc"` | orchestrator 拆任务→派工→critic 验收 |
| `harness serve` | 常驻 daemon(serve_port, agent 跨任务保活) |
| `harness submit MODE "topic" [--agents a,b] [--endpoint e]` | 提交任务给 daemon |
| `harness serve-status/serve-stop` | daemon 管理 |
| `harness status [rundir]` / `inject AGENT "msg"` | 控制口 |
| `harness export/import` | run 目录 zip 迁移 |
| `harness check` | 全 agent 端点连通性 |
| `harness agents/tools` | 查看 roster/工具 |

全局 flag: `--home DIR`(配置根) `--endpoint E` `--no-tools` `--no-procs` `--reminder s`

## 架构

- `types.go` 配置加载(endpoints/agents.json/agents/*.md frontmatter/harness.json)
- `endpoint.go` OpenAI 兼容调用: SSE 流式、IPv4 强制、session、429/5xx 退避重试、developer→system 降级
- `tools.go` 工具注册表 + 权限类别(read/write/exec/collab) ∩ 白名单裁决 + 沙箱路径 + .tool.json 插件
- `proc.go` 主进程侧子进程句柄: spawn/res/push 路由/watch 订阅
- `child.go` `_agent` 子进程: 自持 history + inbox 消化 + 工具循环
- `run.go` Run/Bus(fnmatch pub-sub-wait)/统一状态机/state.json/events.jsonl/artifact 提取
- `modes.go` ask/chat/brainstorm/council/task
- `serve.go` 常驻 daemon + job 队列 + agent 保活池
- `control.go` 运行中控制口(status/inject/state)
- `export.go` zip 导入导出
- `inproc.go` --no-procs 回退路径

## 测试

`go test` — 覆盖 fnmatch/Bus pub-sub-wait/状态机/transcript/artifact 提取/权限裁决/路径沙箱/grep/SSE 解析(content+tool_calls+reasoning)/HTTP 重试/frontmatter/config 合并/IPC roundtrip。

## agent 配置

`agents.json` 或 `agents/*.md`(frontmatter):
```yaml
---
model: glm-5.3
endpoint: olo
temp: 0.4
tools: [read_file, grep]
permissions: {read: true, write: false, exec: false, collab: true}
watch: [{on: artifact.written, note: 评审新产物}]
developer: [注入文本]
---
persona 正文
```
