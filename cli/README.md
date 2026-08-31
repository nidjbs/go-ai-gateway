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

## Commands

| Command | What it does |
|---|---|
| `gw models` | List available aliases |
| `gw ask [-m alias] [-p prompt] "question"` | General chat; `-p`/`--prompt` sets a prompt |
| `gw repl [-m alias] [--system p] [-f file]` | Multi-turn chat; `/save <name>` distills the session into a reusable command |
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
- Custom: `~/.config/gw/prompts/<name>.md` — the whole file content is used as the system prompt:

```sh
$ cat > ~/.config/gw/prompts/code-review.md <<'EOF'
You are a senior Go code reviewer. Point out potential bugs and improvements.
EOF
$ gw ask --prompt code-review "review this code: ..."
```

- `gw ask --prompt <text>` uses the text directly as the prompt.
