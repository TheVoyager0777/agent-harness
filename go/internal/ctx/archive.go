package ctx

// 会话档案库: 每 agent 一份 append-only JSONL — 压缩输入源 + recall 检索域。
// <Root>/archive/<agent>.jsonl ; 子进程与主进程同盘共享。

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/xjcdw0777/agent-harness/internal/config"
)

type archMsg struct {
	Role    string `json:"role"`
	Content string `json:"content"`
	T       string `json:"t"`
}

var archMu sync.Mutex

func dir() string {
	d := filepath.Join(config.Root, "archive")
	os.MkdirAll(d, 0o755)
	return d
}

func file(agent string) string {
	safe := strings.Map(func(r rune) rune {
		if r == '/' || r == 92 || r == ':' {
			return '_'
		}
		return r
	}, agent)
	return filepath.Join(dir(), safe+".jsonl")
}

// Append: 追加消息到档案(append-only, 不做去重——档案即原始账本)。
func Append(agent string, msgs ...config.Message) {
	if agent == "" {
		return
	}
	archMu.Lock()
	defer archMu.Unlock()
	f, err := os.OpenFile(file(agent), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	enc := json.NewEncoder(f)
	for _, m := range msgs {
		enc.Encode(archMsg{Role: m.Role, Content: m.Content,
			T: now()})
	}
}

func now() string {
	return timeNow()
}

var timeNow = func() string {
	return time.Now().Format("15:04:05")
}

// ReadAll: 全量档案(压缩/recall 用)。
func ReadAll(agent string) []archMsg {
	b, err := os.ReadFile(file(agent))
	if err != nil {
		return nil
	}
	var out []archMsg
	for _, line := range strings.Split(string(b), "\n") {
		if line == "" {
			continue
		}
		var m archMsg
		if json.Unmarshal([]byte(line), &m) == nil {
			out = append(out, m)
		}
	}
	return out
}

type Hit struct {
	Idx     int    `json:"idx"`
	Role    string `json:"role"`
	Snippet string `json:"snippet"`
	T       string `json:"t"`
}

// Search: 关键词检索(与 ctxsvc recall 同构: 分词命中计分)。
func Search(agent, query string, limit int) []Hit {
	msgs := ReadAll(agent)
	if limit <= 0 {
		limit = 8
	}
	terms := tokenize(query)
	var scored []Hit
	for i, m := range msgs {
		score := 0
		low := strings.ToLower(m.Content)
		for _, t := range terms {
			score += strings.Count(low, t)
		}
		if score > 0 {
			snip := m.Content
			if len(snip) > 300 {
				snip = snip[:300] + "…"
			}
			scored = append(scored, Hit{i, m.Role, snip, m.T})
		}
	}
	sort.Slice(scored, func(i, j int) bool { return scored[i].Idx > scored[j].Idx })
	if len(scored) > limit {
		scored = scored[:limit]
	}
	return scored
}

// Read: 按索引区间回读原文(recall 二级取件)。
func Read(agent string, from, to int) []archMsg {
	msgs := ReadAll(agent)
	if from < 0 {
		from = 0
	}
	if to >= len(msgs) {
		to = len(msgs) - 1
	}
	if from > to || len(msgs) == 0 {
		return nil
	}
	return msgs[from : to+1]
}

func tokenize(q string) []string {
	var out []string
	for _, f := range strings.Fields(strings.ToLower(q)) {
		f = strings.Trim(f, "\"'`.,;:!?()[]{}")
		if len(f) >= 2 {
			out = append(out, f)
		}
	}
	return out
}
