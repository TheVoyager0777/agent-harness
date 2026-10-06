package tools

// 上下文治理 + 代码索引 扩展工具(装配于 init, 依赖 ctx/index 包)。

import (
	"encoding/json"
	"fmt"

	"github.com/xjcdw0777/agent-harness/internal/config"
	"github.com/xjcdw0777/agent-harness/internal/ctx"
	"github.com/xjcdw0777/agent-harness/internal/index"
)

func init() {
	Registry["ctx_search"] = &ToolDef{
		Fn: tCtxSearch, Schema: schema("ctx_search",
			"检索你的对话档案库(已压缩历史的原文)。关键词分词命中,返回片段+索引。",
			map[string]any{"query": map[string]any{"type": "string"},
				"limit": map[string]any{"type": "integer"}},
			[]string{"query"})}
	Registry["ctx_read"] = &ToolDef{
		Fn: tCtxRead, Schema: schema("ctx_read",
			"按索引区间回读档案原文(ctx_search 二级取件)。",
			map[string]any{"from": map[string]any{"type": "integer"},
				"to": map[string]any{"type": "integer"}},
			[]string{"from"})}
	Registry["code_search"] = &ToolDef{
		Fn: tCodeSearch, Schema: schema("code_search",
			"在代码索引中检索符号/代码片段。返回 path:line + 命中块。",
			map[string]any{"query": map[string]any{"type": "string"},
				"limit": map[string]any{"type": "integer"}},
			[]string{"query"})}
	Registry["code_index"] = &ToolDef{
		Fn: tCodeIndex, Schema: schema("code_index",
			"代码索引状态/重建。action: status|refresh",
			map[string]any{"action": map[string]any{"type": "string"}},
			nil)}
	toolClass["ctx_search"] = "read"
	toolClass["ctx_read"] = "read"
	toolClass["code_search"] = "read"
	toolClass["code_index"] = "read"
}

func tCtxSearch(a *config.Agent, args map[string]any) (string, error) {
	hits := ctx.Search(a.Name, argS(args, "query"), argI(args, "limit", 8))
	if len(hits) == 0 {
		return "(无命中)", nil
	}
	b, _ := json.MarshalIndent(hits, "", " ")
	return string(b), nil
}

func tCtxRead(a *config.Agent, args map[string]any) (string, error) {
	msgs := ctx.Read(a.Name, argI(args, "from", 0), argI(args, "to", 50))
	if len(msgs) == 0 {
		return "(空)", nil
	}
	out := ""
	for _, m := range msgs {
		out += fmt.Sprintf("[%s] %s\n---\n", m.Role, m.Content)
	}
	return out, nil
}

func tCodeSearch(a *config.Agent, args map[string]any) (string, error) {
	chunks := index.Search(argS(args, "query"), argI(args, "limit", 8))
	if len(chunks) == 0 {
		return "(无命中)", nil
	}
	out := ""
	for _, c := range chunks {
		out += fmt.Sprintf("### %s:%d %s\n```%s\n%s\n```\n",
			c.Path, c.Line, c.Name, c.Lang, c.Text)
	}
	return out, nil
}

func tCodeIndex(a *config.Agent, args map[string]any) (string, error) {
	if argS(args, "action") == "refresh" {
		return index.Refresh(), nil
	}
	return index.Summary(), nil
}
