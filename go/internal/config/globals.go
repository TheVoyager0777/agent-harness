package config

import "os"

// CLI 级全局开关
var (
	EpOverride  string // --endpoint 强制端点
	NoTools     bool   // --no-tools
	PrefillFlag string // --prefill
)

var RemindOverride *string // --reminder
var ResumeOverride *bool   // --resume

// ResumeEnabled: context.resume 或 --resume(经 HARNESS_RESUME 穿透子进程) 启用跨 run 会话复用。
func ResumeEnabled() bool {
	if ResumeOverride != nil {
		return *ResumeOverride
	}
	if os.Getenv("HARNESS_RESUME") == "1" {
		return true
	}
	return G != nil && G.Context.Resume
}
