# gw — go-ai-gateway CLI

`gw` is the command-line client for [go-ai-gateway](../README.md), aimed at personal/desktop use. It calls a local gateway process over HTTP (reusing the gateway's alias routing, rate limits, and logging) for quick LLM interactions.

```
$ gw trans "hello world"        # translate
你好,世界▌
$ gw summarize diff.txt         # summarize
$ gw commit                     # generate a Git commit message
$ gw reload                     # hot-reload the config without restarting the gateway
```

## Installation

```sh
cli/install.sh
```

Installs to `/usr/local/bin` with sudo by default (on the macOS system PATH, so `gw` works in every shell). To skip sudo (installs to `~/.local/bin`, restart the terminal):

```sh
GW_NO_SUDO=1 cli/install.sh
```

Or manually:

```sh
cd cli && go build -o ~/.local/bin/gw .
```

## Quick start (one command)

1. Prepare a gateway config — only `providers` and `aliases` are needed (`admin` is injected automatically):

```yaml
# ~/gw.yaml
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
```

2. Bring everything up with one command:

```sh
export OPENAI_API_KEY=sk-...
gw up ~/gw.yaml     # build + start the gateway in the background, inject admin, write the CLI config
gw models           # ready to use
gw trans "hello world"
gw reload           # hot-reload after config edits, no restart
gw down             # stop the local gateway
```

`gw up` automatically: injects the admin block (for `gw reload`), builds and starts the gateway, waits until ready, and writes `~/.config/gw/config.yaml`. Nothing else to configure afterwards.

> Gateway binary resolution: `GW_GATEWAY_BIN` picks a prebuilt binary; otherwise, running `gw` from the source repo auto-builds via `go build ./cmd/gateway`.

## Configuration

`gw up` writes `~/.config/gw/config.yaml` automatically; you can also create it by hand (missing keys fall back to defaults):

```yaml
gateway_url: http://127.0.0.1:8080
admin_url: http://127.0.0.1:8081    # ops/healthz port; reload/usage go here; defaults to gateway_url
api_key: sk-...          # gateway auth (static or api-key)
admin_token: ""          # needed for reload / usage
default_alias: chat      # default alias
```

Environment variable overrides: `GW_GATEWAY_URL` `GW_ADMIN_URL` `GW_API_KEY` `GW_ADMIN_TOKEN` `GW_ALIAS` `GW_CONFIG`.

## Agent REPL and session logs

`gw repl` runs an agentic loop: the model can call `read_file` / `write_file` / `list_dir` tools, and results are fed back automatically (bounded to 8 tool rounds per user turn).

### File tool permissions

- `file_roots` (config, default = the working directory at session start) allowlists directories `read_file`/`write_file` may touch. Paths are canonicalized (symlinks and `..` resolved) before any check; anything outside a root is denied.
- Reads inside a root are allowed without prompting. Writes are confirmed on a TTY unless `write_confirm` is `never` (skip) or `always` (prompt even on non-TTY, which then fails). Non-interactive sessions deny writes by default.

```yaml
file_roots:
  - /path/to/project
write_confirm: auto      # auto | always | never
```

Env overrides: `GW_FILE_ROOTS` (path-list), `GW_SESSION_DIR`.

### Session log

Every REPL session appends one JSON line per agent event to `~/.config/gw/sessions/<id>.jsonl` (`0600`; override with `GW_SESSION_DIR`):

| type | when |
|---|---|
| `session.started` / `session.ended` | session open / close |
| `user.message` | a user line |
| `model.request` | one model round-trip (tokens, duration_ms) |
| `assistant.message` | model reply text |
| `tool.call` / `tool.result` | a file tool invocation (path, allowed, content) |
| `agent.error` | a failed request or the tool-round cap |

Each event carries `session_id`, `event_id`, `occurred_at`, and a `request_id` also forwarded to the gateway as `X-Request-Id`, so gateway-side events (`request.started` etc.) correlate with the session log. The flat snake_case schema mirrors `internal/events.Event` and is the stable source future context engineering will map from.

## Saved commands

`/save <name>` distills a REPL session into a reusable command at `~/.config/gw/prompts/<name>.md`. The file is YAML frontmatter + a markdown body (the system prompt); old frontmatter-free prompts still work.

```markdown
---
name: weekly-report
description: 生成周报
tools: [read_file, write_file]
schedule: "0 9 * * 1"
---

你是周报助手。汇总本周提交并生成周报...
```

- `tools` — tools the command may call (`read_file`, `write_file`, `list_dir`, `delete_file`, `mkdir`, `delete_dir`, `rename`). Run with `gw run <name> "输入"` to execute agentically; `gw ask --prompt <name>` stays single-turn without tools. Mutations (write/mkdir/rename/delete) follow `write_confirm`.
- `schedule` — a cron expression (`0 9 * * 1`) or `@every 24h` / `@daily` for periodic execution.

### Scheduler

`gw schedule` manages a built-in background daemon (pid + log under `~/.config/gw/`):

```sh
gw schedule set weekly-report "0 9 * * 1"   # set or re-set the cron expression
gw schedule unset weekly-report             # clear it
gw schedule                                 # list commands, next run, daemon status
gw schedule run                             # run due commands once (manual/OS-cron hook)
gw schedule start                           # start the background daemon
gw schedule stop                            # stop it
```

The daemon executes each scheduled command agentically and records last-run times in `scheduler-state.json` (so a manual `gw schedule run` never double-runs what the daemon already did).

## Commands

| Command | What it does |
|---|---|
| `gw models` | List available aliases |
| `gw ask [-m alias] [-p prompt] "question"` | General chat; `-p`/`--prompt` sets a prompt |
| `gw repl [-m alias] [--system p] [-f file]` | Multi-turn **agent** chat (can call `read_file`/`write_file` tools); `/save <name>` distills the session into a reusable command |
| `gw run <command> [input]` | Run a saved command agentically; declared `tools` are available |
| `gw schedule [set/unset/run/start/stop]` | Manage the built-in scheduler (list, set cron, run due, start/stop the daemon) |
| `gw trans [-m alias] [-t lang] "text"` | Translate (built-in prompt; auto-detects 中文↔English) |
| `gw summarize [-m alias] [-f file\|-]` | Summarize (file / stdin / argument) |
| `gw explain [-m alias] [-f file\|-] "content"` | Explain |
| `gw commit [-m alias] [-f file\|-]` | Generate a Conventional Commits message (defaults to `git diff`) |
| `gw reload [--path file]` | Hot-reload the gateway config |
| `gw status` | Gateway health check |
| `gw usage [--alias a] [--from t] [--to t]` | Usage query (requires admin_token) |

Common flags: `-m/--model <alias>` picks the model for this call (must be a configured alias, see `gw models`; defaults to `default_alias`); `--no-stream` disables streaming output.

```sh
$ gw models                 # list aliases
$ gw trans -m chat "hello"  # call with the chat alias
$ gw ask -m flash "question"  # call with the flash alias
```

## Custom prompts

- Built-in templates: `trans`, `summarize`, `explain`, `commit`.
- Custom: `~/.config/gw/prompts/<name>.md` — the file body (frontmatter stripped) is used as the system prompt:

```sh
$ cat > ~/.config/gw/prompts/code-review.md <<'EOF'
You are a senior Go code reviewer. Point out potential bugs and improvements.
EOF
$ gw ask --prompt code-review "review this code: ..."
```

- `gw ask --prompt <text>` uses the text directly as the prompt.
