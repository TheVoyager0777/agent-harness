package modes

// 模式: ask / chat / brainstorm / council / task / serve / submit
// agent 自持 history: 主进程只传新信息(他人发言拼进 prompt),不重建历史。

import (
	"github.com/xjcdw0777/agent-harness/internal/config"
	"github.com/xjcdw0777/agent-harness/internal/core"
	"github.com/xjcdw0777/agent-harness/internal/proc"
		"bufio"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strings"
	"sync"
)

var UseProcs = true

// StartControl: 运行期控制口钩子, serve 包装配(解耦 modes→serve)。
var StartControl = func(run *core.Run) int { return 0 }

func turnOf(run *core.Run, name, prompt, extraSys, prefill string) string {
	if UseProcs {
		p := proc.Spawn(name, run)
		if p == nil {
			return "*(unknown agent " + name + ")*"
		}
		return p.Turn(prompt, extraSys, prefill, 0)
	}
	return speakInproc(run, name, prompt, extraSys, prefill)
}

func Ask(topic, agentName, prefill string) {
	run := core.NewRun("ask", topic)
	StartControl(run)
	defer proc.StopAll(run)
	name := agentName
	if name == "" {
		for n := range config.Agents {
			name = n
			break
		}
	}
	turnOf(run, name, topic, "", prefill)
	fmt.Printf("\nrun dir: %s | tokens %v\n", run.Dir, run.Usage)
}

func Chat(agentName string) {
	name := agentName
	if name == "" {
		for n := range config.Agents {
			name = n
			break
		}
	}
	run := core.NewRun("chat", "interactive:"+name)
	StartControl(run)
	defer proc.StopAll(run)
	fmt.Printf("chat %s (%s) | /use /agents /inject /send A msg /status /quit\n",
		name, config.Agents[name].Model)
	sc := bufio.NewScanner(os.Stdin)
	for {
		fmt.Print("\n>>> ")
		if !sc.Scan() {
			break
		}
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		switch {
		case line == "/quit" || line == "/q":
			run.Say("system", fmt.Sprintf("chat ended, tokens=%v", run.Usage), nil)
			fmt.Println("run dir: " + run.Dir)
			return
		case line == "/agents":
			for n, a := range config.Agents {
				fmt.Printf("  %-16s %-26s @%s\n", n, a.Model, a.Endpoint)
			}
		case line == "/status":
			for n, s := range run.SM.Snap() {
				fmt.Printf("  %-16s %-10s %s\n", n, s.State, s.Detail)
			}
		case strings.HasPrefix(line, "/use "):
			n := strings.TrimSpace(line[5:])
			if config.Agents[n] != nil {
				name = n
				if p := proc.Procs[n]; p != nil {
					p.Stop() // 新会话: 重启 agent 进程清 history
					delete(proc.Procs, n)
				}
				fmt.Printf("switched -> %s, history cleared\n", n)
			} else {
				fmt.Println("未知 agent " + n)
			}
		case strings.HasPrefix(line, "/inject "):
			if p := proc.Spawn(name, run); p != nil {
				p.Inject(line[8:])
				fmt.Println("(injected)")
			}
		case strings.HasPrefix(line, "/send "):
			parts := strings.SplitN(line[6:], " ", 2)
			if len(parts) == 2 {
				if p := proc.Spawn(parts[0], run); p != nil {
					p.Inject("[chat] " + parts[1])
					fmt.Println("(sent to " + parts[0] + ")")
				} else {
					fmt.Println("未知 agent " + parts[0])
				}
			}
		default:
			turnOf(run, name, line, "", "")
		}
	}
}

func Brainstorm(topic string, rounds int, roster []string) {
	run := core.NewRun("brainstorm", topic)
	StartControl(run)
	defer proc.StopAll(run)
	var names []string
	for _, n := range roster {
		if config.Agents[n] != nil {
			names = append(names, n)
		}
	}
	if len(names) == 0 {
		for n := range config.Agents {
			names = append(names, n)
		}
	}
	// 自持 history: 每轮 prompt 只带"新信息"(他人上轮发言)
	latest := map[string]string{}
	for rd := 1; rd <= rounds; rd++ {
		run.Say("system", fmt.Sprintf("### --- round %d ---", rd), nil)
		core.BusPub("main", "round.start", map[string]any{"round": rd})
		for _, name := range names {
			var prompt string
			if rd == 1 {
				prompt = fmt.Sprintf("议题: %s\n你是圆桌成员。基于上下文给出专业判断。可以: 假设、判别实验、指出他人盲点、用工具取证。具体可操作,400 字内。", topic)
			} else {
				var others []string
				for n, o := range latest {
					if n != name {
						others = append(others, fmt.Sprintf("[%s] %s", n, o))
					}
				}
				prompt = "继续。这是其他成员上一轮的发言:\n\n" +
					strings.Join(others, "\n\n") + "\n\n回应新观点,深化或反驳。"
			}
			out := turnOf(run, name, prompt, "", "")
			latest[name] = out
			core.BusPub(name, "agent.spoke", map[string]any{"round": rd, "chars": len(out)})
		}
	}
	orch := "orchestrator"
	if config.Agents[orch] == nil {
		orch = names[0]
	}
	var sb strings.Builder
	for n, o := range latest {
		sb.WriteString(fmt.Sprintf("[%s] %s\n\n", n, o))
	}
	out := turnOf(run, orch,
		"圆桌最终发言汇总:\n\n"+sb.String()+
			"\n收敛: (1)已共识假设 (2)判别实验清单(按信息量/成本) (3)下一步分工。", "", "")
	run.Say("orchestrator(synthesis)", out, run.ExtractArtifacts(out))
	fmt.Printf("\nrun dir: %s | tokens %v\n", run.Dir, run.Usage)
}

func Council(question string, roster []string) {
	run := core.NewRun("council", question)
	StartControl(run)
	defer proc.StopAll(run)
	var names []string
	for _, n := range roster {
		if config.Agents[n] != nil {
			names = append(names, n)
		}
	}
	if len(names) == 0 {
		for n := range config.Agents {
			names = append(names, n)
		}
	}
	// 阶段1: 并行独立作答
	answers := map[string]string{}
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, n := range names {
		wg.Add(1)
		go func(n string) {
			defer wg.Done()
			out := turnOf(run, n,
				"问题: "+question+"\n独立给出分析和结论(勿参考他人)。具体、可用工具取证。", "", "")
			mu.Lock()
			answers[n] = out
			mu.Unlock()
			core.BusPub(n, "agent.spoke", map[string]any{"chars": len(out)})
		}(n)
	}
	wg.Wait()
	// 阶段2: 互评(各 agent 自持 history,只补他人答案)
	for _, name := range names {
		var others []string
		for n, a := range answers {
			if n != name {
				others = append(others, fmt.Sprintf("[%s] %s", n, a))
			}
		}
		turnOf(run, name,
			"问题: "+question+"\n其他人的答案:\n"+strings.Join(others, "\n\n")+
				"\n评审: 最强点、最弱点、被漏掉的变量。", "", "")
	}
	orch := "orchestrator"
	if config.Agents[orch] == nil {
		orch = names[0]
	}
	var sb strings.Builder
	for n, a := range answers {
		sb.WriteString(fmt.Sprintf("[%s] %s\n\n", n, a))
	}
	out := turnOf(run, orch,
		"综合作答与互评(在你自己的发言基础上),给裁决: 结论+置信度+最关键验证实验。问题: "+
			question+"\n\n全部作答:\n"+sb.String(), "", "")
	run.Say("orchestrator(verdict)", out, run.ExtractArtifacts(out))
	fmt.Printf("\nrun dir: %s | tokens %v\n", run.Dir, run.Usage)
}

var jsonArrRe = regexp.MustCompile(`(?s)\[.*\]`)

func Task(desc string, roster []string) {
	run := core.NewRun("task", desc)
	StartControl(run)
	defer proc.StopAll(run)
	var names []string
	for _, n := range roster {
		if config.Agents[n] != nil {
			names = append(names, n)
		}
	}
	if len(names) == 0 {
		for n := range config.Agents {
			names = append(names, n)
		}
	}
	orch := "orchestrator"
	if config.Agents[orch] == nil {
		orch = names[0]
	}
	planRaw := turnOf(run, orch,
		fmt.Sprintf("把任务拆成可执行子任务,每项指定负责人(从 %v 选)。只输出 JSON: [{id,owner,title,depends_on}]。任务: %s",
			names, desc), "", "")
	run.Say("orchestrator(plan)", planRaw, nil)
	type step struct {
		ID, Owner, Title string
		DependsOn        []string `json:"depends_on"`
	}
	var plan []step
	raw := jsonArrRe.FindString(planRaw)
	if raw != "" {
		var sj []struct {
			ID        string   `json:"id"`
			Owner     string   `json:"owner"`
			Title     string   `json:"title"`
			DependsOn []string `json:"depends_on"`
		}
		if json.Unmarshal([]byte(raw), &sj) == nil {
			for _, x := range sj {
				plan = append(plan, step{x.ID, x.Owner, x.Title, x.DependsOn})
			}
		}
	}
	if len(plan) == 0 {
		plan = []step{{"t1", names[0], desc, nil}}
	}
	done := map[string]string{}
	for _, st := range plan {
		owner := st.Owner
		if config.Agents[owner] == nil {
			owner = names[0]
		}
		var dep []string
		for _, k := range st.DependsOn {
			if d, ok := done[k]; ok {
				dep = append(dep, fmt.Sprintf("[%s] %s", k, core.Trunc(d, 1500)))
			}
		}
		prompt := "子任务: " + st.Title + "\n"
		if len(dep) > 0 {
			prompt += "前序产出:\n" + strings.Join(dep, "\n") + "\n"
		}
		prompt += "完成它。产出用 ```file:path 代码块或 write_file 工具写文件。"
		done[st.ID] = turnOf(run, owner, prompt, "", "")
		core.BusPub(owner, "task.done", map[string]any{"id": st.ID})
	}
	if config.Agents["critic"] != nil {
		var sb strings.Builder
		for k, v := range done {
			sb.WriteString(fmt.Sprintf("### %s\n%s\n\n", k, core.Trunc(v, 2000)))
		}
		turnOf(run, "critic", "验收产出,列问题/缺口/下一步:\n"+sb.String(), "", "")
	}
	fmt.Printf("\nrun dir: %s | tokens %v\n", run.Dir, run.Usage)
}
