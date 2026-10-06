package tools

// 工具调用执行器: 解析→校验→执行 三段管道。
// 并行协调: 每调用解析出资源锁键, 按键分组——组间并行、组内按提交序执行;
// 共享类(RLock)与独占类(Lock)在同键上互斥, 跨 agent 同样生效(执行器在主进程单例)。

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/xjcdw0777/agent-harness/internal/config"
)

// Call: 一次工具调用请求(子进程批次元素)。
type Call struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Args string `json:"args"` // JSON 字符串
}

// Result: 一次执行结果, ID 回关联。
type Result struct {
	ID      string `json:"id"`
	Content string `json:"result"`
}

type lockKey struct {
	key  string // 资源键: 同键互斥协调
	excl bool   // 独占(写/执行) vs 共享(读/检索/协作)
}

// Executor: 全局执行器, 进程内单例——所有 agent 子进程的工具调用汇到此协调。
var Ex = NewExecutor()

func NewExecutor() *Executor {
	return &Executor{locks: map[string]*sync.RWMutex{},
		holds: map[string]string{}, release: map[string]chan struct{}{}}
}

type Executor struct {
	mu      sync.Mutex
	locks   map[string]*sync.RWMutex
	holds   map[string]string        // 资源键→持有 agent (跨批次持锁)
	release map[string]chan struct{} // 持有键的释放通知
}

func (e *Executor) slot(k string) *sync.RWMutex {
	e.mu.Lock()
	defer e.mu.Unlock()
	l := e.locks[k]
	if l == nil {
		l = &sync.RWMutex{}
		e.locks[k] = l
	}
	return l
}

// pathKey: 归一化路径做资源键(解析失败也返回确定键, 错误留给执行层暴露)。
func pathKey(raw string) string {
	if raw == "" {
		raw = config.Root
	}
	if !filepath.IsAbs(raw) {
		raw = filepath.Join(config.Root, raw)
	}
	return "file:" + normPath(raw)
}

// lockKeyOf: 调用→资源键。只读共享; 写/命令执行独占; 未知工具按类别兜底。
func lockKeyOf(name, argstr string) lockKey {
	var args map[string]any
	if argstr != "" {
		json.Unmarshal([]byte(argstr), &args)
	}
	pth := argS(args, "path")
	switch name {
	case "write_file":
		return lockKey{pathKey(pth), true}
	case "read_file", "list_dir", "grep":
		return lockKey{pathKey(pth), false}
	case "run_cmd":
		return lockKey{"proc:exec", true}
	case "wait_event", "send_msg":
		return lockKey{"collab", false}
	case "ctx_search":
		return lockKey{"ctx:" + argS(args, "agent"), false}
	case "code_search", "file_overview":
		return lockKey{"index", false}
	}
	switch toolClass[name] {
	case "exec", "write":
		return lockKey{"tool:" + name, true}
	default:
		return lockKey{"tool:" + name, false}
	}
}

// ExecBatch: 解析→校验→加锁执行。结果与 calls 同序。
func (e *Executor) ExecBatch(a *config.Agent, calls []Call) []Result {
	res := make([]Result, len(calls))
	for i, c := range calls {
		res[i].ID = c.ID
	}
	// 校验: 权限/注册, 拒绝的立即出结果不占锁
	admit := make([]int, 0, len(calls))
	for i, c := range calls {
		if deny := CheckPerm(a, c.Name); deny != "" {
			res[i].Content = "tool denied: " + deny
			continue
		}
		if _, ok := Registry[c.Name]; !ok {
			res[i].Content = "tool error: unknown tool " + c.Name
			continue
		}
		admit = append(admit, i)
	}
	// 按资源键分组(保序); 组间并行
	type grp struct {
		key  string
		idxs []int
	}
	var groups []*grp
	seen := map[string]*grp{}
	keys := make([]lockKey, len(calls))
	for _, i := range admit {
		keys[i] = lockKeyOf(calls[i].Name, calls[i].Args)
		g := seen[keys[i].key]
		if g == nil {
			g = &grp{key: keys[i].key}
			seen[keys[i].key] = g
			groups = append(groups, g)
		}
		g.idxs = append(g.idxs, i)
	}
	var wg sync.WaitGroup
	for _, g := range groups {
		wg.Add(1)
		go func(g *grp) {
			defer wg.Done()
			l := e.slot(g.key)
			for _, i := range g.idxs {
				c := calls[i]
				// 持锁检查: 他人持锁→等释放; 自持→跳过槽锁(防自死锁)
				ok, self := e.waitHolds(a.Name, g.key, 120*time.Second)
				if !ok {
					res[i].Content = "tool error: lock wait timeout on " + g.key
					continue
				}
				if self {
					res[i].Content = execParsed(a, c.Name, c.Args)
					continue
				}
				ex := keys[i].excl
				if ex {
					l.Lock()
				} else {
					l.RLock()
				}
				res[i].Content = execParsed(a, c.Name, c.Args)
				if ex {
					l.Unlock()
				} else {
					l.RUnlock()
				}
			}
		}(g)
	}
	wg.Wait()
	return res
}

// ---------- 长程持锁 ----------
// agent 一次请求内跨多轮工具调用独占某资源:
// lock_acquire{path|key:"exec"|"*"} 持锁 → 后续各轮调用跳过排队,
// 其他 agent 触同键阻塞等释放; lock_release / turn 结束 / 进程退出时释放。

// normHoldKey: 用户给的资源名→执行器键。
func normHoldKey(k string) string {
	switch k {
	case "*", "global":
		return "*"
	case "exec", "proc:exec":
		return "proc:exec"
	case "collab", "index":
		return k
	}
	if strings.HasPrefix(k, "file:") || strings.HasPrefix(k, "tool:") ||
		strings.HasPrefix(k, "ctx:") {
		return k
	}
	return pathKey(k) // 默认按文件路径
}

// Acquire: 独占持有资源键。同 owner 重入幂等; 超时返回 false。
func (e *Executor) Acquire(owner, rawKey string, timeout time.Duration) bool {
	key := normHoldKey(rawKey)
	l := e.slot(key)
	deadline := time.Now().Add(timeout)
	for {
		e.mu.Lock()
		if e.holds[key] == owner {
			e.mu.Unlock()
			return true
		}
		e.mu.Unlock()
		if l.TryLock() {
			e.mu.Lock()
			e.holds[key] = owner
			e.release[key] = make(chan struct{})
			e.mu.Unlock()
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// Release: 释放持有键(仅 owner 可放)。返回是否真释放。
func (e *Executor) Release(owner, rawKey string) bool {
	key := normHoldKey(rawKey)
	e.mu.Lock()
	if e.holds[key] != owner {
		e.mu.Unlock()
		return false
	}
	delete(e.holds, key)
	close(e.release[key])
	delete(e.release, key)
	l := e.locks[key] // slot() 会再锁 e.mu——在 mu 内直接取
	e.mu.Unlock()
	l.Unlock()
	return true
}

// ReleaseAll: turn 结束/进程退出时清掉该 agent 全部持锁。
func (e *Executor) ReleaseAll(owner string) {
	var keys []string
	e.mu.Lock()
	for k, o := range e.holds {
		if o == owner {
			keys = append(keys, k)
		}
	}
	e.mu.Unlock()
	for _, k := range keys {
		e.Release(owner, k)
	}
}

// Holds: 当前持锁快照(status/diagnostics)。
func (e *Executor) Holds() map[string]string {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := map[string]string{}
	for k, o := range e.holds {
		out[k] = o
	}
	return out
}

// waitHolds: 调用前检查。返回 (ok, self):
//
//	ok=false   等待超时, 调用应失败
//	self=true  本 agent 持有该键(或全局)*——跳过槽锁直接执行, 否则自死锁
func (e *Executor) waitHolds(owner, key string, timeout time.Duration) (bool, bool) {
	deadline := time.Now().Add(timeout)
	for {
		e.mu.Lock()
		holder, held := e.holds[key]
		global, gHeld := e.holds["*"]
		if held && holder == owner {
			e.mu.Unlock()
			return true, true
		}
		if !held && (!gHeld || global == owner) {
			e.mu.Unlock()
			return true, false
		}
		if gHeld && global == owner && !held {
			e.mu.Unlock()
			return true, false // 自持全局锁,本键无锁→正常走槽锁
		}
		var ch chan struct{}
		if held && holder != owner {
			ch = e.release[key]
		} else {
			ch = e.release["*"]
		}
		e.mu.Unlock()
		select {
		case <-ch:
		case <-time.After(time.Until(deadline)):
			return false, false
		}
		if time.Now().After(deadline) {
			return false, false
		}
	}
}

// execParsed: 校验已过, 解析参数并执行+截断。
func execParsed(a *config.Agent, name, argstr string) string {
	t := Registry[name]
	var args map[string]any
	if argstr != "" && json.Unmarshal([]byte(argstr), &args) != nil {
		args = map[string]any{}
	}
	res, err := t.Fn(a, args)
	if err != nil {
		res = "tool error: " + err.Error()
	}
	mx := config.G.ToolOutputMax
	if mx <= 0 {
		mx = 12000
	}
	if len(res) > mx {
		res = res[:mx] + fmt.Sprintf("\n...[truncated %dB]", len(res)-mx)
	}
	return res
}
