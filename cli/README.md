# gw — go-ai-gateway CLI

`gw` 是 [go-ai-gateway](../README.md) 的命令行客户端,面向个人电脑使用场景。它通过 HTTP 请求本地常驻的 gateway(复用其别名路由、限流、日志),快速调用 LLM。

```
$ gw trans "hello world"        # 翻译
你好,世界▌
$ gw summarize diff.txt         # 总结
$ gw commit                     # 生成 Git 提交信息
$ gw reload                     # 改完配置后热更,无需重启 gateway
```

## 安装

```sh
cli/install.sh
# 默认用 sudo 装到 /usr/local/bin(macOS 系统 PATH,任何 shell 都能直接用 gw)
# 会提示输入 sudo 密码一次
# 不想用 sudo(装到 ~/.local/bin,需重启终端): GW_NO_SUDO=1 cli/install.sh
```

也可手动: `cd cli && go build -o ~/.local/bin/gw .`

## 快速开始(一键)

1. 准备一份 gateway 配置——**只需写 `providers` 和 `aliases`**(`admin` 会自动注入):

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

2. 一条命令拉起全部:

```sh
export OPENAI_API_KEY=sk-...
gw up ~/gw.yaml     # 构建并后台启动 gateway、注入 admin、自动写好 CLI 配置
gw models           # 立即可用
gw trans "hello world"
gw reload           # 改完配置后热更,无需重启
gw down             # 停止本地 gateway
```

`gw up` 自动完成:注入 admin 配置(供 `gw reload`)、构建并启动 gateway、等待就绪、生成 `~/.config/gw/config.yaml`。之后无需再配任何东西。

> gateway 二进制来源:`GW_GATEWAY_BIN` 指定预装二进制;否则在源码仓库里运行 `gw` 时会自动 `go build ./cmd/gateway`。

## 配置

`gw up` 会自动生成 `~/.config/gw/config.yaml`;也可以手动写(缺省则用默认值):

```yaml
gateway_url: http://127.0.0.1:8080
admin_url: http://127.0.0.1:8081    # ops/healthz 端口,reload/usage 走这里;缺省 = gateway_url
api_key: sk-...          # gateway 认证(static 或 api-key 的 key)
admin_token: ""          # reload / usage 需要
default_alias: chat      # 默认别名
```

环境变量覆盖:`GW_GATEWAY_URL` `GW_ADMIN_URL` `GW_API_KEY` `GW_ADMIN_TOKEN` `GW_ALIAS` `GW_CONFIG`。

## 命令

| 命令 | 作用 |
|---|---|
| `gw models` | 列出 gateway 可用别名 |
| `gw ask [-m 别名] [-p prompt] "问题"` | 通用对话;`-p`/`--prompt` 可指定 prompt |
| `gw trans [-m 别名] [-t 语言] "文本"` | 翻译(内置 prompt) |
| `gw summarize [-m 别名] [-f file\|-]` | 总结(文件/stdin/参数) |
| `gw explain [-m 别名] [-f file\|-] "内容"` | 解释 |
| `gw commit [-m 别名] [-f file\|-]` | 生成 Conventional Commits 提交信息(默认取 `git diff`) |
| `gw reload [--path 文件]` | 触发 gateway 配置热更 |
| `gw status` | gateway 健康检查 |
| `gw usage [--alias a] [--from t] [--to t]` | 用量查询(需 admin_token) |

通用选项:`-m/--model <别名>` 指定本次调用的模型名(必须是 gateway 配置的别名,`gw models` 可查看;默认取 `default_alias`),`--no-stream` 关闭流式输出。

```sh
$ gw models                 # 查看可选别名
$ gw trans -m chat "hello"  # 指定用 chat 别名调用
$ gw ask -m flash "问题"     # 指定用 flash 别名调用
```

## 自定义 prompt

- 内置模板:`trans`、`summarize`、`explain`、`commit`。
- 自定义:`~/.config/gw/prompts/<name>.md`,整个文件内容作为 system prompt:

```sh
$ cat > ~/.config/gw/prompts/code-review.md <<'EOF'
你是一位资深 Go 代码审查者,请指出潜在 bug 与可改进处。
EOF
$ gw ask --prompt code-review "审查这段代码: ..."
```

- `gw ask --prompt <文本>` 直接以文本作为 prompt。
