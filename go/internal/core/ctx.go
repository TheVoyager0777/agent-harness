package core

import (
	"github.com/xjcdw0777/agent-harness/internal/config"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

var ctxCache string
var ctxAt time.Time

// IndexOverview: 索引概览钩子(由 proc 装配, 必须非阻塞)。
var IndexOverview func() string

func hasTool(a *config.Agent, name string) bool {
	for _, t := range a.Tools {
		if t == name {
			return true
		}
	}
	return false
}

// envBriefing: 工作环境简报 — 路径边界/索引导航/批量调用/长程锁纪律。
func envBriefing(a *config.Agent) string {
	var b strings.Builder
	b.WriteString("\n\n# 工作环境\n")
	if len(config.G.ToolRoots) > 0 {
		b.WriteString("可读目录: " + strings.Join(config.G.ToolRoots, " | ") + "\n")
	}
	if len(config.G.WriteRoots) > 0 {
		b.WriteString("可写目录: " + strings.Join(config.G.WriteRoots, " | ") + "\n")
	}
	if hasTool(a, "code_search") {
		ov := ""
		if IndexOverview != nil {
			ov = IndexOverview()
		}
		if ov == "" {
			ov = "构建中,稍后可用"
		}
		b.WriteString("代码索引: " + ov + "。定位代码优先用 code_search(按符号/关键词拿 path:line+片段),再 read_file 精读;code_index 可查状态/重建。不要盲目 list_dir/grep 扫目录。\n")
	}
	b.WriteString("工具编排: 相互独立的调用在同一响应中一次发出多个 tool_calls(框架按资源键并行调度);有依赖的调用分轮等结果。每轮尽量合并能并行的调用,不要逐个单发。\n")
	if hasTool(a, "lock_acquire") {
		b.WriteString("长程锁: 需对同一资源做多步操作(如改文件→验证→再改)时先 lock_acquire 拿资源键,完成后 lock_release;持有期间他人触同键会排队,不会自锁。\n")
	}
	if hasTool(a, "ctx_search") {
		b.WriteString("记忆: 早期历史可能被压缩;用 ctx_search 检索档案、ctx_read 回读原文区间。\n")
	}
	if hasTool(a, "send_msg") || hasTool(a, "wait_event") {
		b.WriteString("协作: send_msg 给其他 agent 投递消息;wait_event 阻塞等待总线事件(如等待前置任务汇报)。\n")
	}
	return b.String()
}

func SharedContext() string {
	if ctxCache != "" && time.Since(ctxAt) < 60*time.Second {
		return ctxCache
	}
	var parts []string
	cdir := filepath.Join(config.Root, "workspace", "context")
	if es, err := os.ReadDir(cdir); err == nil {
		names := []string{}
		for _, e := range es {
			if strings.HasSuffix(e.Name(), ".md") {
				names = append(names, e.Name())
			}
		}
		sort.Strings(names)
		for _, fn := range names {
			b, _ := os.ReadFile(filepath.Join(cdir, fn))
			parts = append(parts, "### context/"+fn+"\n"+strings.TrimSpace(string(b)))
		}
	}
	ctxCache, ctxAt = strings.Join(parts, "\n\n"), time.Now()
	return ctxCache
}

func SysContent(a *config.Agent, extra string) string {
	s := a.Persona + envBriefing(a)
	if ctx := SharedContext(); ctx != "" {
		s += "\n\n# 项目上下文\n" + ctx
	}
	if extra != "" {
		s += "\n\n" + extra
	}
	return s
}

func ReminderText() string {
	if config.RemindOverride != nil {
		return *config.RemindOverride
	}
	return config.G.Reminder.Text
}
