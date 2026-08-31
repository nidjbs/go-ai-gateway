package main

import (
	"fmt"
	"os"
)

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	if len(args) == 0 {
		usage()
		return 0
	}
	switch args[0] {
	case "models":
		return cmdModels(args[1:])
	case "ask":
		return cmdAsk(args[1:])
	case "repl":
		return cmdRepl(args[1:])
	case "run":
		return cmdRun(args[1:])
	case "schedule":
		return cmdSchedule(args[1:])
	case "trans":
		return cmdTrans(args[1:])
	case "summarize":
		return cmdSummarize(args[1:])
	case "explain":
		return cmdExplain(args[1:])
	case "commit":
		return cmdCommit(args[1:])
	case "reload":
		return cmdReload(args[1:])
	case "status":
		return cmdStatus(args[1:])
	case "up":
		return cmdUp(args[1:])
	case "down":
		return cmdDown(args[1:])
	case "usage":
		return cmdUsage(args[1:])
	case "help", "-h", "--help":
		usage()
		return 0
	default:
		fmt.Fprintf(os.Stderr, "gw: unknown command %q\n\n", args[0])
		usage()
		return 2
	}
}

func usage() {
	fmt.Print(`gw - 个人简单重复任务 CLI(一切皆为 CLI: 对话→命令→定时任务)

用法:
  gw models                     列出 gateway 可用别名
  gw ask [选项] "问题"           通用对话,可用 --prompt 指定 prompt
  gw repl [选项]                多轮 agent 会话(可读写文件);/save <name> 沉淀为可复用命令
  gw run <command> [input]      以 agent 循环执行保存的命令(声明 tools 时自动可用)
  gw schedule                   调度:list / set <cmd> <cron> / unset / run / start / stop
  gw trans [选项] "文本"         翻译(内置 prompt)
  gw summarize [选项]           总结(读取文件或 stdin)
  gw explain [选项] "问题"       解释内容
  gw commit [选项]              生成 Git 提交信息
  gw up [config.yaml]          一键:启动本地 gateway + 配置本 CLI
  gw down                      停止本地 gateway
  gw reload [--path 文件]       触发 gateway 配置热更
  gw status                    检查 gateway 健康状态
  gw usage [--alias a] [--from t] [--to t]  查询用量(需 admin token)

通用选项:
  -m, --model <别名>  指定本次调用的模型名(必须是 gateway 配置的别名,可用 gw models 查看;默认取 default_alias)
  --no-stream        非流式输出
  -h, --help         帮助

配置: ~/.config/gw/config.yaml
  gateway_url, api_key, admin_token, default_alias
  file_roots: repl 可读写的目录(默认当前目录);write_confirm: auto/always/never
  环境变量: GW_GATEWAY_URL GW_API_KEY GW_ADMIN_TOKEN GW_ALIAS GW_FILE_ROOTS
  自定义 prompt: ~/.config/gw/prompts/<name>.md
  会话日志: ~/.config/gw/sessions/<id>.jsonl(GW_SESSION_DIR 可覆盖)
`)
}

// firstNonEmpty returns the first non-empty string.
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
