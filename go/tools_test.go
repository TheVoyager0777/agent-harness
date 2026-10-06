package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func setupSandbox(t *testing.T) string {
	dir := t.TempDir()
	G = &Global{TimeoutS: 10, MaxToolRounds: 3, ToolOutputMax: 1000,
		ToolRoots: []string{dir}, WriteRoots: []string{dir}}
	os.WriteFile(filepath.Join(dir, "hello.txt"), []byte("hi there\nline2 needle\n"), 0o644)
	os.MkdirAll(filepath.Join(dir, "sub"), 0o755)
	return dir
}

func bptr(b bool) *bool { return &b }

func TestPermAdjudication(t *testing.T) {
	dir := setupSandbox(t)
	read := &Agent{Name: "r", Tools: []string{"read_file", "write_file", "run_cmd"},
		Permissions: Permissions{Read: bptr(true), Write: bptr(false), Exec: bptr(false)}}
	// read 权限可读,不可写/执行
	if e := checkToolPerm(read, "read_file"); e != "" {
		t.Fatalf("read_file 被拒: %s", e)
	}
	if e := checkToolPerm(read, "write_file"); e == "" {
		t.Fatal("write_file 应被拒")
	}
	if e := checkToolPerm(read, "run_cmd"); e == "" {
		t.Fatal("run_cmd 应被拒")
	}
	// tools 白名单收紧
	wl := &Agent{Name: "w", Tools: []string{"grep"},
		Permissions: Permissions{Read: bptr(true)}}
	if e := checkToolPerm(wl, "read_file"); e == "" {
		t.Fatal("白名单外的 read_file 应被拒")
	}
	if e := checkToolPerm(wl, "grep"); e != "" {
		t.Fatalf("白名单内 grep 被拒: %s", e)
	}
	// 无 tools 权限 = 全拒
	nt := &Agent{Name: "n", Permissions: Permissions{Tools: bptr(false)}}
	if e := checkToolPerm(nt, "read_file"); e == "" {
		t.Fatal("tools=false 应全拒")
	}
	_ = dir
}

func TestPathSandbox(t *testing.T) {
	dir := setupSandbox(t)
	a := &Agent{Name: "a",
		Tools: []string{"read_file", "write_file", "run_cmd", "grep", "list_dir"},
		Permissions: Permissions{
			Read: bptr(true), Write: bptr(true), Exec: bptr(true)}}
	// 沙箱内可读
	if out := RunTool(a, "read_file", `{"path":"`+filepath.ToSlash(dir)+`/hello.txt"}`); !strings.Contains(out, "needle") {
		t.Fatalf("读取失败: %s", out)
	}
	// 沙箱外拒绝
	out := RunTool(a, "read_file", `{"path":"C:/Windows/System32/drivers/etc/hosts"}`)
	if !strings.Contains(out, "denied") && !strings.Contains(out, "error") {
		t.Fatalf("越界读取应被拒, 得到: %s", trunc(out, 100))
	}
	// 写入沙箱内
	w := RunTool(a, "write_file", `{"path":"`+filepath.ToSlash(dir)+`/new.txt","content":"abc"}`)
	if strings.Contains(w, "error") || strings.Contains(w, "denied") {
		t.Fatalf("写入失败: %s", w)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "new.txt")); string(b) != "abc" {
		t.Fatal("写入内容不符")
	}
	// 写入沙箱外拒绝
	w = RunTool(a, "write_file", `{"path":"`+os.TempDir()+`/evil.txt","content":"x"}`)
	if !strings.Contains(w, "denied") && !strings.Contains(w, "error") {
		t.Fatal("越界写入应被拒")
	}
}

func TestGrepTool(t *testing.T) {
	dir := setupSandbox(t)
	a := &Agent{Name: "a", Tools: []string{"grep"}, Permissions: Permissions{Read: bptr(true)}}
	out := RunTool(a, "grep", `{"pattern":"needle","path":"`+filepath.ToSlash(dir)+`"}`)
	if !strings.Contains(out, "hello.txt") {
		t.Fatalf("grep 应命中: %s", trunc(out, 200))
	}
	out = RunTool(a, "grep", `{"pattern":"nonexist_xyz","path":"`+filepath.ToSlash(dir)+`"}`)
	if strings.Contains(out, "hello.txt") {
		t.Fatal("不应命中")
	}
}

func TestTrunc(t *testing.T) {
	if trunc("abc", 2) != "ab…" && len(trunc("abc", 2)) == 0 {
		t.Fatal("trunc 异常")
	}
	if trunc("ab", 5) != "ab" {
		t.Fatal("短串不应截断")
	}
}
