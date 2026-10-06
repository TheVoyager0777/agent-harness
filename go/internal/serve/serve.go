package serve

// serve: 常驻调度 daemon。固定控制端口(harness.json serve_port,默认 8390),
// agent 进程按需 spawn 并跨任务保活,提交经 TCP JSONL。
//   harness serve            起 daemon
//   harness submit MODE "topic" [--agents a,b] [--rounds N]
//   harness serve-status     daemon 状态
//   harness serve-stop       停 daemon

import (
	"encoding/json"
	"fmt"
	"github.com/xjcdw0777/agent-harness/internal/config"
	"github.com/xjcdw0777/agent-harness/internal/core"
	"github.com/xjcdw0777/agent-harness/internal/modes"
	"github.com/xjcdw0777/agent-harness/internal/proc"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type daemon struct {
	mu    sync.Mutex
	jobs  []jobInfo
	procs map[string]*proc.AgentProc // daemon 级保活池
}

type jobInfo struct {
	ID    string `json:"id"`
	Mode  string `json:"mode"`
	Topic string `json:"topic"`
	State string `json:"state"`
	Ep    string `json:"ep,omitempty"`
	At    string `json:"at"`
}

var D *daemon

func init() {
	modes.StartControl = func(run *core.Run) int { return ServeControl(run, 0) }
	proc.OnSpawned = func(name string, p *proc.AgentProc) {
		if D != nil {
			D.procs[name] = p
		}
	}
}

func Serve() {
	port := config.G.ServePort
	if port <= 0 {
		port = 8390
	}
	ln, err := net.Listen("tcp4", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		fmt.Fprintln(os.Stderr, "serve 端口占用:", err)
		os.Exit(1)
	}
	D = &daemon{procs: map[string]*proc.AgentProc{}}
	pidF := filepath.Join(config.Root, "runs", "pid")
	os.MkdirAll(filepath.Dir(pidF), 0o755)
	os.WriteFile(pidF, []byte(fmt.Sprintf("%d %d", os.Getpid(), port)), 0o644)
	fmt.Printf("harness serve :%d  home=%s agents=%d\n", port, config.Root, len(config.Agents))
	for {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		go handleServeConn(c)
	}
}

func handleServeConn(c net.Conn) {
	defer c.Close()
	var cmd map[string]any
	if json.NewDecoder(c).Decode(&cmd) != nil {
		return
	}
	op, _ := cmd["op"].(string)
	reply := func(v any) { json.NewEncoder(c).Encode(v) }

	switch op {
	case "submit":
		mode, _ := cmd["mode"].(string)
		topic, _ := cmd["topic"].(string)
		rounds := 3
		if r, ok := cmd["rounds"].(float64); ok {
			rounds = int(r)
		}
		var roster []string
		if rs, ok := cmd["agents"].([]any); ok {
			for _, x := range rs {
				roster = append(roster, fmt.Sprint(x))
			}
		}
		if len(roster) == 0 {
			if s, ok := cmd["agents"].(string); ok && s != "" {
				roster = strings.Split(s, ",")
			}
		}
		j := jobInfo{ID: core.RandHex(4), Mode: mode, Topic: topic,
			State: "queued", At: time.Now().Format("15:04:05"),
			Ep: strings.TrimSpace(fmt.Sprint(cmd["endpoint"]))}
		D.mu.Lock()
		D.jobs = append(D.jobs, j)
		D.mu.Unlock()
		go runJob(j.ID, mode, topic, rounds, roster, j.Ep)
		reply(map[string]any{"ok": true, "job": j.ID})
	case "status":
		D.mu.Lock()
		jobs := append([]jobInfo{}, D.jobs...)
		procs := map[string]bool{}
		for n, p := range D.procs {
			procs[n] = p.Alive()
		}
		D.mu.Unlock()
		reply(map[string]any{"jobs": jobs, "procs": procs})
	case "inject":
		ag, _ := cmd["agent"].(string)
		if p := D.procs[ag]; p != nil {
			p.Inject(fmt.Sprint(cmd["msg"]))
			reply(map[string]any{"ok": true})
		} else {
			reply(map[string]any{"ok": false, "error": "no live agent"})
		}
	case "stop":
		reply(map[string]any{"ok": true})
		go func() { time.Sleep(200 * time.Millisecond); os.Exit(0) }()
	default:
		reply(map[string]any{"error": "unknown op"})
	}
}

func runJob(id, mode, topic string, rounds int, roster []string, ep string) {
	setJob := func(st string) {
		D.mu.Lock()
		for i := range D.jobs {
			if D.jobs[i].ID == id {
				D.jobs[i].State = st
			}
		}
		D.mu.Unlock()
	}
	setJob("running")
	proc.DaemonMode = true
	if ep != "" && ep != "<nil>" {
		core.JobEp = ep
		defer func() { core.JobEp = "" }()
	}
	defer func() { proc.DaemonMode = false }()
	switch mode {
	case "brainstorm":
		modes.Brainstorm(topic, rounds, roster)
	case "council":
		modes.Council(topic, roster)
	case "task":
		modes.Task(topic, roster)
	case "ask":
		modes.Ask(topic, "", "")
	default:
		setJob("error: unknown mode")
		return
	}
	setJob("done")
	fmt.Printf("[job %s] %s done\n", id, mode)
}

// ---------- 客户端 ----------

func Call(obj map[string]any) map[string]any {
	port := config.G.ServePort
	if port <= 0 {
		port = 8390
	}
	c, err := net.DialTimeout("tcp4", fmt.Sprintf("127.0.0.1:%d", port),
		3*time.Second)
	if err != nil {
		return map[string]any{"error": "daemon 未运行 (harness serve 启动)"}
	}
	defer c.Close()
	json.NewEncoder(c).Encode(obj)
	var res map[string]any
	json.NewDecoder(c).Decode(&res)
	return res
}
