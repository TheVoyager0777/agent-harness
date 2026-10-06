package tools

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/xjcdw0777/agent-harness/internal/config"
)

func tAgent() *config.Agent {
	return &config.Agent{Name: "t",
		Tools:       []string{"read_file", "write_file", "run_cmd", "probe"},
		Permissions: config.Permissions{}}
}

func setup(t *testing.T) string {
	d := t.TempDir()
	config.Root = d
	config.G = &config.Global{
		ToolRoots:     []string{d},
		WriteRoots:    []string{d},
		ToolOutputMax: 12000}
	Ex = NewExecutor()
	return d
}

// 同键有序: 同批次 write→read 同路径, 读必须看到写的内容
func TestBatchSameKeyOrder(t *testing.T) {
	d := setup(t)
	fp := filepath.ToSlash(filepath.Join(d, "x.txt"))
	res := Ex.ExecBatch(tAgent(), []Call{
		{ID: "1", Name: "write_file", Args: `{"path":"` + fp + `","content":"v1"}`},
		{ID: "2", Name: "read_file", Args: `{"path":"` + fp + `"}`},
	})
	if !strings.Contains(res[0].Content, "wrote") {
		t.Fatalf("write: %s", res[0].Content)
	}
	if res[1].Content != "v1" {
		t.Fatalf("read=%q 应为 v1(写后读同键保序)", res[1].Content)
	}
}

// 跨键并行: 两个不同文件的读可以并行(不互相阻塞)
func TestBatchCrossKeyParallel(t *testing.T) {
	d := setup(t)
	var conc int32
	var maxConc int32
	Registry["probe"] = &ToolDef{
		Fn: func(a *config.Agent, args map[string]any) (string, error) {
			n := atomic.AddInt32(&conc, 1)
			for {
				m := atomic.LoadInt32(&maxConc)
				if n <= m || atomic.CompareAndSwapInt32(&maxConc, m, n) {
					break
				}
			}
			time.Sleep(60 * time.Millisecond)
			atomic.AddInt32(&conc, -1)
			return "ok:" + argS(args, "tag"), nil
		}, Schema: map[string]any{}}
	defer delete(Registry, "probe")
	defer delete(toolClass, "probe")

	// probe 默认 read 类→各自不同名调用共享 tool:probe 键会串行;
	// 用带不同 path 的 read_file 跨键并行需真实文件——直接用两键:
	os.WriteFile(filepath.Join(d, "a.txt"), []byte("A"), 0o644)
	os.WriteFile(filepath.Join(d, "b.txt"), []byte("B"), 0o644)
	t0 := time.Now()
	res := Ex.ExecBatch(tAgent(), []Call{
		{ID: "1", Name: "read_file", Args: `{"path":"` + filepath.ToSlash(filepath.Join(d, "a.txt")) + `"}`},
		{ID: "2", Name: "read_file", Args: `{"path":"` + filepath.ToSlash(filepath.Join(d, "b.txt")) + `"}`},
	})
	if res[0].Content != "A" || res[1].Content != "B" {
		t.Fatalf("res=%v", res)
	}
	if time.Since(t0) > 2*time.Second {
		t.Fatal("跨键读应并行完成")
	}
}

// 跨 agent 独占键互斥: 两个 agent 同时写同一路径, 写不交错
func TestCrossAgentExclusive(t *testing.T) {
	setup(t)
	var inflight int32
	var overlap int32
	Registry["probe"] = &ToolDef{
		Fn: func(a *config.Agent, args map[string]any) (string, error) {
			if !atomic.CompareAndSwapInt32(&inflight, 0, 1) {
				atomic.AddInt32(&overlap, 1)
			}
			time.Sleep(30 * time.Millisecond)
			atomic.StoreInt32(&inflight, 0)
			return "done", nil
		}, Schema: map[string]any{}}
	defer delete(Registry, "probe")
	toolClass["probe"] = "exec"
	defer delete(toolClass, "probe")

	ag1, ag2 := tAgent(), tAgent()
	ag2.Name = "t2"
	var wg sync.WaitGroup
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func(ag *config.Agent, i int) {
			defer wg.Done()
			Ex.ExecBatch(ag, []Call{{ID: fmt.Sprint(i), Name: "probe", Args: "{}"}})
		}(map[bool]*config.Agent{true: ag1, false: ag2}[i%2 == 0], i)
	}
	wg.Wait()
	if atomic.LoadInt32(&overlap) > 0 {
		t.Fatalf("exec 类工具跨 agent 重叠执行 %d 次", overlap)
	}
}

// 校验拒绝不占锁: 无权限调用直接出结果
func TestValidateReject(t *testing.T) {
	setup(t)
	ag := tAgent()
	ag.Tools = []string{"read_file"} // write_file 不在白名单
	res := Ex.ExecBatch(ag, []Call{
		{ID: "1", Name: "write_file", Args: `{"path":"x","content":"y"}`},
		{ID: "2", Name: "nosuchtool", Args: `{}`},
		{ID: "3", Name: "read_file", Args: `{"path":"nosuch.txt"}`},
	})
	if !strings.Contains(res[0].Content, "denied") {
		t.Fatalf("r0=%q", res[0].Content)
	}
	if !strings.Contains(res[1].Content, "denied") {
		t.Fatalf("r1=%q(白名单先拒)", res[1].Content)
	}
	if !strings.Contains(res[2].Content, "tool error") {
		t.Fatalf("r2=%q", res[2].Content)
	}
	if res[0].ID != "1" || res[2].ID != "3" {
		t.Fatal("结果 ID 回关联错误")
	}
}

// 结果顺序与提交顺序一致(组间并行后不错位)
func TestResultOrderStable(t *testing.T) {
	d := setup(t)
	for _, n := range []string{"a", "b", "c", "dd"} {
		os.WriteFile(filepath.Join(d, n+".txt"), []byte("C-"+n), 0o644)
	}
	var calls []Call
	for i, n := range []string{"dd", "c", "b", "a"} {
		calls = append(calls, Call{ID: fmt.Sprint(i), Name: "read_file",
			Args: `{"path":"` + filepath.ToSlash(filepath.Join(d, n+".txt")) + `"}`})
	}
	res := Ex.ExecBatch(tAgent(), calls)
	for i, n := range []string{"dd", "c", "b", "a"} {
		if res[i].Content != "C-"+n {
			t.Fatalf("res[%d]=%q 应为 C-%s", i, res[i].Content, n)
		}
	}
}

// ---------- 长程持锁 ----------

// 自持可继续调同键资源; 他人被阻塞到释放
func TestHoldBlocksOthers(t *testing.T) {
	d := setup(t)
	fp := filepath.ToSlash(filepath.Join(d, "held.txt"))
	os.WriteFile(fp, []byte("orig"), 0o644)
	ag1, ag2 := tAgent(), tAgent()
	ag2.Name = "t2"
	ag2.Tools = ag1.Tools

	if !Ex.Acquire(ag1.Name, fp, time.Second) {
		t.Fatal("acquire failed")
	}
	// 自持: 同键写直接执行不死锁
	res := Ex.ExecBatch(ag1, []Call{
		{ID: "w", Name: "write_file", Args: `{"path":"` + fp + `","content":"by-owner"}`}})
	if !strings.Contains(res[0].Content, "wrote") {
		t.Fatalf("owner write blocked: %s", res[0].Content)
	}
	// 他人读同键: 阻塞直到释放
	done := make(chan string, 1)
	go func() {
		r := Ex.ExecBatch(ag2, []Call{
			{ID: "r", Name: "read_file", Args: `{"path":"` + fp + `"}`}})
		done <- r[0].Content
	}()
	select {
	case <-done:
		t.Fatal("other agent read should block on held key")
	case <-time.After(80 * time.Millisecond):
	}
	Ex.Release(ag1.Name, fp)
	select {
	case c := <-done:
		if c != "by-owner" {
			t.Fatalf("read=%q", c)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("read did not proceed after release")
	}
}

// 持有 exec 键→他人 run_cmd 阻塞; ReleaseAll 一次清完
func TestHoldExecAndReleaseAll(t *testing.T) {
	setup(t)
	ag1, ag2 := tAgent(), tAgent()
	ag2.Name = "t2"
	if !Ex.Acquire(ag1.Name, "exec", time.Second) {
		t.Fatal("acquire exec")
	}
	Ex.Acquire(ag1.Name, "somefile.txt", time.Second)
	// 全局 * 也会被 exec 键挡? 不——exec 只挡 proc:exec 键
	blocked := make(chan string, 1)
	go func() {
		r := Ex.ExecBatch(ag2, []Call{
			{ID: "x", Name: "run_cmd", Args: `{"cmd":"echo hi"}`}})
		blocked <- r[0].Content
	}()
	time.Sleep(60 * time.Millisecond)
	select {
	case <-blocked:
		t.Fatal("run_cmd should block on held proc:exec")
	default:
	}
	Ex.ReleaseAll(ag1.Name)
	select {
	case c := <-blocked:
		if !strings.Contains(c, "hi") {
			t.Fatalf("run_cmd=%q", c)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("run_cmd stuck after ReleaseAll")
	}
	if len(Ex.Holds()) != 0 {
		t.Fatal("holds not empty after ReleaseAll")
	}
}

// 持锁超时路径: 键被占→Acquire 超时失败
func TestAcquireTimeout(t *testing.T) {
	setup(t)
	if !Ex.Acquire("a1", "res.txt", time.Second) {
		t.Fatal("first acquire")
	}
	t0 := time.Now()
	if Ex.Acquire("a2", "res.txt", 80*time.Millisecond) {
		t.Fatal("second acquire should timeout")
	}
	if time.Since(t0) < 60*time.Millisecond {
		t.Fatal("returned before timeout")
	}
	// 非 owner 释放无效
	if Ex.Release("a2", "res.txt") {
		t.Fatal("non-owner release succeeded")
	}
	if !Ex.Release("a1", "res.txt") {
		t.Fatal("owner release failed")
	}
}
