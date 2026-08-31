package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newTestPolicy(t *testing.T, roots ...string) *FilePolicy {
	t.Helper()
	p, err := newFilePolicy(roots, "auto")
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestReadFileInRoot(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	p := newTestPolicy(t, root)
	out, err := p.readFile(filepath.Join(root, "a.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if out != "hello" {
		t.Fatalf("read = %q", out)
	}
}

func TestReadFileOutsideRoot(t *testing.T) {
	root := t.TempDir()
	secret := filepath.Join(t.TempDir(), "secret.txt")
	if err := os.WriteFile(secret, []byte("secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	p := newTestPolicy(t, root)
	if _, err := p.readFile(secret); err == nil {
		t.Fatal("read outside root must fail")
	}
}

func TestReadFileDotDotEscape(t *testing.T) {
	root := t.TempDir()
	p := newTestPolicy(t, root)
	if _, err := p.readFile(filepath.Join(root, "..", "foo.txt")); err == nil {
		t.Fatal(".. escape must fail")
	}
}

func TestReadFileSymlinkEscape(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(t.TempDir(), "secret.txt")
	if err := os.WriteFile(target, []byte("secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(root, "link")); err != nil {
		t.Skip("symlink not supported")
	}
	p := newTestPolicy(t, root)
	if _, err := p.readFile(filepath.Join(root, "link")); err == nil {
		t.Fatal("symlink escape must fail")
	}
}

func TestReadFileTruncates(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "big.txt"), []byte(strings.Repeat("a", maxFileBytes+100)), 0o644); err != nil {
		t.Fatal(err)
	}
	p := newTestPolicy(t, root)
	out, err := p.readFile(filepath.Join(root, "big.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "[截断") {
		t.Fatal("expected truncation marker")
	}
	if len(out) > maxFileBytes+len(truncMarker) {
		t.Fatalf("out too long: %d", len(out))
	}
}

func TestWriteFileConfirmDeniedNonTTY(t *testing.T) {
	// Force a non-TTY stdin so auto mode denies without prompting.
	old := os.Stdin
	stdin, err := os.CreateTemp(t.TempDir(), "stdin")
	if err != nil {
		t.Fatal(err)
	}
	os.Stdin = stdin
	defer func() { os.Stdin = old }()

	root := t.TempDir()
	p := newTestPolicy(t, root)
	if _, err := p.writeFile(filepath.Join(root, "new.txt"), "hi"); err == nil {
		t.Fatal("non-TTY write must be denied in auto mode")
	}
}

func TestIsTTYRegularFile(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "f")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if isTTY(f) {
		t.Fatal("regular file is not a TTY")
	}
}

func TestWriteFileWithInjectedConfirm(t *testing.T) {
	root := t.TempDir()
	p := newTestPolicy(t, root)
	p.confirm = func(string) bool { return true }
	if _, err := p.writeFile(filepath.Join(root, "new.txt"), "hi"); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(root, "new.txt"))
	if err != nil || string(data) != "hi" {
		t.Fatalf("written = %q, err %v", data, err)
	}
}

func TestWriteFileNeverSkipsConfirm(t *testing.T) {
	root := t.TempDir()
	p, err := newFilePolicy([]string{root}, "never")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.writeFile(filepath.Join(root, "new.txt"), "hi"); err != nil {
		t.Fatalf("never mode should skip confirm: %v", err)
	}
}

func TestWriteFileOutsideRoot(t *testing.T) {
	root := t.TempDir()
	p := newTestPolicy(t, root)
	p.confirm = func(string) bool { return true }
	if _, err := p.writeFile(filepath.Join(t.TempDir(), "x.txt"), "hi"); err == nil {
		t.Fatal("write outside root must fail")
	}
}

func TestDispatchToolUnknown(t *testing.T) {
	p := newTestPolicy(t, t.TempDir())
	if _, err := p.DispatchTool(ToolCall{Function: ToolFunction{Name: "nope", Arguments: "{}"}}); err == nil {
		t.Fatal("unknown tool must fail")
	}
}

func TestNewFilePolicyDefaultRoot(t *testing.T) {
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	p, err := newFilePolicy(nil, "")
	if err != nil {
		t.Fatal(err)
	}
	if !p.allowed(cwd) {
		t.Fatal("default root should be the working directory")
	}
}

func TestNewFilePolicyBadMode(t *testing.T) {
	if _, err := newFilePolicy(nil, "bogus"); err == nil {
		t.Fatal("invalid write_confirm must fail")
	}
}
