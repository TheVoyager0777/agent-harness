package proc

// AgentProc: 主进程侧的 agent 子进程句柄。
// 子进程 = 本二进制 `_agent NAME` 入口,stdin/stdout JSONL。
// 子进程持有私有 history/inbox/工具循环;模型/工具/总线全部 IPC 回主进程。

import (
	"github.com/xjcdw0777/agent-harness/internal/config"
	"github.com/xjcdw0777/agent-harness/internal/core"
	"github.com/xjcdw0777/agent-harness/internal/model"
	"github.com/xjcdw0777/agent-harness/internal/index"
	"github.com/xjcdw0777/agent-harness/internal/tools"
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"
)

var Procs = map[string]*AgentProc{}
var procsMu sync.Mutex
var DaemonMode bool

// OnSpawned: 新 agent 进程就绪回调(serve daemon 池装配用)。
var OnSpawned func(name string, p *AgentProc)

func init() {
	// 装配跨包钩子(解耦 core/tools → proc)
	core.BusDeliver = func(who string, d map[string]any) {
		if p := Procs[who]; p != nil {
			p.Push(map[string]any{"type": "push", "event": "bus", "data": d})
		}
	}
	tools.AgentInject = func(name, text string) error {
		p := Procs[name]
		if p == nil {
			return fmt.Errorf("agent not running: %s", name)
		}
		p.Inject(text)
		return nil
	}
}

// Alive: 子进程仍在运行。
func (p *AgentProc) Alive() bool { return p.cmd != nil && p.cmd.ProcessState == nil }

// Snapshot: 各 agent 存活状态(serve/status 用)。
func Snapshot() map[string]bool {
	procsMu.Lock()
	defer procsMu.Unlock()
	out := map[string]bool{}
	for n, p := range Procs {
		out[n] = p.Alive()
	}
	return out
}

type AgentProc struct {
	Name    string
	Agent   *config.Agent
	Run     *core.Run
	cmd     *exec.Cmd
	stdin   *json.Encoder
	wmu     sync.Mutex
	pend    map[int]chan map[string]any
	pendMu  sync.Mutex
	seq     int
	turnRes chan map[string]any
	LastStreamed bool
}

func Spawn(name string, run *core.Run) *AgentProc {
	procsMu.Lock()
	defer procsMu.Unlock()
	if p := Procs[name]; p != nil && p.cmd.ProcessState == nil {
		return p
	}
	a := config.Agents[name]
	if a == nil {
		return nil
	}
	errLog, _ := os.Create(filepath.Join(run.Dir, name+".stderr.log"))
	cmd := exec.Command(os.Args[0], "_agent", name, "--home", config.Root)
	cmd.Env = append(os.Environ(), "PYTHONIOENCODING=utf-8")
	stdin, _ := cmd.StdinPipe()
	stdout, _ := cmd.StdoutPipe()
	cmd.Stderr = errLog
	p := &AgentProc{Name: name, Agent: a, Run: run, cmd: cmd,
		stdin: json.NewEncoder(stdin),
		pend:  map[int]chan map[string]any{},
		turnRes: make(chan map[string]any, 1)}
	if err := cmd.Start(); err != nil {
		run.SM.Set(name, "error", "spawn: "+err.Error())
		return nil
	}
	go p.readLoop(stdout)
	Procs[name] = p
	// daemon 保活池注册(serve 经 OnSpawned 装配, 解耦 proc→serve)
	if DaemonMode && OnSpawned != nil {
		OnSpawned(name, p)
	}
	// watch 声明 → 自动订阅
	if a.Permissions.Get("collab", true) {
		for _, w := range a.Watch {
			core.BusSub(name, w.On, w.Note)
		}
	}
	run.SM.Set(name, "idle", "")
	return p
}

func (p *AgentProc) send(obj map[string]any) error {
	p.wmu.Lock()
	defer p.wmu.Unlock()
	return p.stdin.Encode(obj)
}

func (p *AgentProc) Push(obj map[string]any) {
	p.send(obj)
}

func (p *AgentProc) Inject(text string) {
	p.Push(map[string]any{"type": "push", "event": "inject",
		"data": map[string]any{"text": text}})
}

func (p *AgentProc) readLoop(stdout io.Reader) {
	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 1<<20), 8<<20)
	for sc.Scan() {
		var m map[string]any
		if json.Unmarshal(sc.Bytes(), &m) != nil {
			continue
		}
		if p.Run != nil {
			p.Run.Event(map[string]any{"kind": "ipc.rx", "agent": p.Name,
				"type": fmt.Sprint(m["type"]), "method": fmt.Sprint(m["method"])})
		}
		switch m["type"] {
		case "req":
			go p.handleReq(m)
		case "res":
			if m["cmd"] == "turn" {
				p.turnRes <- m
			}
		}
	}
}

func (p *AgentProc) reply(id float64, ok bool, result any, errStr string) {
	m := map[string]any{"type": "res", "id": id}
	if ok {
		m["result"] = result
	} else {
		m["error"] = errStr
	}
	p.send(m)
}

func (p *AgentProc) handleReq(m map[string]any) {
	id, _ := m["id"].(float64)
	method, _ := m["method"].(string)
	params, _ := m["params"].(map[string]any)
	perm := p.Agent.Permissions
	fail := func(e string) { p.reply(id, false, nil, e) }
	switch method {
	case "model.call":
		p.Run.SM.Set(p.Name, "thinking", "")
		var rawMsgs []json.RawMessage
		b, _ := json.Marshal(params["messages"])
		json.Unmarshal(b, &rawMsgs)
		var msgs []config.Message
		for _, rm := range rawMsgs {
			var mm config.Message
			json.Unmarshal(rm, &mm)
			msgs = append(msgs, mm)
		}
		var tools []map[string]any
		if t, ok := params["tools"].([]any); ok {
			for _, x := range t {
				if tm, ok := x.(map[string]any); ok {
					tools = append(tools, tm)
				}
			}
		}
		fmt.Printf("\n[%s] ", p.Name)
		onDelta := func(txt, kind string) {
			if kind == "content" {
				p.LastStreamed = true
				fmt.Print(txt)
			}
		}
		msg, usage, err := model.Call(p.Agent, p.Name, msgs, tools, onDelta, p.Run)
		fmt.Println()
		if err != nil {
			p.Run.SM.Set(p.Name, "error", err.Error()[:min(120, len(err.Error()))])
			fail(err.Error())
			return
		}
		for _, k := range []string{"prompt_tokens", "completion_tokens"} {
			if v, ok := usage[k].(float64); ok {
				p.Run.Usage[k] += int(v)
			}
		}
		p.reply(id, true, map[string]any{"message": msg, "usage": usage}, "")
	case "tool.call":
		tn, _ := params["name"].(string)
		args, _ := params["args"].(string)
		p.Run.SM.Set(p.Name, "tooling", tn)
		fmt.Printf("\n    [%s] tool %s(%s)\n", p.Name, tn, core.Trunc(args, 70))
		res := tools.Run(p.Agent, tn, args)
		p.Run.Event(map[string]any{"kind": "tool", "agent": p.Name,
			"tool": tn, "args": core.Trunc(args, 200)})
		p.reply(id, true, map[string]any{"result": res}, "")
	case "bus.pub":
		if !perm.Get("collab", true) {
			fail("collab denied")
			return
		}
		ev, _ := params["event"].(string)
		data, _ := params["data"].(map[string]any)
		core.BusPub(p.Name, ev, data)
		p.reply(id, true, map[string]any{"ok": true}, "")
	case "bus.sub":
		if !perm.Get("collab", true) {
			fail("collab denied")
			return
		}
		pat, _ := params["pattern"].(string)
		note, _ := params["note"].(string)
		core.BusSub(p.Name, pat, note)
		p.reply(id, true, map[string]any{"ok": true}, "")
	case "bus.wait":
		if !perm.Get("collab", true) {
			fail("collab denied")
			return
		}
		pat, _ := params["pattern"].(string)
		to, _ := params["timeout"].(float64)
		if to <= 0 {
			to = 60
		}
		p.Run.SM.Set(p.Name, "waiting", pat)
		ev := core.BusWait(p.Name, pat, time.Duration(to*float64(time.Second)))
		p.reply(id, true, map[string]any{"event": ev}, "")
	case "context.get":
		cx := core.SharedContext()
		if is := indexSummary(); is != "" {
			cx += "\n\n# 代码索引\n" + is
		}
		p.reply(id, true, map[string]any{"context": cx}, "")
	case "state.set":
		st, _ := params["state"].(string)
		det, _ := params["detail"].(string)
		p.Run.SM.Set(p.Name, st, det)
		p.reply(id, true, map[string]any{"ok": true}, "")
	default:
		fail("unknown method " + method)
	}
}

func (p *AgentProc) Turn(prompt string, extraSys string, prefill string, timeoutS int) string {
	p.LastStreamed = false
	p.send(map[string]any{"type": "cmd", "cmd": "turn", "id": 0,
		"params": map[string]any{"prompt": prompt, "extra_sys": extraSys,
			"prefill": prefill}})
	p.Run.SM.Set(p.Name, "thinking", "")
	if timeoutS <= 0 {
		timeoutS = config.G.TurnTimeoutS
		if timeoutS <= 0 {
			timeoutS = 1800
		}
	}
	var res map[string]any
	select {
	case res = <-p.turnRes:
		if ok, _ := res["ok"].(bool); !ok {
			p.Run.SM.Set(p.Name, "error", fmt.Sprint(res["error"]))
			return fmt.Sprintf("*(agent error: %v)*", res["error"])
		}
	case <-time.After(time.Duration(timeoutS) * time.Second):
		p.Run.SM.Set(p.Name, "error", "turn timeout")
		return "*(turn timeout)*"
	}
	p.Run.SM.Set(p.Name, "done", "")
	text, _ := (res["result"].(map[string]any))["text"].(string)
	if !p.LastStreamed && text != "" {
		fmt.Println(text)
	}
	wrote := p.Run.ExtractArtifacts(text)
	p.Run.Say(p.Name, text, wrote)
	return text
}

func (p *AgentProc) Stop() {
	p.send(map[string]any{"type": "cmd", "cmd": "shutdown", "id": -1})
	done := make(chan struct{})
	go func() { p.cmd.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		p.cmd.Process.Kill()
	}
	p.Run.SM.Set(p.Name, "stopped", "")
	procsMu.Lock()
	delete(Procs, p.Name)
	procsMu.Unlock()
}

func StopAll(run *core.Run) {
	if DaemonMode {
		return // daemon 保活: 任务结束不杀 agent
	}
	procsMu.Lock()
	list := make([]*AgentProc, 0, len(Procs))
	for _, p := range Procs {
		list = append(list, p)
	}
	procsMu.Unlock()
	for _, p := range list {
		p.Stop()
	}
}



func indexSummary() string {
	defer func() { _ = recover() }()
	return index.Summary()
}
