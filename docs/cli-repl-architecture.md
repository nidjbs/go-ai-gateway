# gw repl 设计架构

> 描述 `cli/` 中 `gw repl` 的现状实现（事件溯源的 agent 循环）。CLI 主文档见 [`cli/README.md`](../cli/README.md)，gateway（本地 OpenAI 兼容服务）架构见 [`architecture.md`](architecture.md)。

一句话概括：**一切交互先落事件日志，再由日志投影出「模型可见的上下文」；模型要动文件只能走受限于根目录白名单的工具调用；远端只连一个本地 gateway，gateway 再做 alias → 上游模型的路由。**

## 分层总览

```
                      ┌────────────────────────────────────────────────────────────┐
 交互层  stdin/终端   │  replLoop (cli/repl.go)                                       │
                      │   ├─ 斜杠命令: /exit /compact /clipboard /save <name> [目的] │
                      │   └─ 普通用户输入 → 转成一轮 agent 交互                       │
                      └──────────────┬─────────────────────────────────────────────┘
                                     │ 每轮
                                     ▼
 agent 引擎   cli/agent.go  agentReply()  工具循环 ≤ 20 轮
                      ┌─────────────────────────────────────────────┐
                      │  turn: client.AgentTurn(Stream)              │
                      │   │  返回 assistant 文本 + tool_calls?       │
                      │   │  有工具调用 → 本地执行 → 结果回灌 → 再转   │
                      │   ▼                                          │
                      │  FilePolicy (cli/tools.go): 根白名单+写确认    │
                      │  read/write/list/delete file · mkdir · rename │
                      └──────────────┬───────────────────────────────┘
                                     │ 每个动作先 emit 事件
                                     ▼
 记忆层  Session (cli/sessionlog.go)  事件溯源：内存 + <state>/sessions/<id>.jsonl append
         ┌──────────────────────────────────────────────┐
         │ events(全量原始)                              │
         │   ├─ surface 事件: system / user / assistant │
         │   │                  / tool                   │
         │   └─ audit 事件:    model.request / tool.call │
         │        · 压缩/裁剪 = ShadowSeqs 隐藏原件,不删 │
         │        · 大工具结果 = replace 事件(原件留档)   │
         └───────────┬────────────────┬─────────────────┘
                     │Messages()      │FullTranscript()
                     ▼                ▼
        cli/context.go 滑动窗口       /save 蒸馏用「全量原始」
        surface ──────────────────► 模型请求     ──► prompts/<name>.md
                                     │
 传输层  cli/client.go + sse.go     HTTP+SSE  /v1/chat/completions (OpenAI 兼容)
         ─────────────────────────► 本地 go-ai-gateway ── alias 路由 ──► 上游模型
```

## 模块职责

| 模块 | 文件 | 职责 |
|---|---|---|
| 命令入口/循环 | `cli/repl.go` `replLoop` | 读 stdin、解析斜杠命令、把普通输入交给 agent 引擎、恢复会话 |
| 会话引导 | `cli/agentrules.go` `cli/prompt.go` | 注入系统提示：`defaultAgentPrompt` 或 `--system` → 追加全局 `agent.md` |
| Agent 引擎 | `cli/agent.go` `agentReply` | 工具循环；每轮发模型请求 → 执行工具 → 回灌 → 直到模型不再调用工具（上限 20 轮） |
| 记忆/事件溯源 | `cli/sessionlog.go` `Session` | append-only 事件日志（内存 + JSONL）；派生 surface 投影 |
| 上下文管理 | `cli/context.go` | 滑动窗口压缩（shadow 最旧）与超大工具结果裁剪（头+省略+尾） |
| 工具执行 | `cli/tools.go` | `FilePolicy` 工具集：`agentTools` 声明、`DispatchTool` 分发、根目录白名单、TTY 写确认 |
| 命令沉淀 | `cli/repl.go` `saveFlow`/`distillCommand`、`cli/command.go` | `/save`：目的句锚定蒸馏 → 草稿预览 → 确认后写 `prompts/<name>.md` |
| 传输 | `cli/client.go`、`cli/sse.go` | 鉴权请求本地 gateway；流式走 SSE；`Chat` 非流式（蒸馏用）、`AgentTurn(Stream)` 工具轮 |
| 剪贴板旁路 | `cli/clipboard.go` | 守护进程记账 + `/clipboard recall` 走本地模型召回，内容不进远端上下文 |
| 状态目录 | `cli/config.go` | `~/.config/gw/{config.yaml,prompts,sessions,agent.md}`，均可用 `GW_*` 覆盖 |

## 一次普通 turn 的数据流

```
用户输入 ── emit(user.message) ──► 进入 surface
   │
   ▼
agentReply: msgs = sess.Messages()          // 从当前 surface 投影
   └─ 循环 ≤ 20:
        client.AgentTurn(msgs, tools)  ────► 本地 gateway(apiKey 鉴权) ─► 上游模型
        emit(model.request: tokens / 耗时)
        emit(assistant.message)
        ├─ 无 tool_calls → 结束；文本流式写 stdout
        └─ 有 tool_calls ─► emit(tool.call)
              └─ FilePolicy.DispatchTool()    // 白名单 + TTY 写确认
                   emit(tool.result: 完整内容)  → 超 8KB 则 EmitTrim(原件留档)
                   msgs 回灌完整结果 → 下一轮
   ▼
maybeCompact: surface 剩余 < 20% 时 → forceCompact 滑到 60% 低水位
```

### 关键点

- **每轮请求的消息**直接来自 `sess.Messages()`，而新输入在调用前已 `emit`，所以「刚说的那句话」立即可见 —— 没有第二条消息通道。
- **工具执行在本地**：模型只声明要调用哪个工具与参数，实际读写发生在 CLI 进程里，受 `file_roots` 约束；写类操作按 `write_confirm`（`auto/always/never`）决定是否 TTY 确认。
- **工具循环内的模型仍看到完整结果**，只有日志里的 surface 才被裁剪；`agentReply` 上限 20 轮防失控链条（`maxAgentTurns`）。

## 记忆模型：事件日志 → 投影

两个不变量（README 亦注明，代码贯彻）：

1. **「模型可见即已记录」** —— 任何进入模型请求的内容都能从 JSONL 重建。会话恢复（`--resume`）、上下文压缩、`/save` 都读同一份日志，互不干扰。
2. **压缩与裁剪只隐藏、不删除**：
   - 滑动窗口把最旧 surface 事件的 seq 记进 `evContextCompact` 的 `ShadowSeqs`，`surfaceEvents()` 投影时跳过被 shadow 的事件；**原件仍在日志里**。
   - 超大工具结果（> 8192 字节）用一条 `replace` 事件（`EmitTrim`）让 surface 只看到「头 4096 + 省略标记 + 尾 1024」，原件留档。
   - `Messages()` = 当前 surface（模型可见、有界）；`FullTranscript()` = 反查原始事件（含被压缩/替换的），供 `/save` 蒸馏整段对话。

| 投影 | 函数 | 用途 |
|---|---|---|
| 模型上下文 | `Session.Messages()` | 每轮 agent 请求 |
| 蒸馏输入 | `Session.FullTranscript()` | `/save` 提炼命令 |
| 恢复 | `loadSession(dir, id)` | `--resume` 重放 + 续写 |

## 斜杠命令与命令沉淀

- `/save <name> [目的句]` —— 目的句缺省时 REPL 内补问；蒸馏要求 `description`/正文围绕目的、正文含编号「整体执行流程」；**先打印草稿再等确认**（`y` 保存 / `n`、空回车取消 / 其它文本当新目的句重炼）。产物是 YAML frontmatter（name/description/tools/schedule）+ 正文（system prompt），见 [`command.go`](../cli/command.go)。
- `/compact` —— 立即手动压缩到 60% 低水位。
- `/clipboard recall <描述>` —— 用本地模型在剪贴板历史中召回，结果复制回剪贴板；敏感内容不经过远端模型。
- `/exit`（及 `exit/quit/退出`）—— 记 `session.ended` 后退出。

### 与其它子命令复用同一套

`gw run <cmd>` / `gw schedule` 与 repl 共享 `agentReply` + `Session` 事件日志，只是**一次性会话**：加载 `prompts/<name>.md` 的正文作 system prompt、按 `tools` 声明只开放对应工具（`selectTools`）、无输入时自主执行，不参与交互压缩。蒸馏出的命令文件、`gw run`、调度器是同一份产物的三种消费方式。

## 配置与状态目录

| 路径 | 覆盖变量 | 内容 |
|---|---|---|
| `~/.config/gw/config.yaml` | `GW_CONFIG` / `GW_STATE_DIR` | gateway 地址、api key、默认 alias、`file_roots`、压缩窗口、剪贴板本地 alias |
| `~/.config/gw/prompts/` | `GW_PROMPTS_DIR` | `/save` 产出的 `<name>.md` |
| `~/.config/gw/sessions/` | `GW_SESSION_DIR` | `<id>.jsonl` 事件日志（0600） |
| `~/.config/gw/agent.md` | `GW_STATE_DIR` | 全局约定，注入每次 agent 会话 |
| `~/.config/gw/clipboard.*` | — | 剪贴板守护进程的日志/pid |

## 边界与安全

- **文件读写**仅在 `file_roots`（默认会话开始时的工作目录）内，`resolvePath` 会拦截越界路径。
- **写/删类工具**按 `write_confirm` 在 TTY 确认；非 TTY 下 `auto` 模式直接拒绝。
- **剪贴板内容**只在本地模型与本地日志之间流转，遥控 agent 上下文里永远见不到。
- 所有 agent 动作都写入会话日志，可随时回溯、`--resume` 续接。
