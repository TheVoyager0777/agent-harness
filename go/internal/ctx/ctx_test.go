package ctx

import (
	"testing"
	"time"

	"github.com/xjcdw0777/agent-harness/internal/config"
)

func TestPinSpans(t *testing.T) {
	text := "前言\n<system-reminder>重要规则123</system-reminder>\n中段\n<memory>记住这个</memory>\n尾"
	spans := pinSpans(text)
	if len(spans) != 2 {
		t.Fatalf("spans=%d", len(spans))
	}
	if text[spans[0].start:spans[0].end] != "<system-reminder>重要规则123</system-reminder>" {
		t.Fatalf("span0=%q", text[spans[0].start:spans[0].end])
	}
}

func TestExtractPins(t *testing.T) {
	msgs := []msgView{
		{role: "user", content: "任务: <system-reminder>规则A</system-reminder> 继续"},
		{role: "assistant", content: "普通回复"},
		{role: "user", content: "<system-reminder>规则A</system-reminder>"},
	}
	rest, ps := extractPins(msgs)
	pins := ps.list()
	if len(pins) != 1 { // 同指纹去重
		t.Fatalf("pins=%d", len(pins))
	}
	if len(rest) != 3 { // pin 剥出但消息保留(有残留文本)
		t.Fatalf("rest=%d", len(rest))
	}
}

func TestEstTokens(t *testing.T) {
	if estTokens("", "m") != 0 {
		t.Fatal("空串")
	}
	n := estTokens("hello world", "glm-5.3")
	if n <= 0 {
		t.Fatal("应>0")
	}
	// CJK 模型系数
	n2 := estTokens("中文测试字符", "glm-5-2-1m")
	if n2 <= 0 || n2 > 60 {
		t.Fatalf("CJK est 异常: %d", n2)
	}
}

func setupArchive(t *testing.T) string {
	d := t.TempDir()
	config.Root = d
	return d
}

func TestArchiveRoundtrip(t *testing.T) {
	setupArchive(t)
	Append("a1",
		config.Message{Role: "user", Content: "讨论 f16 字段结构"},
		config.Message{Role: "assistant", Content: "f16 是 BotGuard 加密块"})
	Append("a1", config.Message{Role: "user", Content: "下一步?"})

	msgs := ReadAll("a1")
	if len(msgs) != 3 {
		t.Fatalf("msgs=%d", len(msgs))
	}
	hits := Search("a1", "f16 BotGuard", 5)
	if len(hits) == 0 {
		t.Fatal("recall 无命中")
	}
	if hits[0].Snippet == "" {
		t.Fatal("空 snippet")
	}
	seg := Read("a1", 0, 1)
	if len(seg) != 2 || seg[0].Role != "user" {
		t.Fatalf("seg=%v", seg)
	}
}

// 0阻塞压缩: Apply 立即返回, 摘要在后台完成后下一 turn 换入
func TestCompactorZeroBlock(t *testing.T) {
	a := &config.Agent{Name: "a1", Model: "m"}
	done := make(chan string, 1)
	c := NewCompactor(a, func(msgs []config.Message) (string, error) {
		done <- "called"
		return "<summary>早期讨论了 f16 结构</summary>", nil
	})
	Conf.MaxEst = 100
	Conf.KeepRecent = 2
	Conf.ChunkEst = 100000

	var hist []config.Message
	for i := 0; i < 10; i++ {
		hist = append(hist,
			config.Message{Role: "user", Content: "这是一段足够长的用户消息用来撑过压缩阈值 padding-padding-padding"},
			config.Message{Role: "assistant", Content: "回复"})
	}
	// Apply 立即返回, 不阻塞(摘要在后台跑)
	t0 := time.Now()
	h2 := c.Apply(hist)
	if time.Since(t0) > 200*time.Millisecond {
		t.Fatal("Apply 阻塞了")
	}
	if len(h2) != len(hist) {
		t.Fatal("首 turn 应返回原 history")
	}
	<-done // 后台确实起了压缩
	time.Sleep(50 * time.Millisecond)
	// 下一 turn: 摘要换入
	h3 := c.Apply(hist)
	if len(h3) > len(hist) {
		t.Fatalf("压缩后应更短: %d", len(h3))
	}
	if h3[0].Role != "user" || !cont(h3[0].Content, "摘要") {
		t.Fatalf("首条应为摘要消息: %q", h3[0].Content[:80])
	}
}

func cont(s, sub string) bool { return len(s) > 0 && indexOf(s, sub) >= 0 }
func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
