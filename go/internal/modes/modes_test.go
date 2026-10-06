package modes

// modes 单测: UseProcs=false 走进程内路径, httptest 假端点喂 canned 回复。

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/xjcdw0777/agent-harness/internal/config"
	"github.com/xjcdw0777/agent-harness/internal/core"
	"github.com/xjcdw0777/agent-harness/internal/tools"
)

type mockSrv struct {
	*httptest.Server
	calls  *int32
	planed *int32 // task 计划请求计数
}

func newMock(t *testing.T) *mockSrv {
	var calls, planed int32
	s := &mockSrv{calls: &calls, planed: &planed}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		var body struct {
			Messages []struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"messages"`
			Tools []map[string]any `json:"tools"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		last := ""
		for _, m := range body.Messages {
			if m.Role == "user" {
				last = m.Content
			}
		}
		content := "canned-" + last[:min(20, len(last))]
		var tcs []map[string]any
		switch {
		case strings.Contains(last, "只输出 JSON"):
			atomic.AddInt32(&planed, 1)
			content = `[{"id":"t1","owner":"a1","title":"写个文件","depends_on":[]},
			             {"id":"t2","owner":"a2","title":"读出来","depends_on":["t1"]}]`
		case strings.Contains(last, "USETOOL") && body.Messages[len(body.Messages)-1].Role == "user":
			tcs = []map[string]any{{
				"id": "tc1", "type": "function",
				"function": map[string]any{"name": "list_dir",
					"arguments": `{"path":"."}`}}}
			content = ""
		}
		msg := map[string]any{"role": "assistant", "content": content}
		if tcs != nil {
			msg["tool_calls"] = tcs
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{"message": msg}},
			"usage":   map[string]any{"prompt_tokens": 10, "completion_tokens": 5}})
	}))
	t.Cleanup(s.Close)
	return s
}

func setupEnv(t *testing.T, srv *mockSrv) string {
	root := t.TempDir()
	config.Root = root
	config.NoTools = false
	config.EpOverride = ""
	UseProcs = false
	off := false
	config.G = &config.Global{
		TimeoutS: 30, TurnTimeoutS: 60, MaxToolRounds: 6,
		ToolOutputMax: 12000, Stream: &off,
		ToolRoots: []string{root}, WriteRoots: []string{root},
		RoleMap: map[string]string{"developer": "system"},
	}
	config.Endpoints = map[string]config.Endpoint{
		"mock": {Base: srv.URL + "/v1", Key: "k"},
	}
	mk := func(name, model string, toolz []string) *config.Agent {
		return &config.Agent{Name: name, Model: model, Endpoint: "mock",
			Persona: "persona-" + name, Tools: toolz}
	}
	config.Agents = map[string]*config.Agent{
		"a1":           mk("a1", "m1", []string{"list_dir"}),
		"a2":           mk("a2", "m2", nil),
		"orchestrator": mk("orchestrator", "mo", nil),
	}
	tools.Ex = tools.NewExecutor()
	t.Cleanup(core.CloseAllRuns)
	return root
}

func TestSpeakInproc(t *testing.T) {
	srv := newMock(t)
	setupEnv(t, srv)
	run := core.NewRun("ask", "hi")
	out := speakInproc(run, "a1", "hello world", "", "")
	if !strings.Contains(out, "canned-hello world") {
		t.Fatalf("out=%q", out)
	}
	if atomic.LoadInt32(srv.calls) != 1 {
		t.Fatalf("calls=%d", *srv.calls)
	}
	if st := run.SM.Snap()["a1"].State; st != "done" {
		t.Fatalf("state=%s", st)
	}
}

func TestSpeakInprocToolLoop(t *testing.T) {
	srv := newMock(t)
	setupEnv(t, srv)
	run := core.NewRun("ask", "tool")
	out := speakInproc(run, "a1", "USETOOL now", "", "")
	// 轮1: tool_calls(list_dir) → 轮2: canned
	if atomic.LoadInt32(srv.calls) != 2 {
		t.Fatalf("calls=%d 应=2(工具轮+收尾轮)", *srv.calls)
	}
	if !strings.Contains(out, "canned") {
		t.Fatalf("out=%q", out)
	}
}

func TestBrainstormFlow(t *testing.T) {
	srv := newMock(t)
	setupEnv(t, srv)
	Brainstorm("评审架构", 2, []string{"a1", "a2"})
	// 2轮×2agent + 综合 = 5 次调用
	if n := atomic.LoadInt32(srv.calls); n != 5 {
		t.Fatalf("calls=%d 应=5", n)
	}
}

func TestCouncilFlow(t *testing.T) {
	srv := newMock(t)
	setupEnv(t, srv)
	Council("X 还是 Y?", []string{"a1", "a2"})
	// 2 独立作答 + 2 互评 + 1 裁决 = 5
	if n := atomic.LoadInt32(srv.calls); n != 5 {
		t.Fatalf("calls=%d 应=5", n)
	}
}

func TestTaskPlanDispatch(t *testing.T) {
	srv := newMock(t)
	setupEnv(t, srv)
	Task("做个小工具", []string{"a1", "a2"})
	if atomic.LoadInt32(srv.planed) != 1 {
		t.Fatal("plan 请求未发")
	}
	// plan(1) + t1(a1) + t2(a2) = 3, 无 critic
	if n := atomic.LoadInt32(srv.calls); n != 3 {
		t.Fatalf("calls=%d 应=3", n)
	}
}

func TestTurnOfUnknownAgent(t *testing.T) {
	srv := newMock(t)
	setupEnv(t, srv)
	run := core.NewRun("ask", "x")
	if out := turnOf(run, "ghost", "hi", "", ""); !strings.Contains(out, "unknown") {
		t.Fatalf("out=%q", out)
	}
}
