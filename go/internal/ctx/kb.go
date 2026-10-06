package ctx

// 提炼知识库(会话+记忆二合一的另一半):
//   - agent 可主动 kb_write 沉淀结论(fact/decision/finding)
//   - 压缩器把 <knowledge>...</knowledge> 块自动提炼入库
//   - KBDigest 注入新 run 的 system, 实现跨会话记忆复用
//
// 落盘 <Root>/archive/_kb.jsonl — append-only, 指纹去重。

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/xjcdw0777/agent-harness/internal/config"
)

type KBEntry struct {
	TS    string   `json:"ts"`
	Agent string   `json:"agent"`
	Kind  string   `json:"kind"` // fact | decision | finding | distilled
	Text  string   `json:"text"`
	Tags  []string `json:"tags,omitempty"`
	FP    string   `json:"fp"`
}

var kbMu sync.Mutex
var kbFPs map[string]bool
var kbLoadedAt time.Time

func kbFile() string {
	return filepath.Join(dir(), "_kb.jsonl")
}

func kbLoad() {
	b, err := os.ReadFile(kbFile())
	kbFPs = map[string]bool{}
	if err != nil {
		return
	}
	for _, line := range strings.Split(string(b), "\n") {
		var e KBEntry
		if json.Unmarshal([]byte(line), &e) == nil && e.FP != "" {
			kbFPs[e.FP] = true
		}
	}
	kbLoadedAt = time.Now()
}

func kbEnsureLoaded() {
	st, err := os.Stat(kbFile())
	if kbFPs == nil || (err == nil && st.ModTime().After(kbLoadedAt)) {
		kbLoad()
	}
}

func fpText(s string) string {
	h := sha256.Sum256([]byte(strings.TrimSpace(s)))
	return hex.EncodeToString(h[:])[:24]
}

// KBWrite: 沉淀一条知识;重复内容(fp)返回 dup。
func KBWrite(agent, kind, text string, tags []string) string {
	text = strings.TrimSpace(text)
	if text == "" || agent == "" {
		return "empty"
	}
	if kind == "" {
		kind = "fact"
	}
	fp := fpText(agent + "|" + text)
	kbMu.Lock()
	defer kbMu.Unlock()
	kbEnsureLoaded()
	if kbFPs[fp] {
		return "dup"
	}
	e := KBEntry{TS: time.Now().Format("2006-01-02 15:04:05"),
		Agent: agent, Kind: kind, Text: text, Tags: tags, FP: fp}
	f, err := os.OpenFile(kbFile(), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return "write error: " + err.Error()
	}
	json.NewEncoder(f).Encode(e)
	f.Close()
	kbFPs[fp] = true
	return "ok"
}

// KBAll: 全量条目(时间序)。
func KBAll() []KBEntry {
	kbMu.Lock()
	defer kbMu.Unlock()
	kbEnsureLoaded()
	b, err := os.ReadFile(kbFile())
	if err != nil {
		return nil
	}
	var out []KBEntry
	for _, line := range strings.Split(string(b), "\n") {
		var e KBEntry
		if json.Unmarshal([]byte(line), &e) == nil && e.Text != "" {
			out = append(out, e)
		}
	}
	return out
}

// KBSearch: 分词计分检索,命中按时间倒序。
func KBSearch(query string, limit int) []KBEntry {
	if limit <= 0 {
		limit = 8
	}
	terms := tokenize(query)
	type scored struct {
		e KBEntry
		s int
	}
	var hits []scored
	for _, e := range KBAll() {
		low := strings.ToLower(e.Text + " " + e.Kind + " " +
			strings.Join(e.Tags, " "))
		s := 0
		for _, t := range terms {
			s += strings.Count(low, t)
		}
		if s > 0 {
			hits = append(hits, scored{e, s})
		}
	}
	sort.Slice(hits, func(i, j int) bool {
		if hits[i].s != hits[j].s {
			return hits[i].s > hits[j].s
		}
		return hits[i].e.TS > hits[j].e.TS
	})
	var out []KBEntry
	for i, h := range hits {
		if i >= limit {
			break
		}
		out = append(out, h.e)
	}
	return out
}

// KBRecall: 面向当前 prompt 的主动召回(Anuma 式 pre-load)。
// 返回可直接拼进 user 消息的注入块;无命中返回 ""。
func KBRecall(prompt string, limit int) string {
	hits := KBSearch(prompt, limit)
	if len(hits) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("[系统注入-相关记忆(非用户输入,静默使用,勿复述)]\n")
	b.WriteString("<relevant_memories>\n")
	for _, e := range hits {
		b.WriteString("- [" + e.Kind + "] " + e.Text +
			" (" + e.Agent + ", " + e.TS + ")\n")
	}
	b.WriteString("</relevant_memories>")
	return b.String()
}

// KBDigest: 最近 N 条的紧凑渲染,注入 system 简报。
func KBDigest(limit int) string {
	all := KBAll()
	if len(all) == 0 {
		return ""
	}
	if limit <= 0 {
		limit = 15
	}
	if len(all) > limit {
		all = all[len(all)-limit:]
	}
	var b strings.Builder
	for _, e := range all {
		b.WriteString("- [" + e.Kind + "] " + e.Text + " (" + e.Agent + ", " + e.TS + ")\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

// extractKnowledge: 从文本提取 <knowledge>...</knowledge> 块。
func extractKnowledge(text string) []string {
	var out []string
	rest := text
	for {
		i := strings.Index(rest, "<knowledge>")
		if i < 0 {
			break
		}
		rest = rest[i+len("<knowledge>"):]
		j := strings.Index(rest, "</knowledge>")
		if j < 0 {
			break
		}
		if s := strings.TrimSpace(rest[:j]); s != "" {
			out = append(out, s)
		}
		rest = rest[j+len("</knowledge>"):]
	}
	return out
}

// kbDistill: 压缩时自动提炼 — 扫描段落消息的 <knowledge> 块入库。
func kbDistill(agent string, seg []config.Message) {
	for _, m := range seg {
		for _, k := range extractKnowledge(m.Content) {
			KBWrite(agent, "distilled", k, nil)
		}
	}
}
