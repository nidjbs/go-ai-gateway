package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// maxFileBytes caps read_file output and write_file input.
const maxFileBytes = 64 * 1024

// truncMarker is appended when read_file output is cut at maxFileBytes.
const truncMarker = "\n… [截断,超过 64KiB]"

// agentTools returns the tool set advertised to the model in agent sessions.
func agentTools() []ToolSpec {
	return []ToolSpec{toolReadFile, toolWriteFile}
}

var toolReadFile = ToolSpec{
	Type: "function",
	Function: ToolSpecFunction{
		Name:        "read_file",
		Description: "Read a text file within the allowed roots and return its content.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path": map[string]any{"type": "string", "description": "Path to the file to read."},
			},
			"required": []string{"path"},
		},
	},
}

var toolWriteFile = ToolSpec{
	Type: "function",
	Function: ToolSpecFunction{
		Name:        "write_file",
		Description: "Write a text file within the allowed roots, creating parent dirs as needed.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path":    map[string]any{"type": "string", "description": "Path to write to."},
				"content": map[string]any{"type": "string", "description": "Full file content to write."},
			},
			"required": []string{"path", "content"},
		},
	},
}

// FilePolicy governs which paths read_file/write_file may touch: a canonical
// allowlist of roots plus a write-confirmation mode. Outside any root every
// operation is denied.
type FilePolicy struct {
	roots   []string
	mode    string            // auto (TTY prompt) | always | never
	confirm func(string) bool // injected in tests
}

// newFilePolicy builds a policy from config roots, defaulting to the current
// working directory. confirmMode is validated against auto/always/never.
func newFilePolicy(roots []string, confirmMode string) (*FilePolicy, error) {
	if len(roots) == 0 {
		cwd, err := os.Getwd()
		if err != nil {
			return nil, err
		}
		roots = []string{cwd}
	}
	canon := make([]string, 0, len(roots))
	for _, r := range roots {
		abs, err := filepath.Abs(r)
		if err != nil {
			return nil, err
		}
		real, err := filepath.EvalSymlinks(abs)
		if err != nil {
			real = abs
		}
		canon = append(canon, filepath.Clean(real))
	}
	switch confirmMode {
	case "", "auto":
		confirmMode = "auto"
	case "always", "never":
	default:
		return nil, fmt.Errorf("无效 write_confirm %q (可选: auto/always/never)", confirmMode)
	}
	return &FilePolicy{roots: canon, mode: confirmMode}, nil
}

// resolvePath canonicalizes p against the filesystem so symlinks and ".." are
// resolved before any permission check. For a not-yet-existing write target the
// parent directory is canonicalized instead.
func resolvePath(p string) (string, error) {
	if !filepath.IsAbs(p) {
		abs, err := filepath.Abs(p)
		if err != nil {
			return "", err
		}
		p = abs
	}
	if real, err := filepath.EvalSymlinks(p); err == nil {
		return filepath.Clean(real), nil
	}
	dir, err := filepath.EvalSymlinks(filepath.Dir(p))
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, filepath.Base(p)), nil
}

// allowed reports whether path is inside one of the policy roots.
func (p *FilePolicy) allowed(path string) bool {
	for _, r := range p.roots {
		rel, err := filepath.Rel(r, path)
		if err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel) {
			return true
		}
	}
	return false
}

// readFile reads a text file, truncated to maxFileBytes with a marker.
func (p *FilePolicy) readFile(path string) (string, error) {
	target, err := resolvePath(path)
	if err != nil {
		return "", err
	}
	if !p.allowed(target) {
		return "", fmt.Errorf("路径 %s 不在允许的 file_roots 内", target)
	}
	f, err := os.Open(target)
	if err != nil {
		return "", err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return "", err
	}
	if st.IsDir() {
		return "", fmt.Errorf("%s 是目录,只能读取文件", target)
	}
	data, err := io.ReadAll(io.LimitReader(f, maxFileBytes+1))
	if err != nil {
		return "", err
	}
	truncated := len(data) > maxFileBytes
	if truncated {
		data = data[:maxFileBytes]
	}
	out := string(data)
	if truncated {
		out += truncMarker
	}
	return out, nil
}

// writeFile writes content after a permission check and write confirmation.
func (p *FilePolicy) writeFile(path, content string) (string, error) {
	target, err := resolvePath(path)
	if err != nil {
		return "", err
	}
	if !p.allowed(target) {
		return "", fmt.Errorf("路径 %s 不在允许的 file_roots 内", target)
	}
	if len(content) > maxFileBytes {
		return "", fmt.Errorf("内容超过 %d 字节", maxFileBytes)
	}
	if !p.confirmWrite(target) {
		return "", fmt.Errorf("写入被拒绝: %s", target)
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return "", err
	}
	if err := os.WriteFile(target, []byte(content), 0o644); err != nil {
		return "", err
	}
	return fmt.Sprintf("已写入 %s (%d 字节)", target, len(content)), nil
}

// confirmWrite applies the mode: never allows, auto/always prompt on a TTY and
// deny otherwise. An injected confirm (tests) short-circuits everything.
func (p *FilePolicy) confirmWrite(path string) bool {
	if p.confirm != nil {
		return p.confirm(path)
	}
	if p.mode == "never" {
		return true
	}
	if !isTTY(os.Stdin) {
		return false
	}
	fmt.Fprintf(os.Stderr, "gw: 允许写入 %s? [y/N] ", path)
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil {
		return false
	}
	line = strings.ToLower(strings.TrimSpace(line))
	return line == "y" || line == "yes"
}

// isTTY reports whether f is a character device (interactive terminal).
func isTTY(f *os.File) bool {
	st, err := f.Stat()
	if err != nil {
		return false
	}
	return st.Mode()&os.ModeCharDevice != 0
}

// DispatchTool executes one model tool call and returns the result string.
func (p *FilePolicy) DispatchTool(call ToolCall) (string, error) {
	var args struct {
		Path    string `json:"path"`
		Content string `json:"content"`
	}
	if err := json.Unmarshal([]byte(call.Function.Arguments), &args); err != nil {
		return "", fmt.Errorf("tool %s 参数解析失败: %w", call.Function.Name, err)
	}
	switch call.Function.Name {
	case "read_file":
		if args.Path == "" {
			return "", fmt.Errorf("read_file 缺少 path")
		}
		return p.readFile(args.Path)
	case "write_file":
		if args.Path == "" {
			return "", fmt.Errorf("write_file 缺少 path")
		}
		return p.writeFile(args.Path, args.Content)
	default:
		return "", fmt.Errorf("未知工具 %q", call.Function.Name)
	}
}
