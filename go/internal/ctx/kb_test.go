package ctx

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/xjcdw0777/agent-harness/internal/config"
)

func setupRoot(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	config.Root = dir
	kbFPs = nil
}

func TestKBWriteSearchDigest(t *testing.T) {
	setupRoot(t)
	if KBWrite("a1", "fact", "f16 实测真实值 p50 4359B", []string{"f16"}) != "ok" {
		t.Fatal("write failed")
	}
	if KBWrite("a2", "decision", "403 归因待判别实验", nil) != "ok" {
		t.Fatal("write2 failed")
	}
	if KBWrite("a1", "fact", "f16 实测真实值 p50 4359B", nil) != "dup" {
		t.Fatal("dedup failed")
	}
	hits := KBSearch("f16 字节", 5)
	if len(hits) != 1 || hits[0].Agent != "a1" {
		t.Fatalf("search hits=%v", hits)
	}
	d := KBDigest(10)
	if !strings.Contains(d, "4359") || !strings.Contains(d, "判别") {
		t.Fatalf("digest missing entries: %q", d)
	}
}

func TestKBReloadAcrossProcess(t *testing.T) {
	setupRoot(t)
	KBWrite("a1", "finding", "worker 侧无探针", nil)
	kbFPs = nil // 模拟新进程冷启动
	kbLoadedAt = time.Now().Add(-time.Hour)
	if len(KBAll()) != 1 {
		t.Fatal("reload lost entries")
	}
}

func TestKnowledgeExtract(t *testing.T) {
	ks := extractKnowledge("前文 <knowledge>f16 gap 约1800B</knowledge> 中文 <knowledge>第二条</knowledge>")
	if len(ks) != 2 || ks[0] != "f16 gap 约1800B" {
		t.Fatalf("extract=%v", ks)
	}
}

func TestLoadSession(t *testing.T) {
	setupRoot(t)
	// 无档案无 state → 空
	if m := LoadSession("nobody", 8000); len(m) != 0 {
		t.Fatal("expected empty")
	}
	// 写档案 + session state
	Append("a1", mkMsg("user", "旧任务: 解 f16"),
		mkMsg("assistant", "f16=2541B 已实测"))
	os.WriteFile(sessionFile("a1"), []byte(
		`{"seed":"早前摘要: f16 缺口 42%","pins":["<memory>记住 oracle 语义</memory>"]}`), 0o644)
	m := LoadSession("a1", 8000)
	if len(m) < 4 {
		t.Fatalf("seed too short: %d", len(m))
	}
	if !strings.Contains(m[0].Content, "会话复用") {
		t.Fatalf("missing resume marker: %v", m[0])
	}
	var joined string
	for _, x := range m {
		joined += x.Content + "\n"
	}
	if !strings.Contains(joined, "早前摘要") || !strings.Contains(joined, "f16=2541B") ||
		!strings.Contains(joined, "oracle 语义") {
		t.Fatalf("seed incomplete: %q", joined)
	}
}

func mkMsg(role, content string) config.Message {
	return config.Message{Role: role, Content: content}
}

func TestKBRecallInjection(t *testing.T) {
	setupRoot(t)
	if KBRecall("anything", 3) != "" {
		t.Fatal("empty KB should produce empty recall block")
	}
	KBWrite("critic", "finding", "f16 decoded length is 2541 bytes", []string{"minter"})
	KBWrite("critic", "fact", "unrelated weather note", nil)
	blk := KBRecall("f16 size gap", 5)
	if !strings.Contains(blk, "<relevant_memories>") ||
		!strings.Contains(blk, "2541 bytes") {
		t.Fatalf("recall block missing hit: %q", blk)
	}
	if !strings.Contains(blk, "系统注入") {
		t.Fatal("missing injection marker")
	}
}

func TestKBRecallBounded(t *testing.T) {
	setupRoot(t)
	for i := 0; i < 30; i++ {
		KBWrite("a", "fact", "tokengap measurement number "+string(rune('0'+i%10))+string(rune('0'+i/10)), nil)
	}
	blk := KBRecall("tokengap", 5)
	if n := strings.Count(blk, "- ["); n > 5 {
		t.Fatalf("recall unbounded: %d entries", n)
	}
}
