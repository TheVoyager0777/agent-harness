package index

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/xjcdw0777/agent-harness/internal/config"
)

func setup(t *testing.T) {
	d := t.TempDir()
	config.Root = d
	config.G = &config.Global{ToolRoots: []string{d}}
	idxFile = ""
	idx = nil
	os.WriteFile(filepath.Join(d, "a.go"), []byte(`package x

func HandleRequest() { // 处理请求
	return
}

func parseInput() {}
`), 0o644)
	os.WriteFile(filepath.Join(d, "b.py"), []byte(`def mint_token():
    return 1

class Compactor:
    pass
`), 0o644)
}

func TestBuildSearch(t *testing.T) {
	setup(t)
	s := Summary()
	if !contains(s, "files:2") {
		t.Fatalf("summary=%s", s)
	}
	hits := Search("HandleRequest", 5)
	if len(hits) == 0 {
		t.Fatal("无命中")
	}
	if hits[0].Path != filepath.Join(config.Root, "a.go") {
		t.Fatalf("hit=%v", hits[0])
	}
	hits = Search("mint_token", 5)
	if len(hits) == 0 || hits[0].Lang != "python" {
		t.Fatalf("py 检索失败: %v", hits)
	}
}

func TestRefresh(t *testing.T) {
	setup(t)
	Ensure()
	// 新文件 → refresh 后可见
	os.WriteFile(filepath.Join(config.Root, "c.go"),
		[]byte("package x\n\nfunc NewThing() {}\n"), 0o644)
	Refresh()
	hits := Search("NewThing", 5)
	if len(hits) == 0 {
		t.Fatal("refresh 后应可检索新文件")
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
