package main

// serve: 常驻调度 daemon。固定控制端口(harness.json serve_port,默认 8390),
// agent 进程按需 spawn 并跨任务保活,提交经 TCP JSONL。
//   harness serve            起 daemon
//   harness submit MODE "topic" [--agents a,b] [--rounds N]
//   harness serve-status     daemon 状态
//   harness serve-stop       停 daemon

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type daemon struct {
	mu     sync.Mutex
	jobs   []jobInfo
	procs  map[string]*AgentProc // daemon 级保活池
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

func serve() {
	port := G.ServePort
	if port <= 0 {
		port = 8390
	}
	ln, err := net.Listen("tcp4", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		fmt.Fprintln(os.Stderr, "serve 端口占用:", err)
		os.Exit(1)
	}
	D = &daemon{procs: map[string]*AgentProc{}}
	pidF := filepath.Join(Root, "runs", "serve.pid")
	os.MkdirAll(filepath.Dir(pidF), 0o755)
	os.WriteFile(pidF, []byte(fmt.Sprintf("%d %d", os.Getpid(), port)), 0o644)
	fmt.Printf("harness serve :%d  home=%s agents=%d\n", port, Root, len(Agents))
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
		j := jobInfo{ID: randHex(4), Mode: mode, Topic: topic,
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
			procs[n] = p.cmd.ProcessState == nil
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
	DaemonMode = true
	if ep != "" && ep != "<nil>" {
		JobEp = ep
		defer func() { JobEp = "" }()
	}
	defer func() { DaemonMode = false }()
	switch mode {
	case "brainstorm":
		brainstorm(topic, rounds, roster)
	case "council":
		council(topic, roster)
	case "task":
		task(topic, roster)
	case "ask":
		ask(topic, "", "")
	default:
		setJob("error: unknown mode")
		return
	}
	setJob("done")
	fmt.Printf("[job %s] %s done\n", id, mode)
}
