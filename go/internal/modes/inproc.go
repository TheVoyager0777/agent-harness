package modes

// 进程内回退路径(--no-procs): 工具循环直接在主进程跑。

import (
	"fmt"
	"github.com/xjcdw0777/agent-harness/internal/config"
	"github.com/xjcdw0777/agent-harness/internal/core"
	"github.com/xjcdw0777/agent-harness/internal/ctx"
	"github.com/xjcdw0777/agent-harness/internal/model"
	"github.com/xjcdw0777/agent-harness/internal/tools"
)

func speakInproc(run *core.Run, name, prompt, extraSys, prefill string) string {
	a := config.Agents[name]
	if a == nil {
		return "*(unknown agent " + name + ")*"
	}
	run.SM.Set(name, "thinking", "")
	msgs := []config.Message{{Role: "system", Content: core.SysContent(a, extraSys)}}
	for _, d := range a.Developer {
		msgs = append(msgs, config.Message{Role: "developer", Content: d})
	}
	if rm := core.ReminderText(); rm != "" {
		msgs = append(msgs, config.Message{Role: "developer", Content: rm})
	}
	// 会话复用(inproc 无自持 history, 注入档案种子)
	if config.ResumeEnabled() {
		msgs = append(msgs, ctx.LoadSession(name, 8000)...)
	}
	rawPrompt := prompt
	if core.AgentHasTool(a, "kb_search") {
		if blk := ctx.KBRecall(prompt, 6); blk != "" {
			prompt = blk + "\n\n" + prompt
		}
	}
	msgs = append(msgs, config.Message{Role: "user", Content: prompt})
	if prefill != "" {
		msgs = append(msgs, config.Message{Role: "assistant", Content: prefill})
	}
	toolList := tools.Effective(a)
	out := ""
	for round := 0; round < config.G.MaxToolRounds; round++ {
		msg, usage, err := model.Call(a, name, msgs, toolList,
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
			res := tools.Run(a, tc.Function.Name, tc.Function.Arguments)
			run.Say(name+"(tool)", fmt.Sprintf("### %s\n%s\n→ %s",
				tc.Function.Name, core.Trunc(tc.Function.Arguments, 300),
				core.Trunc(res, 300)), nil)
			msgs = append(msgs, config.Message{Role: "tool",
				ToolCallID: tc.ID, Content: res})
		}
		run.SM.Set(name, "thinking", "")
	}
	tools.Ex.ReleaseAll(name)
	run.SM.Set(name, "done", "")
	// inproc 同样记档案, 保证 ctx_search/会话复用可用
	ctx.Append(name,
		config.Message{Role: "user", Content: rawPrompt},
		config.Message{Role: "assistant", Content: out})
	fmt.Println(out)
	wrote := run.ExtractArtifacts(out)
	run.Say(name, out, wrote)
	return out
}
