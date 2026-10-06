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
// 全局: --home DIR --endpoint E --no-tools --no-procs --reminder s

import (
	"encoding/json"
	"flag"
	"fmt"
	"net"
	"os"
	"strings"
	"time"
)

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
		loadConfig(resolveHome(home))
		agentMain(name)
		return
	}
	flag.Parse()
	args := flag.Args()
	if len(args) == 0 {
		fmt.Println("用法见文件头注释 | 命令: ask chat brainstorm council task serve submit serve-status serve-stop status inject export import agents tools check")
		return
	}
	loadConfig(resolveHome(*homeFlag))
	cmd := args[0]
	rest := args[1:]
	// 全局开关
	EpOverride = optStr(rest, "--endpoint")
	if optBool(rest, "--no-tools") {
		NoTools = true
	}
	if optBool(rest, "--no-procs") {
		UseProcs = false
	}
	PrefillFlag = optStr(rest, "--prefill")
	if r := optStr(rest, "--reminder"); r != "" {
		RemindOverride = &r
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
		ask(pos[0], optStr(rest, "--agent"), optStr(rest, "--prefill"))
	case "chat":
		chat(optStr(rest, "--agent"))
	case "brainstorm":
		brainstorm(pos[0], optInt(rest, "--rounds", 3), rosterOf(rest))
	case "council":
		council(pos[0], rosterOf(rest))
	case "task":
		task(pos[0], rosterOf(rest))
	case "serve":
		serve()
	case "submit":
		submit(pos, rest)
	case "serve-status":
		fmt.Println(jstr(serveCall(map[string]any{"op": "status"})))
	case "serve-stop":
		fmt.Println(jstr(serveCall(map[string]any{"op": "stop"})))
	case "status":
		out := ctlCall(get(0), map[string]any{"op": "status"})
		fmt.Println(jstr(out))
	case "inject":
		out := ctlCall(get(2), map[string]any{"op": "inject",
			"agent": get(0), "msg": get(1)})
		fmt.Println(jstr(out))
	case "export":
		exportRun(get(0), optStr(rest, "-o"))
	case "import":
		importRun(get(0))
	case "agents":
		for n, a := range Agents {
			perm, _ := json.Marshal(a.Permissions)
			fmt.Printf("%-16s %-28s @%-6s tools=%v perm=%s\n",
				n, a.Model, a.Endpoint, a.Tools, perm)
		}
	case "tools":
		for n, t := range Tools {
			d := ""
			if f, ok := t.Schema["function"].(map[string]any); ok {
				d, _ = f["description"].(string)
			}
			fmt.Printf("%-12s %s\n", n, trunc(d, 60))
		}
	case "check":
		checkAll()
	default:
		fmt.Println("未知命令 " + cmd)
	}
}

// ---------- 参数小工具 ----------

func positional(args []string) []string {
	var out []string
	for i := 0; i < len(args); i++ {
		if strings.HasPrefix(args[i], "-") {
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

func serveCall(obj map[string]any) map[string]any {
	port := G.ServePort
	if port <= 0 {
		port = 8390
	}
	c, err := net.DialTimeout("tcp4", fmt.Sprintf("127.0.0.1:%d", port),
		3*time.Second)
	if err != nil {
		return map[string]any{"error": "daemon 未运行 (harness serve 启动)"}
	}
	defer c.Close()
	json.NewEncoder(c).Encode(obj)
	var res map[string]any
	json.NewDecoder(c).Decode(&res)
	return res
}

func submit(pos []string, rest []string) {
	mode, topic := pos[0], ""
	if len(pos) > 1 {
		topic = pos[1]
	}
	res := serveCall(map[string]any{
		"op": "submit", "mode": mode, "topic": topic,
		"rounds": optInt(rest, "--rounds", 3), "agents": optStr(rest, "--agents"),
		"endpoint": optStr(rest, "--endpoint")})
	fmt.Println(jstr(res))
}

func checkAll() {
	for n, a := range Agents {
		t0 := time.Now()
		_, _, err := callModel(a, n, []Message{
			{Role: "user", Content: "回复 OK 两个字符即可"}}, nil, nil, nil)
		st := "OK"
		if err != nil {
			st = "FAIL: " + trunc(err.Error(), 60)
		}
		fmt.Printf("%-16s %-28s %vms  %s\n", n, a.Model,
			time.Since(t0).Milliseconds(), st)
	}
}
