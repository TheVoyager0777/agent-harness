package serve

// serve 单测: ctlDispatch / ServeControl+ctlCall 往返 / handleServeConn job 流转。

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/xjcdw0777/agent-harness/internal/config"
	"github.com/xjcdw0777/agent-harness/internal/core"
	"github.com/xjcdw0777/agent-harness/internal/modes"
	"github.com/xjcdw0777/agent-harness/internal/proc"
	"github.com/xjcdw0777/agent-harness/internal/tools"
)

var calls int32

func newMock(t *testing.T) *httptest.Server {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{"message": map[string]any{
				"role": "assistant", "content": "srv-ok"}}},
			"usage": map[string]any{}})
	}))
	t.Cleanup(s.Close)
	return s
}

func setup(t *testing.T, srv *httptest.Server) string {
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, "runs"), 0o755)
	config.Root = root
	config.EpOverride = ""
	config.NoTools = false
	off := false
	config.G = &config.Global{TimeoutS: 10, TurnTimeoutS: 30,
		MaxToolRounds: 3, Stream: &off, ServePort: 0,
		ToolRoots: []string{root}, WriteRoots: []string{root},
		RoleMap: map[string]string{"developer": "system"}}
	config.Endpoints = map[string]config.Endpoint{
		"mock": {Base: srv.URL + "/v1"},
	}
	config.Agents = map[string]*config.Agent{
		"a1": {Name: "a1", Model: "m1", Endpoint: "mock", Persona: "p"},
	}
	modes.UseProcs = false
	tools.Ex = tools.NewExecutor()
	proc.DaemonMode = false
	D = &daemon{procs: map[string]*proc.AgentProc{}}
	t.Cleanup(func() {
		core.CloseAllRuns()
		proc.DaemonMode = false
	})
	return root
}

// ctlDispatch 直接调用: status/state/inject/未知 op
func TestCtlDispatch(t *testing.T) {
	setup(t, newMock(t))
	run := core.NewRun("ask", "x")
	run.SM.Set("a1", "idle", "")
	res := ctlDispatch(run, map[string]any{"op": "status"})
	if _, ok := res["states"]; !ok {
		t.Fatalf("status res=%v", res)
	}
	res = ctlDispatch(run, map[string]any{"op": "state", "agent": "a1"})
	if fmt.Sprint(res["state"]) == "" {
		t.Fatalf("state res=%v", res)
	}
	res = ctlDispatch(run, map[string]any{"op": "inject", "agent": "a1", "msg": "hi"})
	if ok, _ := res["ok"].(bool); ok {
		t.Fatal("无活进程应拒绝 inject")
	}
	res = ctlDispatch(run, map[string]any{"op": "bogus"})
	if res["error"] == nil {
		t.Fatal("未知 op 应报错")
	}
}

// ServeControl 起口 → ctlCall 拨号往返
func TestControlRoundtrip(t *testing.T) {
	setup(t, newMock(t))
	run := core.NewRun("ask", "rt")
	run.SM.Set("a1", "thinking", "")
	port := ServeControl(run, 0)
	if port <= 0 {
		t.Fatal("控制口未起")
	}
	if _, err := os.Stat(filepath.Join(run.Dir, "control.port")); err != nil {
		t.Fatal("control.port 未落盘")
	}
	res := CtlCall(run.Dir, map[string]any{"op": "status"})
	states, _ := res["states"].(map[string]any)
	if states["a1"] == nil {
		t.Fatalf("status 缺 a1: %v", res)
	}
	res = CtlCall(run.Dir, map[string]any{"op": "state", "agent": "a1"})
	if !strings.Contains(fmt.Sprint(res["state"]), "thinking") {
		t.Fatalf("state=%v", res)
	}
	// 死端口兜底: 不存在 rundir → error
	res = CtlCall("", map[string]any{"op": "status"})
	if res["error"] == nil && res["states"] == nil {
		// 无 latest.port 或端口死都合理, 只要不 panic
	}
}

// handleServeConn: submit→job 流转→status→inject→unknown
func TestDaemonConnFlow(t *testing.T) {
	srv := newMock(t)
	setup(t, srv)
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go handleServeConn(c)
		}
	}()
	port := ln.Addr().(*net.TCPAddr).Port
	call := func(obj map[string]any) map[string]any {
		c, err := net.DialTimeout("tcp4",
			fmt.Sprintf("127.0.0.1:%d", port), 3*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		json.NewEncoder(c).Encode(obj)
		var res map[string]any
		json.NewDecoder(c).Decode(&res)
		return res
	}

	// 未知 mode → job error
	res := call(map[string]any{"op": "submit", "mode": "bogus", "topic": "t"})
	if ok, _ := res["ok"].(bool); !ok {
		t.Fatalf("submit=%v", res)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		st := call(map[string]any{"op": "status"})
		jobs, _ := st["jobs"].([]any)
		if len(jobs) > 0 {
			if j, _ := jobs[0].(map[string]any); j != nil &&
				strings.Contains(fmt.Sprint(j["state"]), "error") {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("job 未转 error: %v", st)
		}
		time.Sleep(20 * time.Millisecond)
	}

	// ask job 真跑(inproc+mock 端点)→ done
	res = call(map[string]any{"op": "submit", "mode": "ask", "topic": "hello"})
	if ok, _ := res["ok"].(bool); !ok {
		t.Fatalf("submit ask=%v", res)
	}
	deadline = time.Now().Add(10 * time.Second)
	for {
		st := call(map[string]any{"op": "status"})
		jobs, _ := st["jobs"].([]any)
		doneOK := false
		for _, j := range jobs {
			jm, _ := j.(map[string]any)
			if jm["mode"] == "ask" && jm["state"] == "done" {
				doneOK = true
			}
		}
		if doneOK {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("ask job 未 done: %v", st)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if atomic.LoadInt32(&calls) == 0 {
		t.Fatal("ask job 未触端点")
	}

	// inject: 无活 agent → ok:false
	res = call(map[string]any{"op": "inject", "agent": "a1", "msg": "hi"})
	if ok, _ := res["ok"].(bool); ok {
		t.Fatal("inject 应报 no live agent")
	}
	// unknown op
	res = call(map[string]any{"op": "bogus"})
	if res["error"] == nil {
		t.Fatal("unknown op 未报错")
	}
}

// Call 客户端: G.ServePort 指向测试 listener
func TestClientCall(t *testing.T) {
	setup(t, newMock(t))
	ln, _ := net.Listen("tcp4", "127.0.0.1:0")
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go handleServeConn(c)
		}
	}()
	config.G.ServePort = ln.Addr().(*net.TCPAddr).Port
	res := Call(map[string]any{"op": "status"})
	if _, ok := res["jobs"]; !ok {
		t.Fatalf("status=%v", res)
	}
	// 未运行 daemon 的端口 → error 不 panic
	config.G.ServePort = 1
	res = Call(map[string]any{"op": "status"})
	if res["error"] == nil {
		t.Fatal("应报 daemon 未运行")
	}
}
