# harness — 多模型 agent 集群协作工具

OpenAI 兼容端点 + function calling + SSE 流式 + 子进程 agent + IPC 总线 + 统一状态机。零三方依赖(纯 stdlib),单文件 `harness.py`。

## 架构

```
主进程 (CLI/主会话/模式编排)
├── 端点池: SSE 流式、X-Session-Id、usage 记账、连接重试
├── 工具沙箱: 权限裁决点(read/write/exec 类别)
├── Bus: pub/sub(fnmatch)/wait + watch 声明式触发
├── StateMachine: boot/idle/thinking/tooling/waiting/speaking/done/error/stopped
│                 → run/state.json + 事件流
└── ControlSocket: 127.0.0.1 动态端口 → status/inject/state

agent = `harness.py _agent NAME` 衍生子进程
├── 私有: persona、对话 history、收件箱(inject/bus)、工具循环
└── IPC(JSONL on stdio) → 主进程: model.call / tool.call / bus.pub|sub|wait /
    context.get / state.set    ← 资源全在主进程,子进程只是"大脑"
```

## 架构 (Go)

```
cmd/harness        CLI 入口(薄)
internal/
  config/          配置·类型·权限·角色
  model/           端点·SSE 流式·重试·session·usage
  core/            Run·事件总线·状态机·共享上下文·transcript
  proc/            agent 子进程生命周期 + JSONL IPC 资源代理
  tools/           沙箱工具·权限裁决·插件·ctx/index 工具
  ctx/             上下文治理: append-only archive·pins 保全·
                   滚动摘要·指纹缓存·0阻塞压缩·recall
  index/           代码库索引·分块·BM25 式检索
  modes/           ask/chat/brainstorm/council/task
  serve/           常驻 daemon·job 队列·agent 进程池·控制口
  xfer/            导出/导入
```

依赖单向: config ← core ← {model,tools,ctx,index} ← proc ← modes ← serve ← cmd。
跨层回调用钩子注入(BusDeliver/AgentInject/StartControl/OnSpawned)。

### 上下文治理 (借鉴 ctxsvc)

- **archive**: 每 agent append-only 消息档案 (`context/archive/<name>.jsonl`)
- **pins**: `<system-reminder>`/`<memory>` 块剥出·指纹去重·永不压缩
- **0阻塞压缩**: turn 开始 Apply() 立即返回; 超 MaxEst 时后台起压缩,
  完成后下一 turn 换入 `<summary>` 消息; 失败降级仅截断不丢 pin
- **滚动摘要**: user 边界切块·ChunkEst 上限·上一块摘要 seed 续写·输入指纹缓存
- **recall**: `ctx_search` 工具按词频检索 archive

### 代码索引 (codebase memory 风格)

`index_paths` 配置的根下自动索引 go/py/js/ts/md 等 17 种文本文件,
跳过 .git/runs/构建产物; 函数级分块 + 文件指纹增量重建。
agent 工具 `code_search`/`file_overview`; `context.get` 自动附带索引概览。
CLI: `harness index [refresh]`。

## 用法

```bash
harness.py ask "问题" --agent NAME        # 单发
harness.py chat --agent NAME              # REPL: /use /agents /inject /send A msg /status /quit
harness.py brainstorm "议题" --rounds 3
harness.py council "问题"
harness.py task "描述"
harness.py status [RUNDIR]                # 活 run 走控制口;否则读 state.json
harness.py inject RUNDIR AGENT "消息"      # 向运行中的 agent 动态注入
harness.py export [RUNDIR] -o out.zip     # 导出 run
harness.py import run.zip                 # 导入到 runs/
harness.py agents | tools | check
```

flags: `--home` 配置根 | `--endpoint` 强制单端点 | `--agents a,b` | `--inproc` 不派生子进程 |
`--no-stream` | `--no-tools` | `--reminder` | `--prefill`

## 权限模型(agents.json)

```json
"permissions": {"read": true, "write": false, "exec": false, "collab": true}
```

- `read` → read_file/list_dir/grep；`write` → write_file；`exec` → run_cmd；插件工具默认 read 类
- `collab` → bus.pub/sub/wait + watch 订阅 + 被 inject
- `tools` 数组是显式白名单，与 permissions 取交集

## 协作原语

- **watch**（声明式触发）: `"watch": [{"on": "artifact.written", "note": "审查新产物"}]` —— 事件匹配时 push 进 agent 收件箱，下个工具轮次注入为 developer 消息
- **bus.wait**：agent 内可调 `bus.wait(pattern, timeout)` 阻塞等待前置事件（如等上游汇报）
- **inject**：运行中经控制口注入消息，下一轮生效
- **事件流**：`agent.state / agent.spoke / round.start / artifact.written / file.written / task.done` 全上总线，进 events.jsonl

## 文件

| 文件 | 作用 |
|---|---|
| `endpoints.json` | 端点表 |
| `agents.json` + `agents/*.md` | roster（md = frontmatter + persona） |
| `harness.json` | 全局配置：`stream/timeout_s/max_tool_rounds/turn_timeout_s/tool_roots/write_roots/reminder/sessions/role_map` |
| `tools/*.py` | 插件（暴露 `TOOLS={name:{fn,schema}}`） |
| `workspace/context/*.md` | 共享上下文 → system |
| `runs/<ts>-<mode>-<id>/` | transcript.md · events.jsonl · state.json · artifacts/ · control.port · *.stderr.log |
