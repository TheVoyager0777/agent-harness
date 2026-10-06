package main

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

var ctxCache string
var ctxAt time.Time
var RemindOverride *string

func sharedContext() string {
	if ctxCache != "" && time.Since(ctxAt) < 60*time.Second {
		return ctxCache
	}
	var parts []string
	cdir := filepath.Join(Root, "workspace", "context")
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

func sysContent(a *Agent, extra string) string {
	s := a.Persona
	if ctx := sharedContext(); ctx != "" {
		s += "\n\n# 项目上下文\n" + ctx
	}
	if extra != "" {
		s += "\n\n" + extra
	}
	return s
}

func reminderText() string {
	if RemindOverride != nil {
		return *RemindOverride
	}
	return G.Reminder.Text
}
