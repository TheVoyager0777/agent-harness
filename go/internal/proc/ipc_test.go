package proc

import (
	"encoding/json"
	"io"
	"testing"
)

func TestChildIPCRoundTrip(t *testing.T) {
	c2pR, c2pW := io.Pipe() // child → parent
	p2cR, p2cW := io.Pipe() // parent → child

	ipc := &childIPC{enc: json.NewEncoder(c2pW),
		pend: map[float64]chan map[string]any{}}

	// child 侧读循环: res → pend
	go func() {
		dec := json.NewDecoder(p2cR)
		for {
			var m map[string]any
			if dec.Decode(&m) != nil {
				return
			}
			if m["type"] == "res" {
				id, _ := m["id"].(float64)
				ipc.pmu.Lock()
				if q := ipc.pend[id]; q != nil {
					q <- m
				}
				ipc.pmu.Unlock()
			}
		}
	}()

	// 假主进程: 收 req 回 res
	go func() {
		dec := json.NewDecoder(c2pR)
		enc := json.NewEncoder(p2cW)
		for {
			var m map[string]any
			if dec.Decode(&m) != nil {
				return
			}
			if m["type"] == "req" && m["method"] == "tool.call" {
				enc.Encode(map[string]any{"type": "res", "id": m["id"],
					"result": map[string]any{"result": "filedata"}})
			}
		}
	}()

	res, err := ipc.call("tool.call",
		map[string]any{"name": "read_file", "args": "{}"}, 5)
	if err != nil {
		t.Fatal(err)
	}
	if res["result"] != "filedata" {
		t.Fatalf("res=%v", res)
	}
	// 超时路径
	if _, err = ipc.call("no.such", map[string]any{}, 1); err == nil {
		t.Fatal("无响应应超时")
	}
}
