package main

// 通用多模型协作 harness (Go 版)
//
//	ask "q" [--agent N] [--prefill s]
//	chat [--agent N]
//	brainstorm "议题" [--rounds N] [--agents a,b]
//	council "问题" [--agents a,b]
//	task "描述" [--agents a,b]
//	serve                     常驻 daemon (serve_port)
//	submit MODE "topic" [...] 提交给 daemon
//	serve-status / serve-stop
//	status [rundir] | inject AGENT "msg" [rundir]
//	export [rundir] [-o zip] | import zip
//	agents | tools | check
//	_agent NAME               (子进程入口)
//
// 全局: --home DIR --endpoint E --no-tools --no-procs --reminder s --resume

import (
	"encoding/json"
	"flag"
	"fmt"
	"github.com/xjcdw0777/agent-harness/internal/config"
	"github.com/xjcdw0777/agent-harness/internal/core"
	"github.com/xjcdw0777/agent-harness/internal/index"
	"github.com/xjcdw0777/agent-harness/internal/model"
	"github.com/xjcdw0777/agent-harness/internal/modes"
	"github.com/xjcdw0777/agent-harness/internal/proc"
	"github.com/xjcdw0777/agent-harness/internal/serve"
	"github.com/xjcdw0777/agent-harness/internal/tools"
	"github.com/xjcdw0777/agent-harness/internal/xfer"
	"os"
	"strings"
	"time"
)

// Version: 构建期 -X main.Version=x.y.z 注入(Makefile ldflags)。
var Version = "dev"

var homeFlag = flag.String("home", "", "配置根(endpoints.json 所在目录)")

func main() {
	// _agent 必须在 flag 解析前分流(子进程参数不规整)
	if len(os.Args) > 2 && os.Args[1] == "_agent" {
		// _agent NAME --home DIR
		name := os.Args[2]
		home := ""
		for i, a := range os.Args {
			if a == "--home" && i+1 < len(os.Args) {
				home = os.Args[i+1]
			}
		}
		config.LoadConfig(config.ResolveHome(home))
		proc.AgentMain(name)
		return
	}
	flag.Parse()
	args := flag.Args()
	if len(args) == 0 {
		fmt.Println("用法见文件头注释 | 命令: ask chat brainstorm council task serve submit serve-status serve-stop status inject export import agents tools check")
		return
	}
	config.LoadConfig(config.ResolveHome(*homeFlag))
	cmd := args[0]
	rest := args[1:]
	// 全局开关
	config.EpOverride = optStr(rest, "--endpoint")
	if optBool(rest, "--no-tools") {
		config.NoTools = true
	}
	if optBool(rest, "--no-procs") {
		modes.UseProcs = false
	}
	config.PrefillFlag = optStr(rest, "--prefill")
	if r := optStr(rest, "--reminder"); r != "" {
		config.RemindOverride = &r
	}
	if optBool(rest, "--resume") {
		t := true
		config.ResumeOverride = &t
	}

	get := func(i int) string {
		if i < len(rest) {
			return rest[i]
		}
		return ""
	}
	pos := positional(rest) // 去掉 --k v 后的位置参数

	switch cmd {
	case "ask":
		modes.Ask(pos[0], optStr(rest, "--agent"), optStr(rest, "--prefill"))
	case "chat":
		modes.Chat(optStr(rest, "--agent"))
	case "brainstorm":
		modes.Brainstorm(pos[0], optInt(rest, "--rounds", 3), rosterOf(rest))
	case "council":
		modes.Council(pos[0], rosterOf(rest))
	case "task":
		modes.Task(pos[0], rosterOf(rest))
	case "serve":
		serve.Serve()
	case "submit":
		submit(pos, rest)
	case "serve-status":
		fmt.Println(jstr(serve.Call(map[string]any{"op": "status"})))
	case "serve-stop":
		fmt.Println(jstr(serve.Call(map[string]any{"op": "stop"})))
	case "status":
		out := serve.CtlCall(get(0), map[string]any{"op": "status"})
		fmt.Println(jstr(out))
	case "inject":
		out := serve.CtlCall(get(2), map[string]any{"op": "inject",
			"agent": get(0), "msg": get(1)})
		fmt.Println(jstr(out))
	case "export":
		xfer.Export(get(0), optStr(rest, "-o"))
	case "import":
		xfer.Import(get(0))
	case "agents":
		for n, a := range config.Agents {
			perm, _ := json.Marshal(a.Permissions)
			fmt.Printf("%-16s %-28s @%-6s tools=%v perm=%s\n",
				n, a.Model, a.Endpoint, a.Tools, perm)
		}
	case "tools":
		for n, t := range tools.Registry {
			d := ""
			if f, ok := t.Schema["function"].(map[string]any); ok {
				d, _ = f["description"].(string)
			}
			fmt.Printf("%-12s %s\n", n, core.Trunc(d, 60))
		}
	case "check":
		checkAll()
	case "index":
		if len(rest) > 1 && rest[1] == "refresh" {
			fmt.Println(index.Refresh())
		} else {
			fmt.Println(index.Summary())
		}
	case "version":
		fmt.Println("harness " + Version)
	default:
		fmt.Println("未知命令 " + cmd)
	}
}

// ---------- 参数小工具 ----------

// boolFlags: 不吞下一个参数的开关类 flag(其余 --k 视为带值)。
var boolFlags = map[string]bool{
	"--no-tools": true, "--no-procs": true, "--resume": true,
	"--version": true, "--help": true, "-h": true,
}

func positional(args []string) []string {
	var out []string
	for i := 0; i < len(args); i++ {
		if strings.HasPrefix(args[i], "-") {
			if boolFlags[args[i]] {
				continue // 开关: 不吃下一个参数
			}
			if i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
				i++
			}
			continue
		}
		out = append(out, args[i])
	}
	if len(out) == 0 {
		out = []string{""}
	}
	return out
}

func optStr(args []string, key string) string {
	for i, a := range args {
		if a == key && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

func optInt(args []string, key string, def int) int {
	v := optStr(args, key)
	if v == "" {
		return def
	}
	var n int
	fmt.Sscanf(v, "%d", &n)
	return n
}

func optBool(args []string, key string) bool {
	for _, a := range args {
		if a == key {
			return true
		}
	}
	return false
}

func rosterOf(args []string) []string {
	s := optStr(args, "--agents")
	if s == "" {
		return nil
	}
	return strings.Split(s, ",")
}

func jstr(v any) string {
	b, _ := json.MarshalIndent(v, "", "  ")
	return string(b)
}

// ---------- daemon 客户端 ----------

func submit(pos []string, rest []string) {
	mode, topic := pos[0], ""
	if len(pos) > 1 {
		topic = pos[1]
	}
	res := serve.Call(map[string]any{
		"op": "submit", "mode": mode, "topic": topic,
		"rounds": optInt(rest, "--rounds", 3), "agents": optStr(rest, "--agents"),
		"endpoint": optStr(rest, "--endpoint")})
	fmt.Println(jstr(res))
}

func checkAll() {
	for n, a := range config.Agents {
		t0 := time.Now()
		_, _, err := model.Call(a, n, []config.Message{
			{Role: "user", Content: "回复 OK 两个字符即可"}}, nil, nil, nil)
		st := "OK"
		if err != nil {
			st = "FAIL: " + core.Trunc(err.Error(), 60)
		}
		fmt.Printf("%-16s %-28s %vms  %s\n", n, a.Model,
			time.Since(t0).Milliseconds(), st)
	}
}
