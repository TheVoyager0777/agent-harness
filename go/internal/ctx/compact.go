package ctx

// 零阻塞上下文压缩器(借鉴 ctxsvc: 分块滚动摘要+指纹缓存+pins 保全)。
//
// 与 ctxsvc 的差异: 摘要不在请求路径上——超阈值时后台 goroutine 做压缩,
// 完成的摘要存 pending, 下一个 turn 边界才被换入。当前 turn 永远用原文,
// 等待成本为零(0阻塞)。
//
// 摘要缓存按输入指纹复用: 滚动续压(旧摘要 seed 入首块)天然增量。

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"github.com/xjcdw0777/agent-harness/internal/config"
)

// SummFn: 摘要回调 — 注入 model 调用(proc 子进程走 IPC, inproc 走 model.Call)。
type SummFn func(msgs []config.Message) (string, error)

type CompactConf struct {
	MaxEst      int64 `json:"max_est"`       // 触发阈值, 0=不压缩
	KeepRecent  int   `json:"keep_recent"`   // 尾部保留原文消息数
	ChunkEst    int64 `json:"chunk_est"`     // 单块输入上限
	SummarySeed bool  `json:"summary_seed"`  // 旧摘要续压
}

var Conf = CompactConf{MaxEst: 60000, KeepRecent: 10, ChunkEst: 24000,
	SummarySeed: true}

// SyncConf: 从 harness.json context 节同步配置(在 config 加载后调用)。
func SyncConf() {
	if config.G == nil {
		return
	}
	cc := config.G.Context
	if cc.MaxEst > 0 {
		Conf.MaxEst = cc.MaxEst
	}
	if cc.KeepRecent > 0 {
		Conf.KeepRecent = cc.KeepRecent
	}
	if cc.ChunkEst > 0 {
		Conf.ChunkEst = cc.ChunkEst
	}
}

const summarizerPrompt = `You are a Summarizer that summarizes conversation history.
You will be shown a conversation between the user and the assistant, a coding
agent. Summarize the conversation + work done for future work to be continued.
Structure as:
<summary>
## Overview
high-level summary (1-2 sentences)
## Key Details & Breadcrumbs
key findings/decisions/constraints/file paths/errors — each with a search
term to recall the original via ctx_search.
## Current State
the immediate task in progress + pending next steps + blockers.
</summary>
Do NOT reproduce verbatim rules/instructions. Be concise; someone should
resume work using only your summary plus the archived history.
Now summarize the conversation above. Provide only the <summary> tags output.`

type Compactor struct {
	Agent   *config.Agent
	Call    SummFn
	mu      sync.Mutex
	running bool
	pending []config.Message // 完成待换入: [summaryMsg]+pins
	seed    string           // 旧摘要(增量续压)
	sumCache map[string]string
}

func NewCompactor(a *config.Agent, fn SummFn) *Compactor {
	return &Compactor{Agent: a, Call: fn, sumCache: map[string]string{}}
}

// Apply: turn 边界调用 — 有完成摘要则换入, 超阈值则后台起压缩。永不阻塞。
// 返回(可能换入后的) history。
func (c *Compactor) Apply(history []config.Message) []config.Message {
	c.mu.Lock()
	if c.pending != nil {
		keep := Conf.KeepRecent
		if keep > len(history) {
			keep = len(history)
		}
		history = append(append([]config.Message{}, c.pending...),
			history[len(history)-keep:]...)
		c.pending = nil
	}
	running := c.running
	c.mu.Unlock()

	if running || Conf.MaxEst <= 0 || len(history) <= Conf.KeepRecent+2 {
		return history
	}
	var est int64
	for _, m := range history[:len(history)-Conf.KeepRecent] {
		est += estMsgTokens(viewOf(m), c.Agent.Model)
	}
	if est < Conf.MaxEst {
		return history
	}
	// 后台压缩快照段(不动当前 history)
	seg := append([]config.Message{}, history[:len(history)-Conf.KeepRecent]...)
	c.mu.Lock()
	c.running = true
	c.mu.Unlock()
	go func() {
		out, pins := c.compress(seg)
		if len(out) > 0 {
			c.mu.Lock()
			c.pending = append(out, pins...)
			c.mu.Unlock()
		}
		c.mu.Lock()
		c.running = false
		c.mu.Unlock()
	}()
	return history
}

// compress: 分块滚动摘要(同步执行, 但在后台 goroutine 中跑)。
func (c *Compactor) compress(seg []config.Message) ([]config.Message, []config.Message) {
	views := make([]msgView, len(seg))
	for i, m := range seg {
		views[i] = viewOf(m)
	}
	rest, ps := extractPins(views)
	pins := pinMsgs(ps)

	// user 边界切块
	var chunks [][]msgView
	var cur []msgView
	var curEst int64
	for _, v := range rest {
		e := estMsgTokens(v, c.Agent.Model)
		if v.role == "user" && len(cur) > 0 && curEst+e > Conf.ChunkEst {
			chunks = append(chunks, cur)
			cur, curEst = nil, 0
		}
		cur = append(cur, v)
		curEst += e
	}
	if len(cur) > 0 {
		chunks = append(chunks, cur)
	}
	if len(chunks) == 0 {
		return nil, pins
	}
	rolling := c.seed
	for _, ch := range chunks {
		msgs := viewsToMsgs(ch)
		if rolling != "" && Conf.SummarySeed {
			msgs = append([]config.Message{{Role: "user",
				Content: "[以下为更早对话历史的摘要,请结合其继续摘要本段]\n" + rolling}},
				msgs...)
		}
		fp := fpOf(c.Agent.Model, msgs)
		if s, ok := c.sumCache[fp]; ok {
			rolling = s
			continue
		}
		callMsgs := append([]config.Message{
			{Role: "system", Content: summarizerPrompt}}, msgs...)
		sum, err := c.Call(callMsgs)
		if err != nil {
			return nil, pins // 失败不阻塞, 下轮重试
		}
		sum = extractSummary(sum)
		if sum == "" {
			return nil, pins
		}
		c.sumCache[fp] = sum
		rolling = sum
	}
	c.seed = rolling // 下一轮续压
	sumMsg := config.Message{Role: "user",
		Content: "[以下为更早对话历史的摘要(由上下文压缩生成,原文在档案库,可用 ctx_search 召回)]\n" + rolling}
	return []config.Message{sumMsg}, pins
}

func pinMsgs(ps *pinSet) []config.Message {
	var out []config.Message
	for _, p := range ps.list() {
		out = append(out, config.Message{Role: "developer", Content: p})
	}
	return out
}

func viewsToMsgs(vs []msgView) []config.Message {
	out := make([]config.Message, len(vs))
	for i, v := range vs {
		out[i] = config.Message{Role: v.role, Content: v.content,
			ToolCallID: v.toolCallID, Name: v.name}
	}
	return out
}

func extractSummary(text string) string {
	i := strings.Index(text, "<summary>")
	j := strings.LastIndex(text, "</summary>")
	if i >= 0 && j > i {
		return strings.TrimSpace(text[i+len("<summary>") : j])
	}
	return strings.TrimSpace(text)
}

func fpOf(model string, msgs []config.Message) string {
	h := sha256.New()
	h.Write([]byte(model))
	b, _ := json.Marshal(msgs)
	h.Write(b)
	return hex.EncodeToString(h.Sum(nil))[:32]
}

// EstHistory: 外部估长(status/调试)。
func EstHistory(model string, msgs []config.Message) int64 {
	var n int64
	for _, m := range msgs {
		n += estMsgTokens(viewOf(m), model)
	}
	return n
}

var _ = fmt.Sprint
