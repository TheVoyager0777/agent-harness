package ctx

// pins: 高权重注入块的识别与保全(借鉴 ctxsvc/pins.go)。
// system-reminder / user-memory / memory / MEMORY.md 与系统提示词同级,
// 只做去重+保留最新, 永不压缩。

import (
	"regexp"
	"sort"
	"strings"
)

var pinTags = []string{"system-reminder", "user-memory", "memory",
	"local-command-stdout"}

var pinMemoryRe = regexp.MustCompile(`(?s)Contents of [^\n]{0,200}MEMORY\.md[^\n]*\n.*?(?:\n\n|$)`)

type pinSpan struct{ start, end int }

func pinSpans(text string) []pinSpan {
	var spans []pinSpan
	for _, tag := range pinTags {
		open, closeTag := "<"+tag+">", "</"+tag+">"
		pos := 0
		for {
			i := strings.Index(text[pos:], open)
			if i < 0 {
				break
			}
			start := pos + i
			afterOpen := start + len(open)
			j := strings.Index(text[afterOpen:], closeTag)
			end := len(text)
			if j >= 0 {
				end = afterOpen + j + len(closeTag)
			}
			spans = append(spans, pinSpan{start, end})
			pos = end
			if j < 0 {
				break
			}
		}
	}
	for _, loc := range pinMemoryRe.FindAllStringIndex(text, -1) {
		spans = append(spans, pinSpan{loc[0], loc[1]})
	}
	sort.Slice(spans, func(i, j int) bool { return spans[i].start < spans[j].start })
	out := spans[:0]
	last := -1
	for _, s := range spans {
		if s.start < last {
			continue
		}
		out = append(out, s)
		last = s.end
	}
	return out
}

var pinDigitsRe = regexp.MustCompile(`\d+`)

// pinKeyOf: 首行去数字指纹(时间戳/序号每轮都变) + 截 120 字。
func pinKeyOf(text string) string {
	first := ""
	for _, l := range strings.Split(text, "\n") {
		if strings.TrimSpace(l) != "" {
			first = strings.TrimSpace(l)
			break
		}
	}
	if first == "" {
		first = text
	}
	key := pinDigitsRe.ReplaceAllString(first, "#")
	if len(key) > 120 {
		key = key[:120]
	}
	return key
}

type pinSet struct {
	order []string
	m     map[string]string
}

func (s *pinSet) add(text string) {
	k := pinKeyOf(text)
	if s.m == nil {
		s.m = map[string]string{}
	}
	if _, dup := s.m[k]; !dup {
		s.order = append(s.order, k)
	}
	s.m[k] = text // 同 key 后者胜=最新全集
}

func (s *pinSet) list() []string {
	out := make([]string, 0, len(s.order))
	for _, k := range s.order {
		out = append(out, s.m[k])
	}
	return out
}

// extractPins: 把消息文本里的 pin 块剥出, 返回剩余消息+pins。
func extractPins(msgs []msgView) ([]msgView, *pinSet) {
	ps := &pinSet{}
	var out []msgView
	for _, m := range msgs {
		spans := pinSpans(m.content)
		if len(spans) == 0 {
			out = append(out, m)
			continue
		}
		var b strings.Builder
		pos := 0
		for _, sp := range spans {
			b.WriteString(m.content[pos:sp.start])
			ps.add(m.content[sp.start:sp.end])
			pos = sp.end
		}
		b.WriteString(m.content[pos:])
		m.content = b.String()
		if strings.TrimSpace(m.content) != "" || m.role != "" {
			out = append(out, m)
		}
	}
	return out, ps
}
