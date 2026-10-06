package ctx

// 跨 run 会话复用:
//   - 压缩器每次产出摘要后落盘 archive/<agent>.session.json (seed+pins)
//   - 新 run spawn 时 LoadSession 恢复: 摘要 + pins + 档案近期尾巴
//   - 由 context.resume 或 --resume 按需启用; 关闭则各 run 全新开始

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"

	"github.com/xjcdw0777/agent-harness/internal/config"
)

type sessionState struct {
	Seed string   `json:"seed"`
	Pins []string `json:"pins"`
}

func sessionFile(agent string) string {
	safe := strings.Map(func(r rune) rune {
		if r == '/' || r == 92 || r == ':' {
			return '_'
		}
		return r
	}, agent)
	return filepath.Join(dir(), safe+".session.json")
}

func (c *Compactor) saveState() {
	c.mu.Lock()
	defer c.mu.Unlock()
	st := sessionState{Seed: c.seed, Pins: c.pins}
	if st.Seed == "" && len(st.Pins) == 0 {
		return
	}
	b, _ := json.Marshal(st)
	os.WriteFile(sessionFile(c.Agent.Name), b, 0o644)
}

// LoadSession: 组装新 run 的初始 history — 摘要+pins+档案尾巴。
// tailEst 为回放尾巴的 token 上限。
func LoadSession(agent string, tailEst int64) []config.Message {
	var out []config.Message
	var st sessionState
	if b, err := os.ReadFile(sessionFile(agent)); err == nil {
		json.Unmarshal(b, &st)
	}
	if st.Seed != "" {
		out = append(out, config.Message{Role: "user",
			Content: "[跨会话恢复的早前工作摘要]\n" + st.Seed})
	}
	for _, p := range st.Pins {
		out = append(out, config.Message{Role: "developer", Content: p})
	}
	// 档案尾巴: 从后往前取, 估长封顶
	msgs := ReadAll(agent)
	if tailEst <= 0 {
		tailEst = 8000
	}
	var tail []archMsg
	var est int64
	for i := len(msgs) - 1; i >= 0; i-- {
		e := estMsgTokens(viewOf(config.Message{Role: msgs[i].Role,
			Content: msgs[i].Content}), "")
		if est+e > tailEst {
			break
		}
		est += e
		tail = append([]archMsg{msgs[i]}, tail...)
	}
	if len(tail) > 0 {
		out = append(out, config.Message{Role: "developer",
			Content: "[以下为该 agent 上个会话的近期档案回放]"})
		for _, m := range tail {
			out = append(out, config.Message{Role: m.Role, Content: m.Content})
		}
	}
	if len(out) > 0 {
		out = append([]config.Message{{Role: "developer",
			Content: "[会话复用已启用] 以下是该 agent 的历史上下文(摘要+档案回放)," +
				"可直接续接,也可用 ctx_search 检索更早的档案原文。"}}, out...)
	}
	return out
}
