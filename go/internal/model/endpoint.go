package model

// 端点调用: SSE 流式 + X-Session-Id + 强制 IPv4(TUN fake-ip6 兼容) + 重试 +
// developer role 降级 + usage 记账。

import (
	"github.com/xjcdw0777/agent-harness/internal/config"
	"github.com/xjcdw0777/agent-harness/internal/core"
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"time"
)

var Sessions = map[string]string{}

func sessionFile() string { return config.Root + "/.sessions.json" }

func saveSessions() {
	if config.G.Sessions == "persist" {
		b, _ := json.MarshalIndent(Sessions, "", " ")
		os.WriteFile(sessionFile(), b, 0o644)
	}
}

func loadSessions() {
	if config.G.Sessions == "persist" {
		config.ReadJSON(sessionFile(), &Sessions)
	}
}

func sessionFor(agent string) string {
	if config.G.Sessions == "none" {
		return ""
	}
	if Sessions[agent] == "" {
		Sessions[agent] = "h-" + core.RandHex(8)
		saveSessions()
	}
	return Sessions[agent]
}

// 强制 IPv4 的 transport(TUN fake-ip6 兼容)
var httpClient = &http.Client{
	Transport: &http.Transport{
		// 不走系统代理: 内部网关直连
		Proxy: nil,
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			return (&net.Dialer{Timeout: 30 * time.Second}).DialContext(ctx, "tcp4", addr)
		},
		MaxIdleConns: 32,
	},
}

func Call(a *config.Agent, agentName string, msgs []config.Message, tools []map[string]any,
	onDelta func(text, kind string), run *core.Run) (config.Message, map[string]any, error) {

	epName := a.Endpoint
	if run != nil && run.Ep != "" {
		epName = run.Ep
	}
	if config.EpOverride != "" {
		epName = config.EpOverride
	}
	ep, ok := config.Endpoints[epName]
	if !ok {
		return config.Message{}, nil, fmt.Errorf("unknown endpoint %q", epName)
	}
	sess := sessionFor(agentName)

	body := map[string]any{
		"model": a.Model, "messages": msgs,
		"temperature": a.Temp, "stream": config.G.StreamEnabled(),
	}
	if config.G.StreamEnabled() {
		body["stream_options"] = map[string]any{"include_usage": true}
	}
	if len(tools) > 0 {
		body["tools"] = tools
	}

	do := func(ms []config.Message, stream bool) (config.Message, http.Header, map[string]any, error) {
		b := map[string]any{}
		for k, v := range body {
			b[k] = v
		}
		b["messages"] = ms
		b["stream"] = stream
		if stream {
			b["stream_options"] = map[string]any{"include_usage": true}
		}
		raw, _ := json.Marshal(b)
		var lastErr error
		for attempt := 0; attempt < 3; attempt++ {
			req, _ := http.NewRequest("POST",
				strings.TrimRight(ep.Base, "/")+"/chat/completions",
				bytes.NewReader(raw))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Authorization", "Bearer "+ep.Key)
			req.Header.Set("Accept", "text/event-stream")
			if sess != "" {
				req.Header.Set("X-Session-Id", sess)
			}
			resp, err := httpClient.Do(req)
			if err != nil {
				lastErr = err
				time.Sleep(time.Duration(2*(attempt+1)) * time.Second)
				continue
			}
			if resp.StatusCode != 200 {
				eb, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
				resp.Body.Close()
				he := HTTPErr{code: resp.StatusCode, body: string(eb)}
				if he.retryable() && attempt < 2 {
					lastErr = he
					time.Sleep(time.Duration(2*(attempt+1)) * time.Second)
					continue
				}
				return config.Message{}, nil, nil, he
			}
			if stream && strings.Contains(resp.Header.Get("Content-Type"), "event-stream") {
				msg, usage := ParseSSE(resp.Body, onDelta)
				resp.Body.Close()
				return msg, resp.Header, usage, nil
			}
			var d struct {
				Choices []struct {
					Message config.Message `json:"message"`
				} `json:"choices"`
				Usage map[string]any `json:"usage"`
			}
			dec := json.NewDecoder(resp.Body)
			err = dec.Decode(&d)
			resp.Body.Close()
			if err != nil {
				return config.Message{}, nil, nil, err
			}
			if len(d.Choices) == 0 {
				return config.Message{}, nil, nil, fmt.Errorf("empty choices")
			}
			return d.Choices[0].Message, resp.Header, d.Usage, nil
		}
		return config.Message{}, nil, nil, lastErr
	}

	msg, hdrs, usage, err := do(msgs, config.G.StreamEnabled())
	if err != nil {
		if he, ok := err.(HTTPErr); ok && he.code == 400 {
			// developer role 降级
			hasDev := false
			for _, m := range msgs {
				if m.Role == "developer" {
					hasDev = true
				}
			}
			if hasDev {
				rm := config.G.RoleMap["developer"]
				if rm == "" {
					rm = "system"
				}
				var mapped []config.Message
				for _, m := range msgs {
					if m.Role == "developer" {
						m.Role = rm
					}
					mapped = append(mapped, m)
				}
				msg, hdrs, usage, err = do(mapped, config.G.StreamEnabled())
			}
			if err != nil {
				if he2, ok := err.(HTTPErr); ok && he2.code == 400 && config.G.StreamEnabled() {
					msg, hdrs, usage, err = do(msgs, false) // 流式降级
				}
			}
		}
		if err != nil {
			return config.Message{}, nil, err
		}
	}
	if ns := hdrs.Get("X-Session-Id"); ns != "" && ns != sess {
		Sessions[agentName] = ns
		saveSessions()
	}
	return msg, usage, nil
}

type HTTPErr struct {
	code int
	body string
}

func (e HTTPErr) Error() string { return fmt.Sprintf("HTTP %d: %s", e.code, e.body) }

func (e HTTPErr) retryable() bool {
	return e.code == 429 || e.code == 408 || e.code >= 500
}

// SSE 解析: content / reasoning_content / tool_calls delta 累积; usage 尾部块
func ParseSSE(r io.Reader, onDelta func(text, kind string)) (config.Message, map[string]any) {
	var parts strings.Builder
	tbuf := map[int]*config.ToolCall{}
	var usage map[string]any
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 1<<20), 4<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(line[5:])
		if data == "[DONE]" {
			break
		}
		var d struct {
			Choices []struct {
				Delta struct {
					Content          string `json:"content"`
					ReasoningContent string `json:"reasoning_content"`
					ToolCalls        []struct {
						Index    int    `json:"index"`
						ID       string `json:"id"`
						Function struct {
							Name      string `json:"name"`
							Arguments string `json:"arguments"`
						} `json:"function"`
					} `json:"tool_calls"`
				} `json:"delta"`
			} `json:"choices"`
			Usage map[string]any `json:"usage"`
		}
		if json.Unmarshal([]byte(data), &d) != nil {
			continue
		}
		if d.Usage != nil {
			usage = d.Usage
		}
		if len(d.Choices) == 0 {
			continue
		}
		dl := d.Choices[0].Delta
		if dl.Content != "" {
			parts.WriteString(dl.Content)
			if onDelta != nil {
				onDelta(dl.Content, "content")
			}
		}
		if dl.ReasoningContent != "" && onDelta != nil {
			onDelta(dl.ReasoningContent, "reasoning")
		}
		for _, tc := range dl.ToolCalls {
			acc := tbuf[tc.Index]
			if acc == nil {
				acc = &config.ToolCall{Type: "function"}
				tbuf[tc.Index] = acc
			}
			if tc.ID != "" {
				acc.ID = tc.ID
			}
			acc.Function.Name += tc.Function.Name
			acc.Function.Arguments += tc.Function.Arguments
		}
	}
	msg := config.Message{Role: "assistant", Content: parts.String()}
	if len(tbuf) > 0 {
		idxs := make([]int, 0, len(tbuf))
		for i := range tbuf {
			idxs = append(idxs, i)
		}
		sortInts(idxs)
		for _, i := range idxs {
			msg.ToolCalls = append(msg.ToolCalls, *tbuf[i])
		}
	}
	return msg, usage
}

func sortInts(s []int) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}
