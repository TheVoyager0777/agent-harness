package main

// 控制套接字: status / inject / state / submit(serve 模式)。

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"
)

func serveControl(run *Run, port int) int {
	ln, err := net.Listen("tcp4", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		if port != 0 {
			fmt.Fprintln(os.Stderr, "control port busy, fallback random")
			return serveControl(run, 0)
		}
		return 0
	}
	actual := ln.Addr().(*net.TCPAddr).Port
	os.WriteFile(filepath.Join(run.Dir, "control.port"),
		[]byte(fmt.Sprint(actual)), 0o644)
	os.WriteFile(filepath.Join(Root, "runs", "latest.port"),
		[]byte(fmt.Sprintf("%d %s", actual, run.Dir)), 0o644)
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				var cmd map[string]any
				if json.NewDecoder(c).Decode(&cmd) != nil {
					return
				}
				res := ctlDispatch(run, cmd)
				json.NewEncoder(c).Encode(res)
			}()
		}
	}()
	return actual
}

func ctlDispatch(run *Run, cmd map[string]any) map[string]any {
	op, _ := cmd["op"].(string)
	switch op {
	case "status":
		procs := map[string]bool{}
		procsMu.Lock()
		for n, p := range Procs {
			procs[n] = p.cmd.ProcessState == nil
		}
		procsMu.Unlock()
		return map[string]any{"states": run.SM.Snap(), "usage": run.Usage,
			"procs": procs}
	case "inject":
		ag, _ := cmd["agent"].(string)
		if p := Procs[ag]; p != nil {
			p.Inject(fmt.Sprint(cmd["msg"]))
			return map[string]any{"ok": true}
		}
		return map[string]any{"ok": false, "error": "no such live agent"}
	case "state":
		ag, _ := cmd["agent"].(string)
		return map[string]any{"state": run.SM.Snap()[ag]}
	default:
		return map[string]any{"error": "unknown op " + op}
	}
}

func ctlCall(rundir string, obj map[string]any) map[string]any {
	port := ""
	if rundir != "" {
		if b, err := os.ReadFile(filepath.Join(rundir, "control.port")); err == nil {
			port = strings.TrimSpace(string(b))
		}
	}
	if port == "" {
		if b, err := os.ReadFile(filepath.Join(Root, "runs", "latest.port")); err == nil {
			port = strings.Fields(string(b))[0]
		} else {
			return map[string]any{"error": "no live run"}
		}
	}
	c, err := net.DialTimeout("tcp4", "127.0.0.1:"+port, 5*time.Second)
	if err != nil {
		return map[string]any{"error": "control port dead"}
	}
	defer c.Close()
	json.NewEncoder(c).Encode(obj)
	var res map[string]any
	bufio.NewReader(c)
	json.NewDecoder(c).Decode(&res)
	return res
}
