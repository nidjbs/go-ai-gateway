package main

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// Search result caps: keep tool output bounded and deterministic.
const (
	searchMaxFileSize  = 16 << 20 // files larger than this are skipped by search_text
	searchMaxFiles     = 50
	searchLinesPerFile = 5
	findMaxEntries     = 200
	tailDefaultLines   = 200
	tailMaxLines       = 5000
	tailReadChunk      = 64 << 10
	tailMaxRead        = 8 << 20 // stop scanning back for a pathological file
)

// maxResultLine caps a single search_text match line in the output.
const maxResultLine = 240

var errResultCap = errors.New("result cap reached")

// resolveBase canonicalizes base and requires it be an allowed directory.
func (p *FilePolicy) resolveBase(path string) (string, error) {
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
	return target, nil
}

// findFiles lists files under base whose name matches the glob (case-insensitive),
// skipping dot-directories (.git, .venv...) and never following symlinks. Paths
// that resolve outside the allowed roots are excluded. Output is relative to base.
func (p *FilePolicy) findFiles(name, base string) (string, error) {
	if name == "" {
		return "", fmt.Errorf("find_files 缺少 name")
	}
	root, err := p.resolveBase(base)
	if err != nil {
		return "", err
	}
	lowerName := strings.ToLower(name)
	var out []string
	filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		if rel == "." {
			return nil
		}
		if d.IsDir() {
			if strings.HasPrefix(d.Name(), ".") {
				return filepath.SkipDir
			}
			return nil
		}
		if d.Type()&os.ModeSymlink != 0 || !p.allowed(path) {
			return nil
		}
		if ok, _ := filepath.Match(name, d.Name()); !ok {
			if ok, _ = filepath.Match(lowerName, strings.ToLower(d.Name())); !ok {
				return nil
			}
		}
		out = append(out, rel)
		if len(out) >= findMaxEntries {
			return errResultCap
		}
		return nil
	})
	if len(out) == 0 {
		return "(未找到匹配的文件)", nil
	}
	text := strings.Join(out, "\n")
	if len(out) >= findMaxEntries {
		text += fmt.Sprintf("\n… (已达上限 %d 条,结果已截断)", findMaxEntries)
	}
	return text, nil
}

// searchText scans files under base for pattern: substring (case-insensitive) or
// RE2 regex. Skips dot-directories, symlinks, binary and oversized files. Output
// is "<rel>:<line>: <text>" with per-file and total caps.
func (p *FilePolicy) searchText(pattern, base string, isRegex bool) (string, error) {
	if pattern == "" {
		return "", fmt.Errorf("search_text 缺少 pattern")
	}
	var re *regexp.Regexp
	if isRegex {
		var err error
		if re, err = regexp.Compile(pattern); err != nil {
			return "", fmt.Errorf("search_text 正则非法: %v", err)
		}
	}
	root, err := p.resolveBase(base)
	if err != nil {
		return "", err
	}
	lowerPat := strings.ToLower(pattern)
	var b strings.Builder
	files := 0
	hitCount := 0
	filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		if rel == "." {
			return nil
		}
		if d.IsDir() {
			if strings.HasPrefix(d.Name(), ".") {
				return filepath.SkipDir
			}
			return nil
		}
		if d.Type()&os.ModeSymlink != 0 || !p.allowed(path) {
			return nil
		}
		info, err := d.Info()
		if err != nil || info.Size() > searchMaxFileSize {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil || bytes.IndexByte(data, 0) >= 0 { // skip binary
			return nil
		}
		perFile := 0
		for i, line := range strings.Split(string(data), "\n") {
			if len(line) > maxResultLine {
				line = line[:maxResultLine] + "…"
			}
			match := false
			if re != nil {
				match = re.MatchString(line)
			} else {
				match = strings.Contains(strings.ToLower(line), lowerPat)
			}
			if !match {
				continue
			}
			fmt.Fprintf(&b, "%s:%d: %s\n", rel, i+1, strings.TrimSpace(line))
			perFile++
			if perFile >= searchLinesPerFile {
				b.WriteString(fmt.Sprintf("%s:… (该文件仅展示前 %d 处命中)\n", rel, perFile))
				break
			}
		}
		if perFile > 0 {
			files++
			hitCount += perFile
			if files >= searchMaxFiles {
				b.WriteString("… (命中文件数已达上限,结果截断)\n")
				return errResultCap
			}
		}
		return nil
	})
	if hitCount == 0 {
		return "(未找到匹配的内容)", nil
	}
	return strings.TrimRight(b.String(), "\n"), nil
}

// tailFile returns the last lines lines of a text file (default 200), reading
// only the tail of oversized files instead of the whole stream.
func (p *FilePolicy) tailFile(path string, lines int) (string, error) {
	if lines <= 0 {
		lines = tailDefaultLines
	}
	if lines > tailMaxLines {
		lines = tailMaxLines
	}
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
	var data []byte
	fromStart := true
	if st.Size() <= tailReadChunk {
		data, err = os.ReadFile(target)
		if err != nil {
			return "", err
		}
	} else {
		// Read backwards in chunks until we have enough newlines or reach 0.
		off := st.Size()
		for {
			n := int64(tailReadChunk)
			if off < n {
				n = off
			}
			off -= n
			buf := make([]byte, n)
			if _, err := f.ReadAt(buf, off); err != nil {
				break
			}
			data = append(buf, data...)
			if bytes.Count(buf, []byte("\n")) >= lines || off == 0 || len(data) > tailMaxRead {
				if off == 0 {
					fromStart = true
				} else {
					fromStart = false
				}
				break
			}
		}
	}
	body := strings.TrimSuffix(string(data), "\n")
	kept := strings.Split(body, "\n")
	if len(kept) > lines {
		kept = kept[len(kept)-lines:]
	}
	var sb strings.Builder
	if !fromStart {
		sb.WriteString(fmt.Sprintf("… (文件较大,仅返回末尾 %d 行)\n", len(kept)))
	}
	sb.WriteString(strings.Join(kept, "\n"))
	return sb.String(), nil
}
