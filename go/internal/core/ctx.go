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
	s := a.Persona
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
