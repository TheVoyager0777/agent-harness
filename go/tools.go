package main

// 工具系统: 内置工具 + tools/*.tool.json 声明式插件(子进程协议)。
// 权限: agent.permissions {tools,read,write,exec,collab}; 工具分 read/write/exec 类。
// 插件工具归 "exec" 类(能跑命令)。

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

type ToolFn func(args map[string]any) (string, error)

type ToolDef struct {
	Fn     ToolFn
	Schema map[string]any
}

var Tools = map[string]*ToolDef{}

var toolClass = map[string]string{
	"read_file": "read", "list_dir": "read", "grep": "read",
	"write_file": "write", "run_cmd": "exec",
}

func init() {
	schema := func(name, desc string, props map[string]any, req []string) map[string]any {
		return map[string]any{"type": "function", "function": map[string]any{
			"name": name, "description": desc,
			"parameters": map[string]any{"type": "object",
				"properties": props, "required": req}}}
	}
	p := func(kv ...string) map[string]any {
		m := map[string]any{}
		for i := 0; i+1 < len(kv); i += 2 {
			m[kv[i]] = map[string]string{"type": kv[i+1]}
		}
		return m
	}
	Tools["read_file"] = &ToolDef{tReadFile, schema("read_file",
		"读文件(限 tool_roots)", p("path", "string", "offset", "integer", "limit", "integer"),
		[]string{"path"})}
	Tools["list_dir"] = &ToolDef{tListDir, schema("list_dir",
		"列目录(限 tool_roots)", p("path", "string"), nil)}
	Tools["grep"] = &ToolDef{tGrep, schema("grep",
		"正则搜索文件内容(限 tool_roots)",
		p("pattern", "string", "path", "string", "glob", "string",
			"context", "integer", "max_results", "integer"), []string{"pattern"})}
	Tools["run_cmd"] = &ToolDef{tRunCmd, schema("run_cmd",
		"执行 shell 命令(cwd=配置根,≤300s)",
		p("cmd", "string", "timeout", "integer"), []string{"cmd"})}
	Tools["write_file"] = &ToolDef{tWriteFile, schema("write_file",
		"写文件(限 write_roots)",
		p("path", "string", "content", "string"), []string{"path", "content"})}
	Tools["wait_event"] = &ToolDef{tWaitEvent, schema("wait_event",
		"阻塞等待总线事件(fnmatch pattern),用于等前置 agent 汇报/产物",
		p("pattern", "string", "timeout", "number"), []string{"pattern"})}
	toolClass["wait_event"] = "collab"
	Tools["send_msg"] = &ToolDef{tSendMsg, schema("send_msg",
		"向另一 agent 收件箱注入消息(协作)",
		p("agent", "string", "text", "string"), []string{"agent", "text"})}
	toolClass["send_msg"] = "collab"
}

// ---------- 沙箱 ----------

func normPath(p string) string {
	r, _ := filepath.Abs(p)
	return strings.ToLower(filepath.Clean(r))
}

func under(path string, roots []string) bool {
	rp := normPath(path)
	for _, r := range roots {
		rn := normPath(r)
		if rp == rn || strings.HasPrefix(rp, rn+string(os.PathSeparator)) {
			return true
		}
	}
	return false
}

func resolvePath(path string, roots []string) (string, error) {
	if !filepath.IsAbs(path) {
		path = filepath.Join(Root, path)
	}
	abs, _ := filepath.Abs(path)
	if !under(abs, roots) {
		return "", fmt.Errorf("path outside allowed roots: %s", abs)
	}
	return abs, nil
}

// ---------- 内置工具实现 ----------

func argS(args map[string]any, k string) string {
	if v, ok := args[k]; ok {
		if s, ok := v.(string); ok {
			return s
		}
		b, _ := json.Marshal(v)
		return string(b)
	}
	return ""
}
func argI(args map[string]any, k string, def int) int {
	if v, ok := args[k]; ok {
		switch n := v.(type) {
		case float64:
			return int(n)
		case string:
			i, _ := strconv.Atoi(n)
			return i
		}
	}
	return def
}

func tReadFile(args map[string]any) (string, error) {
	p, err := resolvePath(argS(args, "path"), G.ToolRoots)
	if err != nil {
		return "", err
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return "", err
	}
	data := string(b)
	off := argI(args, "offset", 0)
	if off > 0 && off < len(data) {
		data = data[off:]
	}
	lim := argI(args, "limit", 20000)
	if lim > 0 && len(data) > lim {
		data = data[:lim]
	}
	if data == "" {
		return "(empty)", nil
	}
	return data, nil
}

func tListDir(args map[string]any) (string, error) {
	p, err := resolvePath(argS(args, "path"), G.ToolRoots)
	if err != nil {
		return "", err
	}
	es, err := os.ReadDir(p)
	if err != nil {
		return "", err
	}
	var out []string
	for i, e := range es {
		if i >= 500 {
			break
		}
		pre := "f "
		if e.IsDir() {
			pre = "d "
		}
		out = append(out, pre+e.Name())
	}
	sort.Strings(out)
	return strings.Join(out, "\n"), nil
}

func tGrep(args map[string]any) (string, error) {
	base, err := resolvePath(argS(args, "path"), G.ToolRoots)
	if err != nil {
		return "", err
	}
	pat := argS(args, "pattern")
	rx, err := regexp.Compile(pat)
	if err != nil {
		return "", err
	}
	max := argI(args, "max_results", 40)
	ctx := argI(args, "context", 0)
	glob := argS(args, "glob")
	var hits []string
	filepath.Walk(base, func(fp string, fi os.FileInfo, err error) error {
		if err != nil || fi.IsDir() || len(hits) >= max {
			return nil
		}
		if glob != "" {
			ok, _ := filepath.Match(glob, fi.Name())
			if !ok {
				return nil
			}
		}
		f, err := os.Open(fp)
		if err != nil {
			return nil
		}
		defer f.Close()
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 1<<20), 1<<20)
		var lines []string
		for sc.Scan() {
			lines = append(lines, sc.Text())
		}
		for i, ln := range lines {
			if !rx.MatchString(ln) {
				continue
			}
			lo, hi := i-ctx, i+ctx+1
			if lo < 0 {
				lo = 0
			}
			if hi > len(lines) {
				hi = len(lines)
			}
			for j := lo; j < hi; j++ {
				mark := "-"
				if j == i {
					mark = ">"
				}
				hits = append(hits, fmt.Sprintf("%s:%d%s%s", fp, j+1, mark, lines[j]))
			}
			if len(hits) >= max {
				break
			}
		}
		return nil
	})
	if len(hits) == 0 {
		return "(no match)", nil
	}
	return strings.Join(hits, "\n"), nil
}

func tRunCmd(args map[string]any) (string, error) {
	to := argI(args, "timeout", 60)
	if to > 300 {
		to = 300
	}
	cmd := exec.Command("cmd", "/c", argS(args, "cmd"))
	if isUnix() {
		cmd = exec.Command("sh", "-c", argS(args, "cmd"))
	}
	cmd.Dir = Root
	done := make(chan struct{})
	var outb, errb strings.Builder
	cmd.Stdout, cmd.Stderr = &outb, &errb
	if err := cmd.Start(); err != nil {
		return "", err
	}
	go func() { cmd.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Duration(to) * time.Second):
		cmd.Process.Kill()
		return "rc=-1 (timeout)\n" + tail(outb.String(), 9000), nil
	}
	rc := cmd.ProcessState.ExitCode()
	return fmt.Sprintf("rc=%d\n%s\n%s", rc, tail(outb.String(), 9000), tail(errb.String(), 3000)), nil
}

func tWriteFile(args map[string]any) (string, error) {
	p, err := resolvePath(argS(args, "path"), G.WriteRoots)
	if err != nil {
		return "", err
	}
	os.MkdirAll(filepath.Dir(p), 0o755)
	c := argS(args, "content")
	if err := os.WriteFile(p, []byte(c), 0o644); err != nil {
		return "", err
	}
	BusPub("main", "file.written", map[string]any{"path": p})
	return fmt.Sprintf("wrote %s (%dB)", p, len(c)), nil
}

// 协作类工具(需 collab 权限): 等事件/发消息
func tWaitEvent(args map[string]any) (string, error) {
	to := float64(argI(args, "timeout", 60))
	ev := BusWait("agent", argS(args, "pattern"), time.Duration(to*float64(time.Second)))
	if ev == nil {
		return "(timeout: no matching event)", nil
	}
	b, _ := json.Marshal(ev)
	return string(b), nil
}

func tSendMsg(args map[string]any) (string, error) {
	dst := argS(args, "agent")
	p := Procs[dst]
	if p == nil {
		return "", fmt.Errorf("agent not running: %s", dst)
	}
	p.Inject("[来自协作 agent 的消息] " + argS(args, "text"))
	return "sent", nil
}

func tail(s string, n int) string {
	if len(s) > n {
		return s[len(s)-n:]
	}
	return s
}

func isUnix() bool { return os.PathSeparator == '/' }

// ---------- 插件: tools/*.tool.json ----------
// {"name","description","parameters":{...},"cmd":"prog"} args JSON 走 stdin,stdout=结果。

type toolPlugin struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Parameters  map[string]any `json:"parameters"`
	Cmd         string         `json:"cmd"`
}

func loadToolPlugins(root string) {
	tdir := filepath.Join(root, "tools")
	es, err := os.ReadDir(tdir)
	if err != nil {
		return
	}
	for _, e := range es {
		if !strings.HasSuffix(e.Name(), ".tool.json") {
			continue
		}
		var pl toolPlugin
		if readJSON(filepath.Join(tdir, e.Name()), &pl) != nil || pl.Name == "" {
			continue
		}
		name := pl.Name
		if _, dup := Tools[name]; dup {
			name = strings.TrimSuffix(e.Name(), ".tool.json") + "." + name
		}
		cmdStr := pl.Cmd
		Tools[name] = &ToolDef{
			Fn: func(args map[string]any) (string, error) {
				b, _ := json.Marshal(args)
				c := exec.Command("cmd", "/c", cmdStr)
				if isUnix() {
					c = exec.Command("sh", "-c", cmdStr)
				}
				c.Dir = Root
				c.Stdin = strings.NewReader(string(b))
				var out strings.Builder
				c.Stdout, c.Stderr = &out, &out
				c.Run()
				return out.String(), nil
			},
			Schema: map[string]any{"type": "function", "function": map[string]any{
				"name": name, "description": pl.Description,
				"parameters": pl.Parameters}},
		}
		toolClass[name] = "exec"
	}
}

// ---------- 权限裁决 ----------

func effectiveTools(a *Agent) []map[string]any {
	if !a.Permissions.get("tools", true) || NoTools {
		return nil
	}
	var out []map[string]any
	for _, tn := range a.Tools {
		t, ok := Tools[tn]
		if !ok {
			continue
		}
		cls := toolClass[tn]
		if cls == "" {
			cls = "read"
		}
		if a.Permissions.get(cls, true) {
			out = append(out, t.Schema)
		}
	}
	return out
}

func checkToolPerm(a *Agent, tname string) string {
	if !a.Permissions.get("tools", true) {
		return "tools disabled for this agent"
	}
	ok := false
	for _, t := range a.Tools {
		if t == tname {
			ok = true
		}
	}
	if !ok {
		return "tool not in agent allowlist: " + tname
	}
	cls := toolClass[tname]
	if cls == "" {
		cls = "read"
	}
	if !a.Permissions.get(cls, true) {
		return "permission denied: " + cls + " class"
	}
	return ""
}

func RunTool(a *Agent, name, argstr string) string {
	if deny := checkToolPerm(a, name); deny != "" {
		return "tool denied: " + deny
	}
	t, ok := Tools[name]
	if !ok {
		return "tool error: unknown tool " + name
	}
	var args map[string]any
	if argstr != "" {
		if json.Unmarshal([]byte(argstr), &args) != nil {
			args = map[string]any{}
		}
	}
	res, err := t.Fn(args)
	if err != nil {
		res = "tool error: " + err.Error()
	}
	mx := G.ToolOutputMax
	if mx <= 0 {
		mx = 12000
	}
	if len(res) > mx {
		res = res[:mx] + fmt.Sprintf("\n...[truncated %dB]", len(res)-mx)
	}
	return res
}
