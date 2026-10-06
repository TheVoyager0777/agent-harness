package ctx

// 消息视图与 token 估算(与 ctxsvc 同规则)。

import (
	"strings"

	"github.com/xjcdw0777/agent-harness/internal/config"
)

type msgView struct {
	role       string
	content    string
	toolCalls  string
	toolCallID string
	name       string
}

func viewOf(m config.Message) msgView {
	tc := ""
	for _, c := range m.ToolCalls {
		tc += c.Function.Name + c.Function.Arguments
	}
	return msgView{role: m.Role, content: m.Content,
		toolCalls: tc, toolCallID: m.ToolCallID, name: m.Name}
}

func estTokens(s, model string) int64 {
	if s == "" {
		return 0
	}
	if strings.HasPrefix(model, "glm-5-2") && strings.HasSuffix(model, "-1m") {
		var cjk, ascii int64
		for _, r := range s {
			if r > 0x2E80 {
				cjk++
			} else {
				ascii++
			}
		}
		return cjk*3 + ascii*3/10 + 1
	}
	return int64(len(s))*10/35 + 1
}

const perMsgOverhead = 24

func estMsgTokens(m msgView, model string) int64 {
	return estTokens(m.content, model) +
		estTokens(m.toolCalls, model) + perMsgOverhead
}
