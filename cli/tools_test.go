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

func TestListDirInRoot(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "b.log"), []byte(strings.Repeat("x", 2048)), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	p := newTestPolicy(t, root)
	out, err := p.listDir(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"a.txt", "b.log", "sub", "f  ", "d  "} {
		if !strings.Contains(out, want) {
			t.Fatalf("list_dir 输出缺少 %q:\n%s", want, out)
		}
	}
}

func TestListDirOutsideRoot(t *testing.T) {
	root := t.TempDir()
	p := newTestPolicy(t, root)
	if _, err := p.listDir(t.TempDir()); err == nil {
		t.Fatal("list_dir outside root must fail")
	}
}

func TestListDirNotDirectory(t *testing.T) {
	root := t.TempDir()
	f := filepath.Join(root, "f.txt")
	if err := os.WriteFile(f, []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}
	p := newTestPolicy(t, root)
	if _, err := p.listDir(f); err == nil {
		t.Fatal("list_dir on a file must fail")
	}
}

func TestHumanSize(t *testing.T) {
	cases := map[int64]string{0: "0B", 512: "512B", 1024: "1.0K", 2048: "2.0K", 34 * 1024 * 1024: "34.0M"}
	for in, want := range cases {
		if got := humanSize(in); got != want {
			t.Fatalf("humanSize(%d) = %q, want %q", in, got, want)
		}
	}
}

func TestDeleteFile(t *testing.T) {
	root := t.TempDir()
	f := filepath.Join(root, "f.txt")
	if err := os.WriteFile(f, []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}
	p := newTestPolicy(t, root)
	p.confirm = func(string) bool { return true }
	if _, err := p.deleteFile(f); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(f); !os.IsNotExist(err) {
		t.Fatalf("file still exists: %v", err)
	}
}

func TestDeleteFileDenied(t *testing.T) {
	root := t.TempDir()
	f := filepath.Join(root, "f.txt")
	if err := os.WriteFile(f, []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}
	p := newTestPolicy(t, root)
	p.confirm = func(string) bool { return false }
	if _, err := p.deleteFile(f); err == nil {
		t.Fatal("denied delete must fail")
	}
	if _, err := os.Stat(f); err != nil {
		t.Fatal("file must remain after denied delete")
	}
}

func TestDeleteFileOnDir(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "d")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	p := newTestPolicy(t, root)
	p.confirm = func(string) bool { return true }
	if _, err := p.deleteFile(dir); err == nil {
		t.Fatal("delete_file on a dir must fail")
	}
}

func TestMkdir(t *testing.T) {
	root := t.TempDir()
	p := newTestPolicy(t, root)
	p.confirm = func(string) bool { return true }
	if _, err := p.mkdir(filepath.Join(root, "a", "b")); err != nil {
		t.Fatal(err)
	}
	if st, err := os.Stat(filepath.Join(root, "a", "b")); err != nil || !st.IsDir() {
		t.Fatalf("dir not created: %v", err)
	}
}

func TestDeleteDirRecursive(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "d")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "x.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	p := newTestPolicy(t, root)
	p.confirm = func(string) bool { return true }
	if _, err := p.deleteDir(dir, false); err == nil {
		t.Fatal("non-recursive delete of non-empty dir must fail")
	}
	if _, err := p.deleteDir(dir, true); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("dir still exists: %v", err)
	}
}

func TestDeleteDirOnFile(t *testing.T) {
	root := t.TempDir()
	f := filepath.Join(root, "f.txt")
	if err := os.WriteFile(f, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	p := newTestPolicy(t, root)
	p.confirm = func(string) bool { return true }
	if _, err := p.deleteDir(f, true); err == nil {
		t.Fatal("delete_dir on a file must fail")
	}
}

func TestRename(t *testing.T) {
	root := t.TempDir()
	src := filepath.Join(root, "a.txt")
	dst := filepath.Join(root, "b.txt")
	if err := os.WriteFile(src, []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}
	p := newTestPolicy(t, root)
	p.confirm = func(string) bool { return true }
	if _, err := p.rename(src, dst); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(dst); err != nil || string(data) != "hi" {
		t.Fatalf("renamed file wrong: %q %v", data, err)
	}
}

func TestRenameOutsideRoot(t *testing.T) {
	root := t.TempDir()
	src := filepath.Join(root, "a.txt")
	outside := filepath.Join(t.TempDir(), "b.txt")
	if err := os.WriteFile(src, []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}
	p := newTestPolicy(t, root)
	p.confirm = func(string) bool { return true }
	if _, err := p.rename(src, outside); err == nil {
		t.Fatal("rename to outside root must fail")
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
