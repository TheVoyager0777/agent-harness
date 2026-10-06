package proc

// agent 子进程本体: `_agent NAME`。
// 私有状态: history(自持,主进程不拼) / inbox(inject+bus) / 工具循环。
// 资源全部 IPC 回主进程。

import (
	"bufio"
	"encoding/json"
	"fmt"
	"github.com/xjcdw0777/agent-harness/internal/config"
	"github.com/xjcdw0777/agent-harness/internal/core"
	"github.com/xjcdw0777/agent-harness/internal/tools"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/xjcdw0777/agent-harness/internal/ctx"
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

func AgentMain(name string) {
	agent := config.Agents[name]
	if agent == nil {
		fmt.Fprintln(os.Stderr, "unknown agent "+name)
		os.Exit(1)
	}
	ipc := &childIPC{enc: json.NewEncoder(os.Stdout),
		pend: map[float64]chan map[string]any{}}
	inbox := make(chan map[string]any, 64)
	cmds := make(chan map[string]any, 8)
	done := make(chan struct{})
	history := []config.Message{}
	ctx.SyncConf()
	// 会话复用: --resume / context.resume 时从档案+压缩态恢复历史
	if config.ResumeEnabled() {
		history = ctx.LoadSession(name, 8000)
	}
	comp := ctx.NewCompactor(agent, func(msgs []config.Message) (string, error) {
		res, err := ipc.call("model.call",
			map[string]any{"messages": msgs}, config.G.TimeoutS+120)
		if err != nil {
			return "", err
		}
		mj, _ := json.Marshal(res["message"])
		var mm config.Message
		json.Unmarshal(mj, &mm)
		return mm.Content, nil
	})

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
				select {
				case inbox <- m:
				case <-done:
					return
				}
			case "cmd":
				select {
				case cmds <- m:
				case <-done:
					return
				}
			}
		}
		close(cmds)
	}()

	defer close(done)

	drainInbox := func(msgs *[]config.Message) {
		for {
			select {
			case ev := <-inbox:
				if ev["event"] == "inject" {
					d, _ := ev["data"].(map[string]any)
					*msgs = append(*msgs, config.Message{Role: "user",
						Content: "[注入消息] " + fmt.Sprint(d["text"])})
				} else if ev["event"] == "bus" {
					d, _ := ev["data"].(map[string]any)
					db, _ := json.Marshal(d["data"])
					*msgs = append(*msgs, config.Message{Role: "developer",
						Content: fmt.Sprintf("[总线事件 %v] from=%v data=%s note=%v",
							d["event"], d["from"], core.Trunc(string(db), 300), d["note"])})
				}
			default:
				return
			}
		}
	}

	doTurn := func(params map[string]any) string {
		// 0阻塞压缩: 有完成摘要则换入, 超阈值后台起压缩
		history = comp.Apply(history)
		// 自持 history: 不接收主进程历史;主进程只发新 prompt/材料
		msgs := []config.Message{{Role: "system", Content: core.SysContent(agent,
			strOf(params, "extra_sys"))}}
		for _, d := range agent.Developer {
			msgs = append(msgs, config.Message{Role: "developer", Content: d})
		}
		if rm := core.ReminderText(); rm != "" {
			msgs = append(msgs, config.Message{Role: "developer", Content: rm})
		}
		msgs = append(msgs, history...)
		// 主动记忆注入(Anuma 式): 对本轮 prompt 召回知识库,前缀注入 user 消息
		prompt := strOf(params, "prompt")
		if core.AgentHasTool(agent, "kb_search") {
			if blk := ctx.KBRecall(prompt, 6); blk != "" {
				prompt = blk + "\n\n" + prompt
			}
		}
		msgs = append(msgs, config.Message{Role: "user", Content: prompt})
		if pf := strOf(params, "prefill"); pf != "" {
			msgs = append(msgs, config.Message{Role: "assistant", Content: pf})
		}
		toolList := tools.Effective(agent)
		out := ""
		for round := 0; round < config.G.MaxToolRounds; round++ {
			drainInbox(&msgs)
			res, err := ipc.call("model.call",
				map[string]any{"messages": msgs, "tools": toolList}, config.G.TimeoutS+120)
			if err != nil {
				out = "*(call failed: " + err.Error() + ")*"
				break
			}
			// 必须整体重建: Unmarshal 不清空缺失的 tool_calls, 会残留上一轮
			var msg config.Message
			mj, _ := json.Marshal(res["message"])
			json.Unmarshal(mj, &msg)
			if len(msg.ToolCalls) == 0 {
				out = msg.Content
				break
			}
			msgs = append(msgs, msg)
			// 一轮全部 tool_calls 合并为单个 batch, 主进程按资源锁并行调度
			calls := make([]map[string]any, len(msg.ToolCalls))
			for i, tc := range msg.ToolCalls {
				calls[i] = map[string]any{"id": tc.ID, "name": tc.Function.Name,
					"args": tc.Function.Arguments}
			}
			tr, err := ipc.call("tool.batch", map[string]any{"calls": calls},
				360*len(calls))
			resByID := map[string]string{}
			if err != nil {
				for _, tc := range msg.ToolCalls {
					resByID[tc.ID] = "tool ipc error: " + err.Error()
				}
			} else if rlist, ok := tr["results"].([]any); ok {
				for _, r := range rlist {
					rm, _ := r.(map[string]any)
					rid, _ := rm["id"].(string)
					resByID[rid], _ = rm["result"].(string)
				}
			}
			for _, tc := range msg.ToolCalls {
				msgs = append(msgs, config.Message{Role: "tool",
					ToolCallID: tc.ID, Content: resByID[tc.ID]})
			}
		}
		// 轮次耗尽/空应答: 强制收尾轮(不带工具)让模型基于已获得信息给结论
		if out == "" {
			msgs = append(msgs, config.Message{Role: "developer",
				Content: "工具轮次已用完。请基于已获得的信息直接给出结论/成果,不要再发起工具调用。"})
			res, err := ipc.call("model.call",
				map[string]any{"messages": msgs}, config.G.TimeoutS+120)
			if err == nil {
				var msg config.Message
				mj, _ := json.Marshal(res["message"])
				json.Unmarshal(mj, &msg)
				out = msg.Content
			}
		}
		// 自持 history: 记录本轮 user+assistant
		history = append(history,
			config.Message{Role: "user", Content: strOf(params, "prompt")},
			config.Message{Role: "assistant", Content: out})
		ctx.Append(agent.Name,
			config.Message{Role: "user", Content: strOf(params, "prompt")},
			config.Message{Role: "assistant", Content: out})
		// 记忆抽取 sidecar(可选): 异步用小模型语义把本轮可持久知识提炼入库
		if config.G != nil && config.G.Context.MemoryExtract && len(out) > 200 {
			go memoryExtract(ipc, agent, strOf(params, "prompt"), out)
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

const memExtractPrompt = `你是记忆抽取器。从给出的对话片段中提炼值得跨会话保留的条目。
只输出 <knowledge>条目</knowledge> 块(可多个);没有值得保留的就输出 <knowledge></knowledge> 或空。
值得保留: 实测结论与数字、已做决策及理由、判别实验结果、环境/工具坑、后续要用的约束。
不要保留: 推测、客套、过程性描述、已在本轮上下文声明过的规则。
每条一行内完成,语言随原文。`

// memoryExtract: Anuma memory_extract sidecar 同构 — 异步抽取, 不阻塞主循环。
func memoryExtract(ipc *childIPC, agent *config.Agent, userText, out string) {
	defer func() { _ = recover() }()
	msgs := []config.Message{
		{Role: "system", Content: memExtractPrompt},
		{Role: "user", Content: "Q: " + core.Trunc(userText, 1500) +
			"\n\nA: " + core.Trunc(out, 4000)},
	}
	res, err := ipc.call("model.call",
		map[string]any{"messages": msgs}, config.G.TimeoutS+60)
	if err != nil {
		return
	}
	var mm config.Message
	mj, _ := json.Marshal(res["message"])
	json.Unmarshal(mj, &mm)
	for _, k := range extractKB(mm.Content) {
		ctx.KBWrite(agent.Name, "auto", k, nil)
	}
}

// extractKB: <knowledge> 块提取(与 ctx 包同语义, 本地副本避免跨包循环)。
func extractKB(text string) []string {
	var out []string
	rest := text
	for {
		i := strings.Index(rest, "<knowledge>")
		if i < 0 {
			break
		}
		rest = rest[i+len("<knowledge>"):]
		j := strings.Index(rest, "</knowledge>")
		if j < 0 {
			break
		}
		if s := strings.TrimSpace(rest[:j]); s != "" {
			out = append(out, s)
		}
		rest = rest[j+len("</knowledge>"):]
	}
	return out
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
