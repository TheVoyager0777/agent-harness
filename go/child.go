package main

// agent 子进程本体: `_agent NAME`。
// 私有状态: history(自持,主进程不拼) / inbox(inject+bus) / 工具循环。
// 资源全部 IPC 回主进程。

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"time"
)

type childIPC struct {
	enc  *json.Encoder
	wmu  sync.Mutex
	pend map[float64]chan map[string]any
	pmu  sync.Mutex
	seq  float64
}

func (c *childIPC) send(m map[string]any) {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	c.enc.Encode(m)
}

func (c *childIPC) call(method string, params map[string]any, timeoutS int) (map[string]any, error) {
	c.pmu.Lock()
	c.seq++
	id := c.seq
	ch := make(chan map[string]any, 1)
	c.pend[id] = ch
	c.pmu.Unlock()
	defer func() { c.pmu.Lock(); delete(c.pend, id); c.pmu.Unlock() }()
	c.send(map[string]any{"type": "req", "id": id, "method": method, "params": params})
	select {
	case r := <-ch:
		if e, ok := r["error"]; ok {
			return nil, fmt.Errorf("%v", e)
		}
		res, _ := r["result"].(map[string]any)
		return res, nil
	case <-timeAfter(timeoutS):
		return nil, fmt.Errorf("ipc timeout: %s", method)
	}
}

func agentMain(name string) {
	agent := Agents[name]
	if agent == nil {
		fmt.Fprintln(os.Stderr, "unknown agent "+name)
		os.Exit(1)
	}
	ipc := &childIPC{enc: json.NewEncoder(os.Stdout),
		pend: map[float64]chan map[string]any{}}
	inbox := make(chan map[string]any, 64)
	cmds := make(chan map[string]any, 8)
	history := []Message{}

	// stdin reader: res → pend, push → inbox, cmd → cmds
	go func() {
		sc := bufio.NewScanner(os.Stdin)
		sc.Buffer(make([]byte, 1<<20), 8<<20)
		for sc.Scan() {
			var m map[string]any
			if json.Unmarshal(sc.Bytes(), &m) != nil {
				continue
			}
			switch m["type"] {
			case "res":
				id, _ := m["id"].(float64)
				ipc.pmu.Lock()
				if q := ipc.pend[id]; q != nil {
					q <- m
				}
				ipc.pmu.Unlock()
			case "push":
				inbox <- m
			case "cmd":
				cmds <- m
			}
		}
		close(cmds)
	}()

	drainInbox := func(msgs *[]Message) {
		for {
			select {
			case ev := <-inbox:
				if ev["event"] == "inject" {
					d, _ := ev["data"].(map[string]any)
					*msgs = append(*msgs, Message{Role: "user",
						Content: "[注入消息] " + fmt.Sprint(d["text"])})
				} else if ev["event"] == "bus" {
					d, _ := ev["data"].(map[string]any)
					db, _ := json.Marshal(d["data"])
					*msgs = append(*msgs, Message{Role: "developer",
						Content: fmt.Sprintf("[总线事件 %v] from=%v data=%s note=%v",
							d["event"], d["from"], trunc(string(db), 300), d["note"])})
				}
			default:
				return
			}
		}
	}

	doTurn := func(params map[string]any) string {
		// 自持 history: 不接收主进程历史;主进程只发新 prompt/材料
		msgs := []Message{{Role: "system", Content: sysContent(agent,
			strOf(params, "extra_sys"))}}
		for _, d := range agent.Developer {
			msgs = append(msgs, Message{Role: "developer", Content: d})
		}
		if rm := reminderText(); rm != "" {
			msgs = append(msgs, Message{Role: "developer", Content: rm})
		}
		msgs = append(msgs, history...)
		msgs = append(msgs, Message{Role: "user", Content: strOf(params, "prompt")})
		if pf := strOf(params, "prefill"); pf != "" {
			msgs = append(msgs, Message{Role: "assistant", Content: pf})
		}
		tools := effectiveTools(agent)
		out := ""
		var msg Message
		for round := 0; round < G.MaxToolRounds; round++ {
			drainInbox(&msgs)
			res, err := ipc.call("model.call",
				map[string]any{"messages": msgs, "tools": tools}, G.TimeoutS+120)
			if err != nil {
				out = "*(call failed: " + err.Error() + ")*"
				break
			}
			mj, _ := json.Marshal(res["message"])
			json.Unmarshal(mj, &msg)
			if len(msg.ToolCalls) == 0 {
				out = msg.Content
				break
			}
			msgs = append(msgs, msg)
			for _, tc := range msg.ToolCalls {
				tr, err := ipc.call("tool.call", map[string]any{
					"name": tc.Function.Name, "args": tc.Function.Arguments}, 360)
				content := ""
				if err != nil {
					content = "tool ipc error: " + err.Error()
				} else {
					content, _ = tr["result"].(string)
				}
				msgs = append(msgs, Message{Role: "tool",
					ToolCallID: tc.ID, Content: content})
			}
		}
		// 自持 history: 记录本轮 user+assistant
		history = append(history,
			Message{Role: "user", Content: strOf(params, "prompt")},
			Message{Role: "assistant", Content: out})
		if len(history) > 40 {
			history = history[len(history)-40:]
		}
		return out
	}

	for m := range cmds {
		if m == nil {
			return
		}
		switch m["cmd"] {
		case "shutdown":
			return
		case "turn":
			params, _ := m["params"].(map[string]any)
			var text string
			func() {
				defer func() {
					if r := recover(); r != nil {
						text = ""
					}
				}()
				text = doTurn(params)
			}()
			ipc.send(map[string]any{"type": "res", "id": m["id"], "cmd": "turn",
				"ok": true, "result": map[string]any{"text": text}})
		}
	}
}

func strOf(m map[string]any, k string) string {
	if v, ok := m[k].(string); ok {
		return v
	}
	return ""
}

func timeAfter(s int) <-chan time.Time {
	if s <= 0 {
		s = 300
	}
	return time.After(time.Duration(s) * time.Second)
}
