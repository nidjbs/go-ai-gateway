package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Snapshot is the deterministic, normalized output of one mock case.
type Snapshot struct {
	Exit   int
	Stdout string
	Files  map[string]string // rel → normalized content
}

// goldenDir returns <root>/<name>.
func goldenDir(root, name string) string { return filepath.Join(root, name) }

func stdPairs(stateDir, workdir string, extra []NormalizeRule) [][2]string {
	pairs := [][2]string{{stateDir, "<state>"}}
	if workdir != "" && workdir != stateDir {
		pairs = append(pairs, [2]string{workdir, "<workdir>"})
	}
	for _, r := range extra {
		if r.Pattern != "" {
			pairs = append(pairs, [2]string{r.Pattern, r.Placeholder})
		}
	}
	return pairs
}

// snapshotOf normalizes a run into a comparable snapshot.
func snapshotOf(c *Case, o *RunOutcome) Snapshot {
	norm := normalizeFunc(stdPairs(o.StateDir, o.Workdir, c.Normalize))
	trim := func(s string) string {
		return sessionIDRe.ReplaceAllString(strings.TrimSpace(norm(s)), "<session>")
	}
	snap := Snapshot{Exit: o.ExitCode, Stdout: trim(o.Stdout), Files: map[string]string{}}
	for rel, content := range o.Files {
		snap.Files[rel] = trim(content)
	}
	return snap
}

// writeGolden persists a snapshot: stdout.txt, exit.txt, files/<rel>.
func writeGolden(root, name string, s Snapshot) error {
	dir := goldenDir(root, name)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "stdout.txt"), []byte(s.Stdout), 0o600); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "exit.txt"), []byte(fmt.Sprintf("%d", s.Exit)), 0o600); err != nil {
		return err
	}
	if len(s.Files) == 0 {
		return nil
	}
	filesDir := filepath.Join(dir, "files")
	if err := os.MkdirAll(filesDir, 0o700); err != nil {
		return err
	}
	rels := make([]string, 0, len(s.Files))
	for rel := range s.Files {
		rels = append(rels, rel)
	}
	sort.Strings(rels)
	for _, rel := range rels {
		p := filepath.Join(filesDir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			return err
		}
		if err := os.WriteFile(p, []byte(s.Files[rel]), 0o600); err != nil {
			return err
		}
	}
	return nil
}

// readGolden loads a snapshot; ok=false when the golden dir is absent.
func readGolden(root, name string) (Snapshot, bool) {
	dir := goldenDir(root, name)
	read := func(f string) (string, bool) {
		b, err := os.ReadFile(filepath.Join(dir, f))
		return string(b), err == nil
	}
	exitStr, ok := read("exit.txt")
	if !ok {
		return Snapshot{}, false
	}
	snap := Snapshot{Files: map[string]string{}}
	fmt.Sscanf(strings.TrimSpace(exitStr), "%d", &snap.Exit)
	if v, ok := read("stdout.txt"); ok {
		snap.Stdout = v
	}
	filesDir := filepath.Join(dir, "files")
	if entries, err := os.ReadDir(filesDir); err == nil {
		for _, e := range entries {
			loadGoldenFiles(filesDir, "", e, snap.Files)
		}
	}
	return snap, true
}

func loadGoldenFiles(base, prefix string, e os.DirEntry, into map[string]string) {
	rel := filepath.Join(prefix, e.Name())
	if e.IsDir() {
		entries, err := os.ReadDir(filepath.Join(base, e.Name()))
		if err == nil {
			for _, sub := range entries {
				loadGoldenFiles(filepath.Join(base, e.Name()), rel, sub, into)
			}
		}
		return
	}
	if b, err := os.ReadFile(filepath.Join(base, e.Name())); err == nil {
		into[rel] = string(b)
	}
}

// compareSnap returns a human diff between got and want; "" when equal.
func compareSnap(got, want Snapshot) string {
	var d strings.Builder
	if got.Exit != want.Exit {
		fmt.Fprintf(&d, "exit: got %d, want %d\n", got.Exit, want.Exit)
	}
	if got.Stdout != want.Stdout {
		fmt.Fprintf(&d, "stdout 不同:\n--- 期望(golden) ---\n%s\n--- 实际 ---\n%s\n", want.Stdout, got.Stdout)
	}
	keys := map[string]bool{}
	for k := range got.Files {
		keys[k] = true
	}
	for k := range want.Files {
		keys[k] = true
	}
	all := make([]string, 0, len(keys))
	for k := range keys {
		all = append(all, k)
	}
	sort.Strings(all)
	for _, k := range all {
		g, gok := got.Files[k]
		w, wok := want.Files[k]
		switch {
		case !wok:
			fmt.Fprintf(&d, "新增产物 %s (golden 无此文件)\n", k)
		case !gok:
			fmt.Fprintf(&d, "缺失产物 %s\n", k)
		case g != w:
			fmt.Fprintf(&d, "产物 %s 不同:\n--- 期望 ---\n%s\n--- 实际 ---\n%s\n", k, w, g)
		}
	}
	return strings.TrimSpace(d.String())
}
