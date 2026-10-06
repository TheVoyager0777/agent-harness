package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// ---------- 配置类型 ----------

type Endpoint struct {
	Base string `json:"base"`
	Key  string `json:"key"`
}

type Permissions struct {
	Tools  *bool `json:"tools"`
	Read   *bool `json:"read"`
	Write  *bool `json:"write"`
	Exec   *bool `json:"exec"`
	Collab *bool `json:"collab"`
}

func (p Permissions) Get(k string, def bool) bool {
	var v *bool
	switch k {
	case "tools":
		v = p.Tools
	case "read":
		v = p.Read
	case "write":
		v = p.Write
	case "exec":
		v = p.Exec
	case "collab":
		v = p.Collab
	}
	if v == nil {
		return def
	}
	return *v
}

type Watch struct {
	On   string `json:"on"`
	Note string `json:"note"`
}

type Agent struct {
	Name        string            `json:"name"`
	Model       string            `json:"model"`
	Endpoint    string            `json:"endpoint"`
	Persona     string            `json:"persona"`
	Temp        float64           `json:"temp"`
	Tools       []string          `json:"tools"`
	Permissions Permissions       `json:"permissions"`
	Watch       []Watch           `json:"watch"`
	Developer   []string          `json:"developer"`
	Extra       map[string]string `json:"-"`
}

type Reminder struct {
	Text  string `json:"text"`
	Every int    `json:"every"`
}

type Global struct {
	TimeoutS      int               `json:"timeout_s"`
	TurnTimeoutS  int               `json:"turn_timeout_s"`
	MaxToolRounds int               `json:"max_tool_rounds"`
	ToolOutputMax int               `json:"tool_output_max"`
	Stream        *bool             `json:"stream"`
	ToolRoots     []string          `json:"tool_roots"`
	WriteRoots    []string          `json:"write_roots"`
	Reminder      Reminder          `json:"reminder"`
	Sessions      string            `json:"sessions"`
	RoleMap       map[string]string `json:"role_map"`
	ServePort     int               `json:"serve_port"`
	IndexPaths    []string          `json:"index_paths"`
	Context       ContextConf       `json:"context"`
}

// ContextConf: 上下文治理参数(ctx 包读)。
type ContextConf struct {
	MaxEst     int64 `json:"max_est"`     // 压缩触发阈值, 0=关
	KeepRecent int   `json:"keep_recent"` // 尾部保留原文消息数
	ChunkEst   int64 `json:"chunk_est"`   // 摘要单块输入上限
}

func (g *Global) StreamEnabled() bool {
	if g.Stream == nil {
		return true
	}
	return *g.Stream
}

// ---------- 消息 ----------

type Message struct {
	Role       string     `json:"role"`
	Content    string     `json:"content"`
	Name       string     `json:"name,omitempty"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
}

type ToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

// ---------- agents/*.md frontmatter ----------

var fmRe = regexp.MustCompile(`(?s)^---\s*\n(.*?)\n---\s*\n(.*)`)
var kvRe = regexp.MustCompile(`^(\w[\w-]*):\s*(.*)$`)
var liRe = regexp.MustCompile(`^\s+-\s+`)

func ParseFrontmatter(text string) (map[string]any, string) {
	m := fmRe.FindStringSubmatch(text)
	if m == nil {
		return map[string]any{}, text
	}
	meta := map[string]any{}
	lines := strings.Split(m[1], "\n")
	for i := 0; i < len(lines); i++ {
		kv := kvRe.FindStringSubmatch(lines[i])
		if kv == nil {
			continue
		}
		k, v := kv[1], strings.TrimSpace(kv[2])
		if strings.HasPrefix(v, "[") && strings.HasSuffix(v, "]") {
			var list []string
			for _, x := range strings.Split(v[1:len(v)-1], ",") {
				x = strings.Trim(strings.TrimSpace(x), `"'`)
				if x != "" {
					list = append(list, x)
				}
			}
			meta[k] = list
		} else if v == "" {
			var items []string
			for i+1 < len(lines) && liRe.MatchString(lines[i+1]) {
				i++
				items = append(items, liRe.ReplaceAllString(lines[i], ""))
			}
			if items != nil {
				meta[k] = items
			} else {
				meta[k] = ""
			}
		} else {
			v = strings.Trim(v, `"'`)
			if f, err := strconv.ParseFloat(v, 64); err == nil {
				meta[k] = f
			} else {
				meta[k] = v
			}
		}
	}
	return meta, m[2]
}

// ---------- 配置加载 ----------

var Root string
var Endpoints map[string]Endpoint
var Agents map[string]*Agent
var G *Global

func ResolveHome(cli string) string {
	try := []string{cli, os.Getenv("HARNESS_HOME"),
		filepath.Join(mustGetwd(), ".harness"), scriptDir()}
	for _, c := range try {
		if c == "" {
			continue
		}
		if _, err := os.Stat(filepath.Join(c, "endpoints.json")); err == nil {
			abs, _ := filepath.Abs(c)
			return abs
		}
	}
	return scriptDir()
}

func mustGetwd() string { d, _ := os.Getwd(); return d }

func scriptDir() string {
	exe, err := os.Executable()
	if err == nil {
		return filepath.Dir(exe)
	}
	return mustGetwd()
}

func LoadConfig(root string) {
	Root = root
	Endpoints = map[string]Endpoint{}
	ReadJSON(filepath.Join(root, "endpoints.json"), &Endpoints)

	Agents = map[string]*Agent{}
	var list []*Agent
	if ReadJSON(filepath.Join(root, "agents.json"), &list) == nil {
		for _, a := range list {
			Agents[a.Name] = a
		}
	}
	adir := filepath.Join(root, "agents")
	if es, err := os.ReadDir(adir); err == nil {
		for _, e := range es {
			if !strings.HasSuffix(e.Name(), ".md") {
				continue
			}
			b, _ := os.ReadFile(filepath.Join(adir, e.Name()))
			meta, body := ParseFrontmatter(string(b))
			name := strings.TrimSuffix(e.Name(), ".md")
			if v, ok := meta["name"].(string); ok && v != "" {
				name = v
			}
			a := Agents[name]
			if a == nil {
				a = &Agent{}
			}
			a.Name = name
			ApplyMeta(a, meta)
			if strings.TrimSpace(body) != "" {
				a.Persona = strings.TrimSpace(body)
			}
			Agents[name] = a
		}
	}

	G = &Global{TimeoutS: 300, TurnTimeoutS: 1800, MaxToolRounds: 12,
		ToolOutputMax: 12000, Sessions: "run", ServePort: 8390,
		RoleMap: map[string]string{"developer": "system"}}
	ReadJSON(filepath.Join(root, "harness.json"), G)
	for i, p := range G.ToolRoots {
		if !filepath.IsAbs(p) {
			p = filepath.Join(root, p)
		}
		G.ToolRoots[i], _ = filepath.Abs(p)
	}
	for i, p := range G.WriteRoots {
		if !filepath.IsAbs(p) {
			p = filepath.Join(root, p)
		}
		G.WriteRoots[i], _ = filepath.Abs(p)
	}
	if len(G.ToolRoots) == 0 {
		G.ToolRoots = []string{root}
	}
	if len(G.WriteRoots) == 0 {
		G.WriteRoots = []string{root}
	}
}

func ApplyMeta(a *Agent, meta map[string]any) {
	if v, ok := meta["model"].(string); ok {
		a.Model = v
	}
	if v, ok := meta["endpoint"].(string); ok {
		a.Endpoint = v
	}
	if v, ok := meta["temp"].(float64); ok {
		a.Temp = v
	}
	if v, ok := meta["tools"].([]string); ok {
		a.Tools = v
	}
	if v, ok := meta["developer"].([]string); ok {
		a.Developer = v
	} else if v, ok := meta["developer"].(string); ok {
		a.Developer = []string{v}
	}
	if pm, ok := meta["permissions"].(map[string]any); ok {
		for k, v := range pm {
			if b, ok := v.(bool); ok {
				switch k {
				case "tools":
					a.Permissions.Tools = &b
				case "read":
					a.Permissions.Read = &b
				case "write":
					a.Permissions.Write = &b
				case "exec":
					a.Permissions.Exec = &b
				case "collab":
					a.Permissions.Collab = &b
				}
			}
		}
	}
}

func ReadJSON(path string, v any) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}
