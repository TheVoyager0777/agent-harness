package core

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func busReset() {
	busMu.Lock()
	busSubs = nil
	busHist = nil
	busMu.Unlock()
}

func TestFnmatch(t *testing.T) {
	cases := []struct {
		pat, s string
		want   bool
	}{
		{"artifact.written", "artifact.written", true},
		{"artifact.*", "artifact.written", true},
		{"*.written", "artifact.written", true},
		{"task.t?", "task.t1", true},
		{"task.t?", "task.t12", false},
		{"a.b.c", "a.b", false},
		{"*", "anything.at.all", true},
	}
	for _, c := range cases {
		if got := Fnmatch(c.pat, c.s); got != c.want {
			t.Errorf("Fnmatch(%q,%q)=%v want %v", c.pat, c.s, got, c.want)
		}
	}
}

func TestBusPubWait(t *testing.T) {
	busReset()
	BusSub("critic", "artifact.written", "验收")
	// BusWait 只推调用后的新事件 → 并发 pub
	done := make(chan *BusEvent)
	go func() { done <- BusWait("critic", "artifact.*", 2*time.Second) }()
	time.Sleep(50 * time.Millisecond)
	BusPub("builder", "artifact.written", map[string]any{"file": "x.go"})
	ev := <-done
	if ev == nil {
		t.Fatal("critic 应收到 artifact.written")
	}
	if ev.Event != "artifact.written" || ev.From != "builder" {
		t.Fatalf("ev=%+v", ev)
	}
	// 超时路径
	if ev := BusWait("nobody", "never.happens", 300*time.Millisecond); ev != nil {
		t.Fatal("无事件应 nil")
	}
}

func TestStateMachine(t *testing.T) {
	dir := t.TempDir()
	run := &Run{Dir: dir, Mode: "t", Usage: map[string]int{}}
	run.SM = NewStateMachine(run)
	for _, st := range []string{"boot", "idle", "thinking", "tooling",
		"waiting", "done", "error", "stopped"} {
		run.SM.Set("a1", st, "d-"+st)
		if got := run.SM.Snap()["a1"].State; got != st {
			t.Fatalf("set %q -> got %q", st, got)
		}
	}
	// 非法状态降级 idle
	run.SM.Set("a1", "bogus", "")
	if run.SM.Snap()["a1"].State != "idle" {
		t.Fatal("非法状态应降级 idle")
	}
	// state.json 落盘
	b, err := os.ReadFile(filepath.Join(dir, "state.json"))
	if err != nil || len(b) == 0 {
		t.Fatal("state.json 未落盘")
	}
}

func TestSayTranscript(t *testing.T) {
	dir := t.TempDir()
	run := &Run{Dir: dir, Mode: "test",
		Usage:   map[string]int{},
		transcr: []string{"# test"}}
	run.SM = NewStateMachine(run)
	run.Say("alice", "hello world", []string{"f.go"})
	b, _ := os.ReadFile(filepath.Join(dir, "transcript.md"))
	s := string(b)
	if !strCont(s, "alice") || !strCont(s, "hello world") || !strCont(s, "f.go") {
		t.Fatalf("transcript 缺内容: %s", s)
	}
}

func TestExtractArtifacts(t *testing.T) {
	dir := t.TempDir()
	run := &Run{Dir: dir, Mode: "test", Usage: map[string]int{}}
	run.SM = NewStateMachine(run)
	text := "看下这段:\n```file:out/x.go\npackage x\n```\n完了"
	wrote := run.ExtractArtifacts(text)
	if len(wrote) != 1 {
		t.Fatalf("应提取 1 个 artifact, got %v", wrote)
	}
	p := filepath.Join(dir, "artifacts", "out", "x.go")
	if b, err := os.ReadFile(p); err != nil || !strCont(string(b), "package x") {
		t.Fatalf("artifact 未落盘: %v", err)
	}
}

func strCont(s, sub string) bool {
	return len(sub) == 0 || (len(s) >= len(sub) && indexStr(s, sub) >= 0)
}

func indexStr(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
