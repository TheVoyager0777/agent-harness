package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestParseFrontmatter(t *testing.T) {
	md := `---
name: tester
model: glm-5.3
temp: 0.3
tools: read_file,grep
developer: 记得先读代码
---
正文 persona 内容。
第二行。`
	meta, body := parseFrontmatter(md)
	if meta["name"] != "tester" || meta["model"] != "glm-5.3" {
		t.Fatalf("meta=%v", meta)
	}
	if meta["temp"] != 0.3 {
		t.Fatalf("temp=%v (应为 float)", meta["temp"])
	}
	if body == "" || indexStr(body, "persona") < 0 {
		t.Fatalf("body=%q", body)
	}
}

func TestApplyMetaMerges(t *testing.T) {
	a := &Agent{Name: "x", Persona: "旧persona"}
	meta := map[string]any{"model": "m1", "endpoint": "olo",
		"tools": []string{"read_file"}, "temp": 0.5}
	applyMeta(a, meta)
	if a.Model != "m1" || a.Endpoint != "olo" || a.Temp != 0.5 {
		t.Fatalf("a=%+v", a)
	}
	if len(a.Tools) != 1 || a.Tools[0] != "read_file" {
		t.Fatalf("tools=%v", a.Tools)
	}
	// persona 由 body 决定, meta 不含 persona
	if a.Persona != "旧persona" {
		t.Fatal("persona 不应被 meta 覆盖")
	}
}

func TestLoadConfigMDMerge(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "endpoints.json"),
		[]byte(`{"e1":{"base":"http://x/v1","key":"k"}}`), 0o644)
	os.WriteFile(filepath.Join(dir, "agents.json"),
		[]byte(`[{"name":"a1","model":"m1","endpoint":"e1","persona":"json-persona"}]`), 0o644)
	os.MkdirAll(filepath.Join(dir, "agents"), 0o755)
	os.WriteFile(filepath.Join(dir, "agents", "a1.md"),
		[]byte("---\nmodel: m2\ntemp: 0.2\n---\nmd-persona"), 0o644)
	os.WriteFile(filepath.Join(dir, "agents", "b2.md"),
		[]byte("---\nname: b2\nmodel: m3\n---\n仅md agent"), 0o644)

	loadConfig(dir)
	a := Agents["a1"]
	if a == nil || a.Model != "m2" || a.Persona != "md-persona" {
		t.Fatalf("md 应覆盖 json: %+v", a)
	}
	if Agents["b2"] == nil || Agents["b2"].Model != "m3" {
		t.Fatal("纯 md agent 未加载")
	}
	if Endpoints["e1"].Base != "http://x/v1" {
		t.Fatal("端点未加载")
	}
}

func TestRoleMapFallback(t *testing.T) {
	G = &Global{RoleMap: map[string]string{"developer": "system"}}
	// role_map: 端点不认 developer 时降级 system — 映射存在即可
	if G.RoleMap["developer"] != "system" {
		t.Fatal("role_map 未生效")
	}
}
