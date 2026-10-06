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
	Registry["kb_write"] = &ToolDef{
		Fn: tKBWrite, Schema: schema("kb_write",
			"沉淀一条可复用知识到共享知识库(跨会话保留)。kind: fact|decision|finding;tags 可选。重要结论/判别结果/踩坑都应写入。",
			map[string]any{"kind": map[string]any{"type": "string"},
				"text": map[string]any{"type": "string"},
				"tags": map[string]any{"type": "array",
					"items": map[string]any{"type": "string"}}},
			[]string{"text"})}
	Registry["kb_search"] = &ToolDef{
		Fn: tKBSearch, Schema: schema("kb_search",
			"检索共享知识库(全体 agent 沉淀的跨会话知识)。",
			map[string]any{"query": map[string]any{"type": "string"},
				"limit": map[string]any{"type": "integer"}},
			[]string{"query"})}
	toolClass["ctx_search"] = "read"
	toolClass["ctx_read"] = "read"
	toolClass["code_search"] = "read"
	toolClass["code_index"] = "read"
	toolClass["kb_search"] = "read"
	toolClass["kb_write"] = "collab"
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

func tKBWrite(a *config.Agent, args map[string]any) (string, error) {
	var tags []string
	if ts, ok := args["tags"].([]any); ok {
		for _, t := range ts {
			tags = append(tags, fmt.Sprint(t))
		}
	}
	return ctx.KBWrite(a.Name, argS(args, "kind"), argS(args, "text"), tags), nil
}

func tKBSearch(a *config.Agent, args map[string]any) (string, error) {
	hits := ctx.KBSearch(argS(args, "query"), argI(args, "limit", 8))
	if len(hits) == 0 {
		return "(无命中)", nil
	}
	out := ""
	for _, e := range hits {
		out += fmt.Sprintf("[%s][%s] %s (%s)\n---\n", e.Kind, e.TS, e.Text, e.Agent)
	}
	return out, nil
}

func tCodeIndex(a *config.Agent, args map[string]any) (string, error) {
	if argS(args, "action") == "refresh" {
		return index.Refresh(), nil
	}
	return index.Summary(), nil
}
