//go:build e2e

package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// E2E 门禁：真实 gateway 二进制 + 确定性 mock 上游 + 驱动 CLI（run），覆盖
// models / ask / trans / repl 工具循环 / save / run / schedule。运行方式：
//   go test -tags e2e ./cli/...   （scripts/release_check.sh 会带上）

type e2eMsg struct {
	Role       string `json:"role"`
	Content    string `json:"content"`
	ToolCallID string `json:"tool_call_id"`
}

func e2eHasRole(msgs []e2eMsg, role string) bool {
	for _, m := range msgs {
		if m.Role == role {
			return true
		}
	}
	return false
}

// e2eMock serves a deterministic OpenAI-compatible upstream; readPath is what
// the scripted read_file tool call returns.
func e2eMock(readPath string) *httptest.Server {
	readArgs, _ := json.Marshal(map[string]string{"path": readPath})
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/models":
			json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": []map[string]any{{"id": "mock-chat", "object": "model"}}})
			return
		case "/v1/chat/completions":
			var req struct {
				Messages []e2eMsg `json:"messages"`
				Stream   bool     `json:"stream"`
				Tools    []any    `json:"tools"`
			}
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				http.Error(w, `{"error":{"message":"bad request"}}`, 400)
				return
			}
			completion := func(message any, finish string) {
				w.Header().Set("Content-Type", "application/json")
				json.NewEncoder(w).Encode(map[string]any{
					"choices": []map[string]any{{"message": message, "finish_reason": finish}},
					"usage":   map[string]any{"prompt_tokens": 10, "completion_tokens": 5},
				})
			}
			toolCall := func() map[string]any {
				return map[string]any{"role": "assistant", "content": "", "tool_calls": []any{map[string]any{
					"id": "call_read_1", "type": "function",
					"function": map[string]string{"name": "read_file", "arguments": string(readArgs)},
				}}}
			}
			if req.Stream {
				w.Header().Set("Content-Type", "text/event-stream")
				if len(req.Tools) > 0 && !e2eHasRole(req.Messages, "tool") {
					argJSON, _ := json.Marshal(string(readArgs)) // JSON string literal
					w.Write([]byte(`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_read_1","function":{"name":"read_file","arguments":` + string(argJSON) + `}}]}}]}` + "\n\n"))
					w.Write([]byte(`data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}` + "\n\n"))
					w.Write([]byte("data: [DONE]\n\n"))
					return
				}
				if e2eHasRole(req.Messages, "tool") {
					w.Write([]byte(`data: {"choices":[{"delta":{"content":"已读取并总结完成"}}]}` + "\n\n"))
					w.Write([]byte("data: [DONE]\n\n"))
					return
				}
				w.Write([]byte(`data: {"choices":[{"delta":{"content":"你好"}}]}` + "\n\n"))
				w.Write([]byte("data: [DONE]\n\n"))
				return
			}
			// 蒸馏：system 提到"命令沉淀助手" → 返回未闭合 frontmatter（验证修复）
			if len(req.Messages) > 0 && req.Messages[0].Role == "system" && strings.Contains(req.Messages[0].Content, "命令沉淀助手") {
				completion(map[string]any{"role": "assistant", "content": "---\nname: demo-cmd\ndescription: 演示\n\ndistilled body"}, "stop")
				return
			}
			if len(req.Tools) > 0 && !e2eHasRole(req.Messages, "tool") {
				completion(toolCall(), "tool_calls")
				return
			}
			if e2eHasRole(req.Messages, "tool") {
				completion(map[string]any{"role": "assistant", "content": "已读取并总结完成"}, "stop")
				return
			}
			completion(map[string]any{"role": "assistant", "content": "mock response"}, "stop")
		}
	}))
}

// repoRoot walks up from the working dir to the repo root (where cmd/gateway
// lives), skipping the cli module which also has its own go.mod.
func repoRoot() string {
	dir, _ := os.Getwd()
	for {
		if _, err := os.Stat(filepath.Join(dir, "cmd", "gateway", "main.go")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
}

// buildGateway compiles the gateway binary once for the whole test run.
func buildGateway(t *testing.T) string {
	t.Helper()
	root := repoRoot()
	if root == "" {
		t.Fatal("repo root not found")
	}
	bin := filepath.Join(t.TempDir(), "gateway")
	cmd := exec.Command("go", "build", "-o", bin, "./cmd/gateway")
	cmd.Dir = root
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build gateway: %v\n%s", err, out)
	}
	return bin
}

// freePort returns an available TCP port.
func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

// startE2EGateway boots a real gateway against the mock and returns its URLs.
func startE2EGateway(t *testing.T, bin, mockURL string) (gwURL, adminURL string) {
	t.Helper()
	listen, healthz := freePort(t), freePort(t)
	cfg := fmt.Sprintf(`listen: 127.0.0.1:%d
healthz: 127.0.0.1:%d
readyz_wait_time: 0s
auth:
  mode: none
providers:
  mock:
    type: openai
    base_url: %s
aliases:
  chat:
    provider: mock
    model: mock-chat
admin:
  enabled: true
  token_env: GW_E2E_ADMIN
`, listen, healthz, mockURL)
	cfgPath := filepath.Join(t.TempDir(), "gw.yaml")
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(t.TempDir(), "gw.log")
	logF, _ := os.Create(logPath)
	proc := exec.Command(bin, "-config", cfgPath)
	proc.Env = append(os.Environ(), "GW_E2E_ADMIN=verifytoken")
	proc.Stdout = logF
	proc.Stderr = logF
	if err := proc.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { proc.Process.Kill() })
	adminURL = fmt.Sprintf("http://127.0.0.1:%d", healthz)
	for i := 0; i < 50; i++ {
		resp, err := http.Get(adminURL + "/readyz")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode >= 200 && resp.StatusCode < 500 {
				return fmt.Sprintf("http://127.0.0.1:%d", listen), adminURL
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	data, _ := os.ReadFile(logPath)
	t.Fatalf("gateway did not become ready (log: %s)\n%s", logPath, data)
	return "", ""
}

// e2eCLI runs the CLI's run() and returns exit code + stdout.
func e2eCLI(t *testing.T, args ...string) (int, string) {
	t.Helper()
	old := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w
	code := run(args)
	w.Close()
	os.Stdout = old
	data, _ := io.ReadAll(r)
	return code, strings.TrimSpace(string(data))
}

// runREPLInput pipes input into a repl session; stdout is discarded.
func runREPLInput(t *testing.T, input string) {
	t.Helper()
	oldIn := os.Stdin
	rf, _ := os.CreateTemp(t.TempDir(), "stdin")
	rf.WriteString(input)
	rf.Seek(0, 0)
	os.Stdin = rf
	oldOut := os.Stdout
	_, w, _ := os.Pipe()
	os.Stdout = w
	code := run([]string{"repl", "-m", "chat"})
	w.Close()
	os.Stdout = oldOut
	os.Stdin = oldIn
	if code != 0 {
		t.Fatalf("repl code = %d", code)
	}
}

func TestCLIE2E(t *testing.T) {
	// 工作目录 + 可读文件（mock 的 read_file 会读它）
	work := t.TempDir()
	readPath := filepath.Join(work, "a.txt")
	if err := os.WriteFile(readPath, []byte("第一行\n第二行\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	mock := e2eMock(readPath)
	defer mock.Close()
	gwBin := buildGateway(t)
	gwURL, _ := startE2EGateway(t, gwBin, mock.URL)

	state := t.TempDir()
	t.Setenv("GW_STATE_DIR", state)
	t.Setenv("GW_FILE_ROOTS", work)
	t.Setenv("GW_CONTEXT_WINDOW", "4") // 小窗口使单轮工具链触发 compaction
	writeTestCLIConfig(t, gwURL, "default_alias: chat\nadmin_token: verifytoken\n")

	// 基础：models / ask / trans
	if code, out := e2eCLI(t, "models"); code != 0 || out != "chat" {
		t.Fatalf("models = %q (code %d)", out, code)
	}
	if code, out := e2eCLI(t, "ask", "--no-stream", "hi"); code != 0 || out != "mock response" {
		t.Fatalf("ask = %q (code %d)", out, code)
	}
	if code, out := e2eCLI(t, "trans", "--no-stream", "hello"); code != 0 || out != "mock response" {
		t.Fatalf("trans = %q (code %d)", out, code)
	}

	// repl：agent 工具循环（mock 返回 read_file → 读到 a.txt → 最终回复）
	runREPLInput(t, "读 a.txt 并总结\n退出\n")
	sessID := latestSessionID(t, state)
	if sessID == "" {
		t.Fatal("no session written")
	}
	sessData, err := os.ReadFile(filepath.Join(state, "sessions", sessID+".jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"type":"tool.call"`, `"type":"tool.result"`, `"type":"context.compact"`} {
		if !strings.Contains(string(sessData), want) {
			t.Fatalf("session missing %s:\n%s", want, sessData)
		}
	}

	// /save：mock 返回未闭合 frontmatter → 自动修复并落盘
	runREPLInput(t, "你好\n/save demo-cmd\n退出\n")
	cmdData, err := os.ReadFile(filepath.Join(state, "prompts", "demo-cmd.md"))
	if err != nil {
		t.Fatalf("saved command missing: %v", err)
	}
	for _, want := range []string{"name: demo-cmd", "distilled body"} {
		if !strings.Contains(string(cmdData), want) {
			t.Fatalf("saved command missing %q:\n%s", want, cmdData)
		}
	}

	// gw run：执行保存的命令（demo-cmd 无 tools → 普通单轮回复）
	if code, out := e2eCLI(t, "run", "demo-cmd", "跑一下"); code != 0 || out != "mock response" {
		t.Fatalf("run = %q (code %d)", out, code)
	}

	// schedule：set / list / unset
	if code, out := e2eCLI(t, "schedule", "set", "demo-cmd", "@every 1h"); code != 0 || !strings.Contains(out, "schedule set") {
		t.Fatalf("schedule set = %q (code %d)", out, code)
	}
	if code, out := e2eCLI(t, "schedule"); code != 0 || !strings.Contains(out, "demo-cmd") {
		t.Fatalf("schedule list = %q (code %d)", out, code)
	}
	if code, _ := e2eCLI(t, "schedule", "unset", "demo-cmd"); code != 0 {
		t.Fatalf("schedule unset code = %d", code)
	}
}

// latestSessionID returns the most recently written session id in <state>/sessions.
func latestSessionID(t *testing.T, state string) string {
	t.Helper()
	dir := filepath.Join(state, "sessions")
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) == 0 {
		return ""
	}
	return strings.TrimSuffix(entries[len(entries)-1].Name(), ".jsonl")
}
