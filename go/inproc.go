package main

// 进程内回退路径(--no-procs): 工具循环直接在主进程跑。

import (
	"fmt"
)

func speakInproc(run *Run, name, prompt, extraSys, prefill string) string {
	a := Agents[name]
	if a == nil {
		return "*(unknown agent " + name + ")*"
	}
	run.SM.Set(name, "thinking", "")
	msgs := []Message{{Role: "system", Content: sysContent(a, extraSys)}}
	for _, d := range a.Developer {
		msgs = append(msgs, Message{Role: "developer", Content: d})
	}
	if rm := reminderText(); rm != "" {
		msgs = append(msgs, Message{Role: "developer", Content: rm})
	}
	msgs = append(msgs, Message{Role: "user", Content: prompt})
	if prefill != "" {
		msgs = append(msgs, Message{Role: "assistant", Content: prefill})
	}
	tools := effectiveTools(a)
	out := ""
	for round := 0; round < G.MaxToolRounds; round++ {
		msg, usage, err := callModel(a, name, msgs, tools,
			func(text, kind string) {
				if kind == "content" {
					fmt.Print(text)
				}
			}, run)
		if err != nil {
			out = "*(call failed: " + err.Error() + ")*"
			break
		}
		for k, v := range usage {
			if f, ok := v.(float64); ok {
				run.Usage[k] += int(f)
			}
		}
		if len(msg.ToolCalls) == 0 {
			out = msg.Content
			break
		}
		run.SM.Set(name, "tooling", "")
		msgs = append(msgs, msg)
		for _, tc := range msg.ToolCalls {
			res := RunTool(a, tc.Function.Name, tc.Function.Arguments)
			run.Say(name+"(tool)", fmt.Sprintf("### %s\n%s\n→ %s",
				tc.Function.Name, trunc(tc.Function.Arguments, 300),
				trunc(res, 300)), nil)
			msgs = append(msgs, Message{Role: "tool",
				ToolCallID: tc.ID, Content: res})
		}
		run.SM.Set(name, "thinking", "")
	}
	run.SM.Set(name, "done", "")
	fmt.Println(out)
	wrote := run.ExtractArtifacts(out)
	run.Say(name, out, wrote)
	return out
}
