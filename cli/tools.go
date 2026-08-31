package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// maxFileBytes caps read_file output and write_file input.
const maxFileBytes = 64 * 1024

// truncMarker is appended when read_file output is cut at maxFileBytes.
const truncMarker = "\n… [截断,超过 64KiB]"

// agentTools returns the tool set advertised to the model in agent sessions.
func agentTools() []ToolSpec {
	return []ToolSpec{toolReadFile, toolWriteFile, toolListDir, toolDeleteFile, toolMkdir, toolDeleteDir, toolRename}
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

var toolListDir = ToolSpec{
	Type: "function",
	Function: ToolSpecFunction{
		Name:        "list_dir",
		Description: "List the entries of a directory within the allowed roots, one per line: d/ or f prefix, name, size, modified time.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path": map[string]any{"type": "string", "description": "Path to the directory to list (default: the working directory)."},
			},
		},
	},
}

var toolDeleteFile = ToolSpec{
	Type: "function",
	Function: ToolSpecFunction{
		Name:        "delete_file",
		Description: "Delete a file within the allowed roots. Requires confirmation.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path": map[string]any{"type": "string", "description": "Path to the file to delete."},
			},
			"required": []string{"path"},
		},
	},
}

var toolMkdir = ToolSpec{
	Type: "function",
	Function: ToolSpecFunction{
		Name:        "mkdir",
		Description: "Create a directory within the allowed roots, creating parent dirs as needed.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path": map[string]any{"type": "string", "description": "Path to the directory to create."},
			},
			"required": []string{"path"},
		},
	},
}

var toolDeleteDir = ToolSpec{
	Type: "function",
	Function: ToolSpecFunction{
		Name:        "delete_dir",
		Description: "Delete a directory within the allowed roots. Empty dirs are removed; set recursive=true to remove contents too. Requires confirmation.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path":      map[string]any{"type": "string", "description": "Path to the directory to delete."},
				"recursive": map[string]any{"type": "boolean", "description": "Remove contents recursively."},
			},
			"required": []string{"path"},
		},
	},
}

var toolRename = ToolSpec{
	Type: "function",
	Function: ToolSpecFunction{
		Name:        "rename",
		Description: "Move or rename a file or directory within the allowed roots. Requires confirmation.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"src": map[string]any{"type": "string", "description": "Source path."},
				"dst": map[string]any{"type": "string", "description": "Destination path."},
			},
			"required": []string{"src", "dst"},
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
// resolved before any permission check. For a not-yet-existing target (mkdir,
// write, rename dst) it canonicalizes the deepest existing ancestor and
// re-attaches the missing suffix, so nested paths resolve to the same real path.
func resolvePath(p string) (string, error) {
	if !filepath.IsAbs(p) {
		abs, err := filepath.Abs(p)
		if err != nil {
			return "", err
		}
		p = abs
	}
	dir := p
	var suffix []string
	for {
		if real, err := filepath.EvalSymlinks(dir); err == nil {
			return filepath.Join(append([]string{real}, suffix...)...), nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("无法解析路径 %s", p)
		}
		suffix = append([]string{filepath.Base(dir)}, suffix...)
		dir = parent
	}
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

// listDir lists a directory's entries within the allowed roots, sorted by name.
func (p *FilePolicy) listDir(path string) (string, error) {
	if path == "" {
		path = "."
	}
	target, err := resolvePath(path)
	if err != nil {
		return "", err
	}
	if !p.allowed(target) {
		return "", fmt.Errorf("路径 %s 不在允许的 file_roots 内", target)
	}
	st, err := os.Stat(target)
	if err != nil {
		return "", err
	}
	if !st.IsDir() {
		return "", fmt.Errorf("%s 不是目录", target)
	}
	entries, err := os.ReadDir(target)
	if err != nil {
		return "", err
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	var b strings.Builder
	for _, e := range entries {
		info, err := e.Info()
		if err != nil {
			continue
		}
		typ := "f"
		size := humanSize(info.Size())
		if info.IsDir() {
			typ = "d"
			size = "-"
		}
		fmt.Fprintf(&b, "%s  %s  %10s  %s\n", typ, e.Name(), size, info.ModTime().Format("2006-01-02 15:04"))
	}
	if b.Len() == 0 {
		return fmt.Sprintf("%s (空目录)", target), nil
	}
	return strings.TrimSpace(b.String()), nil
}

// humanSize renders a byte count in a compact human form (1.2K, 34M...).
func humanSize(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%dB", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f%c", float64(n)/float64(div), "KMGTPE"[exp])
}

// confirmAction gates any mutation (write, mkdir, rename, delete) on the mode:
// never allows, auto/always prompt on a TTY and deny otherwise. An injected
// confirm (tests) short-circuits everything.
func (p *FilePolicy) confirmAction(action, target string) bool {
	if p.confirm != nil {
		return p.confirm(target)
	}
	if p.mode == "never" {
		return true
	}
	if !isTTY(os.Stdin) {
		return false
	}
	fmt.Fprintf(os.Stderr, "gw: 允许%s %s? [y/N] ", action, target)
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil {
		return false
	}
	line = strings.ToLower(strings.TrimSpace(line))
	return line == "y" || line == "yes"
}

func (p *FilePolicy) confirmWrite(path string) bool {
	return p.confirmAction("写入", path)
}

// deleteFile removes a file after permission + confirmation.
func (p *FilePolicy) deleteFile(path string) (string, error) {
	target, err := resolvePath(path)
	if err != nil {
		return "", err
	}
	if !p.allowed(target) {
		return "", fmt.Errorf("路径 %s 不在允许的 file_roots 内", target)
	}
	if !p.confirmAction("删除", target) {
		return "", fmt.Errorf("删除被拒绝: %s", target)
	}
	st, err := os.Lstat(target)
	if err != nil {
		return "", err
	}
	if st.IsDir() {
		return "", fmt.Errorf("%s 是目录,请用 delete_dir", target)
	}
	if err := os.Remove(target); err != nil {
		return "", err
	}
	return fmt.Sprintf("已删除 %s", target), nil
}

// mkdir creates a directory after permission + confirmation.
func (p *FilePolicy) mkdir(path string) (string, error) {
	target, err := resolvePath(path)
	if err != nil {
		return "", err
	}
	if !p.allowed(target) {
		return "", fmt.Errorf("路径 %s 不在允许的 file_roots 内", target)
	}
	if !p.confirmAction("创建目录", target) {
		return "", fmt.Errorf("创建目录被拒绝: %s", target)
	}
	if err := os.MkdirAll(target, 0o755); err != nil {
		return "", err
	}
	return fmt.Sprintf("已创建目录 %s", target), nil
}

// deleteDir removes a directory (recursive only when requested) after confirmation.
func (p *FilePolicy) deleteDir(path string, recursive bool) (string, error) {
	target, err := resolvePath(path)
	if err != nil {
		return "", err
	}
	if !p.allowed(target) {
		return "", fmt.Errorf("路径 %s 不在允许的 file_roots 内", target)
	}
	st, err := os.Lstat(target)
	if err != nil {
		return "", err
	}
	if !st.IsDir() {
		return "", fmt.Errorf("%s 不是目录,请用 delete_file", target)
	}
	if !p.confirmAction("删除目录", target) {
		return "", fmt.Errorf("删除目录被拒绝: %s", target)
	}
	if recursive {
		if err := os.RemoveAll(target); err != nil {
			return "", err
		}
		return fmt.Sprintf("已删除目录 %s(含内容)", target), nil
	}
	if err := os.Remove(target); err != nil {
		return "", fmt.Errorf("目录非空,删除失败: %s", target)
	}
	return fmt.Sprintf("已删除空目录 %s", target), nil
}

// rename moves a file or directory after both paths pass permission + confirm.
func (p *FilePolicy) rename(src, dst string) (string, error) {
	from, err := resolvePath(src)
	if err != nil {
		return "", err
	}
	to, err := resolvePath(dst)
	if err != nil {
		return "", err
	}
	if !p.allowed(from) || !p.allowed(to) {
		return "", fmt.Errorf("路径 %s -> %s 超出允许的 file_roots", from, to)
	}
	if !p.confirmAction("移动", to) {
		return "", fmt.Errorf("移动被拒绝: %s -> %s", from, to)
	}
	if err := os.MkdirAll(filepath.Dir(to), 0o755); err != nil {
		return "", err
	}
	if err := os.Rename(from, to); err != nil {
		return "", err
	}
	return fmt.Sprintf("已移动 %s -> %s", from, to), nil
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
		Path      string `json:"path"`
		Content   string `json:"content"`
		Recursive bool   `json:"recursive"`
		Src       string `json:"src"`
		Dst       string `json:"dst"`
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
	case "list_dir":
		return p.listDir(args.Path)
	case "delete_file":
		if args.Path == "" {
			return "", fmt.Errorf("delete_file 缺少 path")
		}
		return p.deleteFile(args.Path)
	case "mkdir":
		if args.Path == "" {
			return "", fmt.Errorf("mkdir 缺少 path")
		}
		return p.mkdir(args.Path)
	case "delete_dir":
		if args.Path == "" {
			return "", fmt.Errorf("delete_dir 缺少 path")
		}
		return p.deleteDir(args.Path, args.Recursive)
	case "rename":
		if args.Src == "" || args.Dst == "" {
			return "", fmt.Errorf("rename 缺少 src/dst")
		}
		return p.rename(args.Src, args.Dst)
	default:
		return "", fmt.Errorf("未知工具 %q", call.Function.Name)
	}
}
