package model

import (
	"strings"
	"testing"
)

func TestParseSSEContent(t *testing.T) {
	sse := `data: {"choices":[{"delta":{"role":"assistant"}}]}

data: {"choices":[{"delta":{"content":"Hello"}}]}

data: {"choices":[{"delta":{"content":" world"}}]}

data: {"choices":[{"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":2}}

data: [DONE]

`
	msg, usage := ParseSSE(strings.NewReader(sse), nil)
	if msg.Content != "Hello world" {
		t.Fatalf("content=%q", msg.Content)
	}
	if usage["prompt_tokens"].(float64) != 10 {
		t.Fatalf("usage=%v", usage)
	}
}

func TestParseSSEToolCalls(t *testing.T) {
	sse := `data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"name":"read_file","arguments":""}}]}}]}

data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"path\":"}}]}}]}

data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"x\"}"}}]}}]}

data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}

data: [DONE]
`
	msg, _ := ParseSSE(strings.NewReader(sse), nil)
	if len(msg.ToolCalls) != 1 {
		t.Fatalf("toolcalls=%d", len(msg.ToolCalls))
	}
	tc := msg.ToolCalls[0]
	if tc.ID != "call_1" || tc.Function.Name != "read_file" {
		t.Fatalf("tc=%+v", tc)
	}
	if tc.Function.Arguments != `{"path":"x"}` {
		t.Fatalf("args=%q", tc.Function.Arguments)
	}
}

func TestParseSSEReasoning(t *testing.T) {
	sse := `data: {"choices":[{"delta":{"reasoning_content":"思考中"}}]}

data: {"choices":[{"delta":{"content":"答"}}]}

data: [DONE]
`
	var kinds []string
	msg, _ := ParseSSE(strings.NewReader(sse),
		func(text, kind string) { kinds = append(kinds, kind+":"+text) })
	if len(kinds) != 2 || kinds[0] != "reasoning:思考中" || kinds[1] != "content:答" {
		t.Fatalf("kinds=%v", kinds)
	}
	if msg.Content != "答" {
		t.Fatalf("content=%q", msg.Content)
	}
}

func TestHttpErrRetryable(t *testing.T) {
	// 429/5xx 可重试, 4xx 不重试 — 若 endpoint.go 里有该判定就测它
	if !(HTTPErr{code: 429}).retryable() {
		t.Error("429 应重试")
	}
	if !(HTTPErr{code: 502}).retryable() {
		t.Error("502 应重试")
	}
	if (HTTPErr{code: 400}).retryable() {
		t.Error("400 不应重试")
	}
	if (HTTPErr{code: 401}).retryable() {
		t.Error("401 不应重试")
	}
}
