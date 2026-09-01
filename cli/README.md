# gw — 你的个人简单重复任务 CLI

> **一切皆为 CLI**：对话、命令、定时任务，全在终端里完成。

`gw` 是 [go-ai-gateway](../README.md) 的个人命令行客户端，定位是**个人简单重复任务**——把日常"让模型做点小事"的操作收敛到终端：

- **简单任务** → 一条命令搞定（`gw trans`、`gw ask`…）
- **重复任务** → 对话沉淀成命令（`/save`），随时 `gw run`
- **周期任务** → 命令挂上调度（`gw schedule`），定时自动执行

它调用本地 gateway（复用其别名路由、限流、日志与事件），不直连任何模型厂商。所有 agent 行为都会写入会话日志，可随时回溯。

## 一切皆为 CLI 的工作流

```sh
# ① 对话 —— agent 会话，可读写文件（流式输出）
gw repl -m common

# ② 沉淀 —— 把一段成功的对话蒸馏成一个可复用命令
gw> /save weekly-report
saved command → ~/.config/gw/prompts/weekly-report.md

# ③ 执行 —— 随时以 agent 方式运行，命令声明的工具自动可用
gw run weekly-report "汇总本周提交"

# ④ 定时 —— 让它在每周一早上 9 点自动跑
gw schedule set weekly-report "0 9 * * 1"
gw schedule                        # 查看下次执行时间 / 守护进程状态
gw schedule start                  # 启动后台调度器
```

从一次对话，到一个命令，再到一个定时任务——**每一步都是 CLI**。

## 快速开始

```sh
# 1. 准备 gateway 配置(只需 providers + aliases,admin 自动注入)
cat > ~/gw.yaml <<'EOF'
auth:
  mode: none
providers:
  openai:
    type: openai
    base_url: https://api.openai.com/v1
    api_key_env: OPENAI_API_KEY
aliases:
  chat:
    provider: openai
    model: gpt-4o
EOF

# 2. 一条命令拉起本地 gateway 并配置好 CLI
export OPENAI_API_KEY=sk-...
gw up ~/gw.yaml      # 构建 + 后台启动 gateway + 注入 admin + 写 CLI 配置
gw models            # 就绪
gw trans "hello world"
```

`gw up` 自动：注入 admin 块（供 `gw reload`）、构建并启动 gateway、等待就绪、写 `~/.config/gw/config.yaml`。

> `GW_GATEWAY_BIN` 指定 gateway 可执行文件；否则从源码仓库自动 `go build ./cmd/gateway`。

## 日常随手用（开箱即用）

`gw up` 之后就绪，这些命令立刻能用：

```sh
# 翻译 —— 自动识别中文↔英文; -t 指定目标语言
gw trans "hello world"                 # → 你好，世界
gw trans "今天天气不错"
gw trans -t 日语 "把这段翻译成日语"

# 总结 —— 文件 / stdin / 参数
gw summarize README.md
gw summarize -f diff.txt
cat long.txt | gw summarize -

# 解释一段内容
gw explain "什么是幂等性？"

# 生成 Git 提交信息 —— 自动取 git diff
gw commit

# 单轮问答
gw ask "golang 的 defer 有什么作用"

# 多轮 agent 对话 —— 可读写文件、流式输出
gw repl

# 查看当前可用的模型别名
gw models
```

所有命令都支持 `-m <别名>` 指定模型（默认 `default_alias`）、`--no-stream` 关闭流式。

## 配置

`gw up` 自动写 `~/.config/gw/config.yaml`；也可以手写（缺省用默认值）：

```yaml
gateway_url: http://127.0.0.1:8080
admin_url: http://127.0.0.1:8081    # ops/healthz 端口; reload/usage 走这里; 缺省同 gateway_url
api_key: sk-...          # gateway 鉴权(static 或 api-key)
admin_token: ""          # reload/usage 需要
default_alias: chat      # 默认别名
```

环境变量覆盖：`GW_GATEWAY_URL` `GW_ADMIN_URL` `GW_API_KEY` `GW_ADMIN_TOKEN` `GW_ALIAS` `GW_CONFIG` `GW_FILE_ROOTS` `GW_SESSION_DIR` `GW_CONTEXT_WINDOW` `GW_CONTEXT_TRIGGER`。

## 对话：`gw repl`

agent 会话：模型可调用文件工具，结果自动回填，继续对话（每轮最多 8 次工具往返）。助手文本**流式输出**；`--no-stream` 关闭流式。会话上下文**事件溯源**自 session 日志（见下），可用 `--resume` 重放恢复上次会话。

```sh
gw repl -m common                     # 默认别名可省略 -m
gw repl --system "你是周报助手"         # 指定系统提示(名字/文件/原文)
gw repl -f notes.txt                  # 用文件内容作为首条消息
gw repl --resume 20260831T...-abcd    # 从该 session 日志恢复并继续
```

会话内命令：`/compact` 立即压缩上下文（压到约 60% 低水位），`/save <name>` 沉淀命令，`/exit` 退出。

### 上下文窗口（滑动压缩 = compaction）

repl 用滑动窗口控制模型上下文：默认保留最近 **20** 条 surface 消息。**大工具结果（>8KB）在发射时即裁剪**为"开头 + 省略标记 + 结尾"（保留开头 4096 字节 + 结尾 1024 字节），surface 永不携带完整大结果，原结果仍完整留在日志。**窗口用到近满（剩余 < 20%）才触发**——把最旧消息通过 `context.compact` 事件滑出到约 60% 低水位。原事件全部保留在日志里（标记 shadowed），只影响投影（surface），`/save` 与重放仍取完整转录。

```yaml
context_window: 20      # 窗口容量(消息条数); 0 = 不压缩
context_trigger: 20     # 剩余容量低于该百分比时触发压缩
```

环境变量：`GW_CONTEXT_WINDOW` `GW_CONTEXT_TRIGGER`。

### 工具集（文件/目录增删改查）

| 工具 | 作用 |
|---|---|
| `read_file` / `write_file` | 读 / 写文件（写会自动建父目录） |
| `list_dir` | 列出目录条目（d/f 前缀 + 名称 + 大小 + 时间） |
| `mkdir` / `delete_dir` | 建目录 / 删目录（`recursive: true` 连内容删） |
| `delete_file` | 删文件 |
| `rename` | 移动 / 重命名文件或目录 |

### 权限模型

- `file_roots`（配置，默认 = 会话启动时的工作目录）白名单允许访问的目录。路径先规范化（解析符号链接与 `..`）再校验，白名单外一律拒绝。
- 白名单内读取免确认；所有**变更操作**（写/建目录/移动/删除）按 `write_confirm` 处理：`auto`（默认，TTY 交互确认，非 TTY 拒绝）`always`（总是提示，非 TTY 失败）`never`（白名单内跳过确认）。

```yaml
file_roots:
  - /path/to/project
write_confirm: auto      # auto | always | never
```

## 沉淀：`/save <name>`

把一段成功的对话蒸馏成一个可复用命令，存为 `~/.config/gw/prompts/<name>.md`（YAML frontmatter + 正文）。模型会根据对话中实际用到的工具，自动声明 `tools`。

```markdown
---
name: weekly-report
description: 生成周报
tools: [read_file, write_file]
schedule: "0 9 * * 1"
---

你是周报助手。汇总本周提交并生成周报...
```

- `tools` — 命令可调用的工具（`read_file` `write_file` `list_dir` `delete_file` `mkdir` `delete_dir` `rename`）。
- `schedule` — cron 表达式（`0 9 * * 1`）或 `@every 24h` / `@daily`，用于定时执行。
- 旧的无 frontmatter 的 `.md` 提示文件仍然兼容。

## 执行：`gw run <command> [input]`

以 agent 方式运行保存的命令：正文作为系统提示，声明的 `tools` 自动可用，无输入时自主执行。

```sh
gw run weekly-report                    # 自主执行(命令正文指导行为)
gw run weekly-report "只汇总 cli/ 的提交"
```

## 定时：`gw schedule`

内置后台调度器（pid/日志在 `~/.config/gw/` 下），按 cron 触发执行已保存命令：

```sh
gw schedule set weekly-report "0 9 * * 1"   # 设定/重设 cron
gw schedule unset weekly-report             # 清除
gw schedule                                 # 列表:下次执行时间 + 守护进程状态
gw schedule run                             # 立即执行一次到期命令(可挂 OS cron)
gw schedule start                           # 启动后台守护进程
gw schedule stop                            # 停止
```

守护进程按 `scheduler-state.json` 记录上次执行时间，避免手动 `gw schedule run` 与守护进程重复执行。变更 schedule 后需 `gw schedule stop && start` 重载（v1 未做热加载）。

## 会话日志（事件溯源）

每次 REPL / `gw run` / 调度执行都是 **append-only 的事件日志**：`~/.config/gw/sessions/<id>.jsonl` 每行一个事件（0600），不变量 **"模型可见即已记录"**——任何进入模型请求的内容都能从日志重建。模型上下文是日志的**派生投影（surface）**；`--resume`、`/save`、未来上下文工程都读同一份日志。

| 类型 | 时机 |
|---|---|
| `session.started` / `session.ended` | 会话开 / 关 |
| `system.context` | 系统提示（surface） |
| `user.message` | 用户输入（surface） |
| `assistant.message` | 模型回复 + 完整 `tool_calls`（surface，无损） |
| `tool.call` / `tool.result` | 工具调用（含完整 arguments）/ 结果（surface） |
| `model.request` | 一次模型往返（tokens、duration_ms，审计） |
| `context.compact` | 压缩：携带 `shadow_seqs` 折叠最旧（审计，原事件保留） |
| `agent.error` | 请求失败或超过工具轮数上限 |

每个事件携带 `session_id`、`event_id`、`seq`（单调）、`occurred_at`、`request_id`（同时作为 `X-Request-Id` 转发给 gateway），与 gateway 侧事件对齐，跨层可回溯。扁平 snake_case schema 与 `internal/events.Event` 一致，是未来上下文工程映射的稳定来源。

## 命令速查

| 命令 | 作用 |
|---|---|
| `gw up [config.yaml]` / `gw down` | 启动 / 停止本地 gateway |
| `gw models` | 列出可用别名 |
| `gw repl [-m alias] [--system p] [-f file] [--resume id]` | 多轮 agent 会话（可读写文件、流式输出、事件溯源）；`/save <name>` 沉淀命令，`--resume` 恢复上次会话 |
| `gw run <command> [input]` | 以 agent 方式运行保存的命令（声明 tools 自动可用） |
| `gw schedule [set/unset/run/start/stop]` | 管理内置调度器 |
| `gw ask [-m alias] [-p prompt] "问题"` | 单轮对话（无工具） |
| `gw trans [-m alias] [-t lang] "文本"` | 翻译 |
| `gw summarize [-m alias] [-f file\|-]` | 总结 |
| `gw explain [-m alias] [-f file\|-] "内容"` | 解释 |
| `gw commit [-m alias] [-f file\|-]` | 生成 Git 提交信息（默认取 `git diff`） |
| `gw reload [--path file]` | 热更 gateway 配置 |
| `gw status` | gateway 健康检查 |
| `gw usage [--alias a] [--from t] [--to t]` | 用量查询（需 admin_token） |

通用选项：`-m/--model <别名>` 指定本次调用别名（默认 `default_alias`）；`--no-stream` 关闭流式。

## 自定义 prompt

- 内置模板：`trans`、`summarize`、`explain`、`commit`。
- 自定义：`~/.config/gw/prompts/<name>.md`（正文即系统提示，frontmatter 会被剥离）：

```sh
cat > ~/.config/gw/prompts/code-review.md <<'EOF'
You are a senior Go code reviewer. Point out potential bugs and improvements.
EOF
gw ask --prompt code-review "review this code: ..."
```

- `gw ask --prompt <text>` 直接把文本作为提示。
