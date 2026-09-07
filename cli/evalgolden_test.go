package main

import (
	"testing"
)

func TestGoldenRoundTrip(t *testing.T) {
	root := t.TempDir()
	snap := Snapshot{
		Exit:   0,
		Stdout: "你好",
		Files:  map[string]string{"prompts/x.md": "---\nname: x\n---\n"},
	}
	if err := writeGolden(root, "ask-hello", snap); err != nil {
		t.Fatal(err)
	}
	got, ok := readGolden(root, "ask-hello")
	if !ok {
		t.Fatal("golden missing")
	}
	if got.Exit != snap.Exit || got.Stdout != snap.Stdout {
		t.Fatalf("round trip = %+v", got)
	}
	if got.Files["prompts/x.md"] != snap.Files["prompts/x.md"] {
		t.Fatalf("file = %q", got.Files["prompts/x.md"])
	}
	if _, ok := readGolden(root, "nope"); ok {
		t.Fatal("read of missing golden must be !ok")
	}
}

func TestSnapshotOfNormalizes(t *testing.T) {
	c := &Case{Name: "x"}
	o := &RunOutcome{
		ExitCode: 0,
		Stdout:   "跑 /tmp/abc/state/sessions/20260101T000000-00000000/x\n",
		StateDir: "/tmp/abc/state",
		Workdir:  "/tmp/abc/work",
		Files:    map[string]string{},
	}
	s := snapshotOf(c, o)
	if s.Stdout != "跑 <state>/sessions/<session>/x" {
		t.Fatalf("normalized stdout = %q", s.Stdout)
	}
}

func TestCompareSnap(t *testing.T) {
	a := Snapshot{Exit: 0, Stdout: "A"}
	b := Snapshot{Exit: 1, Stdout: "B"}
	if d := compareSnap(a, a); d != "" {
		t.Fatalf("equal diff = %q", d)
	}
	if d := compareSnap(a, b); d == "" {
		t.Fatal("diff must be non-empty")
	}
}
