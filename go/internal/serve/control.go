package serve

// 控制套接字: status / inject / state / submit(serve 模式)。

import (
	"github.com/xjcdw0777/agent-harness/internal/config"
	"github.com/xjcdw0777/agent-harness/internal/core"
	"github.com/xjcdw0777/agent-harness/internal/proc"
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"
)

func ServeControl(run *core.Run, port int) int {
	ln, err := net.Listen("tcp4", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		if port != 0 {
			fmt.Fprintln(os.Stderr, "control port busy, fallback random")
			return ServeControl(run, 0)
		}
		return 0
	}
	actual := ln.Addr().(*net.TCPAddr).Port
	os.WriteFile(filepath.Join(run.Dir, "control.port"),
		[]byte(fmt.Sprint(actual)), 0o644)
	os.WriteFile(filepath.Join(config.Root, "runs", "latest.port"),
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

func ctlDispatch(run *core.Run, cmd map[string]any) map[string]any {
	op, _ := cmd["op"].(string)
	switch op {
	case "status":
		procs := proc.Snapshot()
		return map[string]any{"states": run.SM.Snap(), "usage": run.Usage,
			"procs": procs}
	case "inject":
		ag, _ := cmd["agent"].(string)
		if p := proc.Procs[ag]; p != nil {
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

func CtlCall(rundir string, obj map[string]any) map[string]any {
	port := ""
	if rundir != "" {
		if b, err := os.ReadFile(filepath.Join(rundir, "control.port")); err == nil {
			port = strings.TrimSpace(string(b))
		}
	}
	if port == "" {
		if b, err := os.ReadFile(filepath.Join(config.Root, "runs", "latest.port")); err == nil {
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
