package index

// 代码库索引(借鉴 codebase-memory-mcp 思路):
//   - 全量/增量索引: 文件清单 + 语言识别 + 符号/分块抽取
//   - 检索: 路径命中 + 词项计分
//   - 工具: code_search(查片段) / code_index(状态|重建)
//
// 索引落盘 <Root>/.harness-index.json, 查询时按 mtime 懒刷新。

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"fmt"
	"sync"
	"time"

	"github.com/xjcdw0777/agent-harness/internal/config"
)

type FileInfo struct {
	Path  string `json:"path"`
	Lang  string `json:"lang"`
	Size  int64  `json:"size"`
	Mtime int64  `json:"mtime"`
}

type Chunk struct {
	Path string `json:"path"`
	Line int    `json:"line"`
	Name string `json:"name"`
	Text string `json:"text"`
	Lang string `json:"lang"`
}

type Index struct {
	Files  map[string]*FileInfo `json:"files"`
	Chunks []Chunk              `json:"chunks"`
	Built  int64                `json:"built"`
	Roots  []string             `json:"roots"`
}

var (
	idx     *Index
	idxMu   sync.RWMutex
	idxFile string
)

const maxFileSize = 1 << 20

var langOf = map[string]string{
	".go": "go", ".py": "python", ".js": "js", ".ts": "ts",
	".mjs": "js", ".jsx": "jsx", ".tsx": "tsx", ".java": "java",
	".rs": "rust", ".c": "c", ".h": "c", ".cpp": "cpp",
	".md": "markdown", ".json": "json", ".yaml": "yaml",
	".yml": "yaml", ".toml": "toml", ".sh": "shell", ".ps1": "shell",
	".sql": "sql", ".html": "html", ".css": "css",
}

var skipDirs = map[string]bool{
	".git": true, "node_modules": true, "vendor": true,
	"__pycache__": true, ".idea": true, "dist": true,
	"build": true, ".next": true, "target": true, "runs": true,
	"archive": true, "bin": true, "obj": true,
}

// 符号边界正则(按语言切 chunk)
var symRes = map[string]*regexp.Regexp{
	"go":     regexp.MustCompile(`^(func |type \w+ |var \w+ |const )`),
	"python": regexp.MustCompile(`^(def |class |async def )`),
	"js":     regexp.MustCompile(`^(export |function |class |const \w+ *=|async function )`),
	"ts":     regexp.MustCompile(`^(export |function |class |const \w+ *=|interface |type |async function )`),
	"rust":   regexp.MustCompile(`^(fn |struct |impl |trait |pub fn |enum )`),
	"java":   regexp.MustCompile(`^\s*(public |private |protected |class |interface )`),
	"markdown": regexp.MustCompile(`^#{1,3} `),
}

func indexPath() string {
	if idxFile == "" {
		idxFile = filepath.Join(config.Root, ".harness-index.json")
	}
	return idxFile
}

// Roots: 索引根 — harness.json index.paths, 缺省 tool_roots。
func Roots() []string {
	if len(config.G.IndexPaths) > 0 {
		return config.G.IndexPaths
	}
	return config.G.ToolRoots
}

// Ensure: 保证索引可用(首次建 + 超 5min 或文件数变动则重建)。
func Ensure() *Index {
	idxMu.Lock()
	defer idxMu.Unlock()
	if idx != nil {
		// 懒刷新: >120s 做一次 mtime 扫, 有变更重建
		if time.Since(lastCheck) > 120*time.Second {
			lastCheck = time.Now()
			go refreshLocked()
		}
		return idx
	}
	if !load() {
		buildLocked()
	}
	return idx
}

var lastCheck time.Time

func load() bool {
	b, err := os.ReadFile(indexPath())
	if err != nil {
		return false
	}
	var i Index
	if json.Unmarshal(b, &i) != nil {
		return false
	}
	idx = &i
	return true
}

func save() {
	b, _ := json.Marshal(idx)
	os.WriteFile(indexPath(), b, 0o644)
}

// Refresh: 外部强制重建。
func Refresh() string {
	idxMu.Lock()
	defer idxMu.Unlock()
	buildLocked()
	save()
	return summaryLocked()
}

func refreshLocked() {
	// mtime 抽查重建(简单策略: 有任一文件变更即全量重建)
	changed := false
	for _, root := range idx.Roots {
		filepath.Walk(root, func(p string, fi os.FileInfo, err error) error {
			if err != nil || fi.IsDir() {
				return nil
			}
			old := idx.Files[p]
			if old == nil || old.Mtime != fi.ModTime().Unix() {
				changed = true
				return filepath.SkipAll
			}
			return nil
		})
		if changed {
			break
		}
	}
	if changed {
		idxMu.Lock()
		buildLocked()
		save()
		idxMu.Unlock()
	}
}

func buildLocked() {
	i := &Index{Files: map[string]*FileInfo{},
		Built: time.Now().Unix(), Roots: Roots()}
	for _, root := range i.Roots {
		filepath.Walk(root, func(p string, fi os.FileInfo, err error) error {
			if err != nil {
				return nil
			}
			if fi.IsDir() {
				if skipDirs[fi.Name()] {
					return filepath.SkipDir
				}
				return nil
			}
			if fi.Size() > maxFileSize || fi.Size() == 0 {
				return nil
			}
			ext := strings.ToLower(filepath.Ext(p))
			lang, ok := langOf[ext]
			if !ok {
				return nil
			}
			i.Files[p] = &FileInfo{Path: p, Lang: lang,
				Size: fi.Size(), Mtime: fi.ModTime().Unix()}
			i.Chunks = append(i.Chunks, chunkFile(p, lang)...)
			return nil
		})
	}
	idx = i
}

var chunkMaxLines = 60

func chunkFile(path, lang string) []Chunk {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	re := symRes[lang]
	var chunks []Chunk
	var cur []string
	name := ""
	start := 1
	ln := 0
	flush := func() {
		if len(cur) > 0 {
			chunks = append(chunks, Chunk{Path: path, Line: start,
				Name: name, Text: strings.Join(cur, "\n"), Lang: lang})
		}
		cur, name = nil, ""
	}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		ln++
		line := sc.Text()
		if re != nil && re.MatchString(strings.TrimLeft(line, " \t")) && len(cur) > 0 {
			flush()
			start = ln
			name = firstSymbol(line)
		}
		cur = append(cur, line)
		if name == "" && re != nil && re.MatchString(strings.TrimLeft(line, " \t")) {
			name = firstSymbol(line)
		}
		if len(cur) >= chunkMaxLines {
			flush()
			start = ln + 1
		}
	}
	flush()
	return chunks
}

func firstSymbol(line string) string {
	line = strings.TrimSpace(line)
	fs := strings.Fields(line)
	if len(fs) >= 2 {
		return strings.TrimRight(fs[1], "({:")
	}
	if len(fs) == 1 {
		return fs[0]
	}
	return ""
}

// Search: 词项计分检索 — 路径命中 + chunk 词频。
func Search(query string, limit int) []Chunk {
	i := Ensure()
	idxMu.RLock()
	defer idxMu.RUnlock()
	terms := tokenize(query)
	if limit <= 0 {
		limit = 10
	}
	type scored struct {
		c     Chunk
		score int
	}
	var hits []scored
	for _, c := range i.Chunks {
		score := 0
		low := strings.ToLower(c.Text)
		lowPath := strings.ToLower(c.Path)
		lowName := strings.ToLower(c.Name)
		for _, t := range terms {
			score += strings.Count(low, t)
			if strings.Contains(lowPath, t) {
				score += 3
			}
			if strings.Contains(lowName, t) {
				score += 5
			}
		}
		if score > 0 {
			hits = append(hits, scored{c, score})
		}
	}
	sort.Slice(hits, func(a, b int) bool {
		if hits[a].score != hits[b].score {
			return hits[a].score > hits[b].score
		}
		return hits[a].c.Path < hits[b].c.Path
	})
	out := make([]Chunk, 0, limit)
	for _, h := range hits {
		out = append(out, h.c)
		if len(out) >= limit {
			break
		}
	}
	return out
}

func tokenize(q string) []string {
	var out []string
	for _, f := range strings.Fields(strings.ToLower(q)) {
		f = strings.Trim(f, "\"'`.,;:!?()[]{}")
		if len(f) >= 2 {
			out = append(out, f)
		}
	}
	return out
}

// Summary: 索引概览(注入 context.get / status)。
func Summary() string {
	Ensure()
	idxMu.RLock()
	defer idxMu.RUnlock()
	return summaryLocked()
}

func summaryLocked() string {
	if idx == nil {
		return "index: not built"
	}
	langs := map[string]int{}
	for _, f := range idx.Files {
		langs[f.Lang]++
	}
	var parts []string
	for l, n := range langs {
		parts = append(parts, l+":"+itoa(n))
	}
	return "files:" + itoa(len(idx.Files)) + " chunks:" +
		itoa(len(idx.Chunks)) + " [" + strings.Join(parts, " ") + "]"
}

func itoa(n int) string { return fmt.Sprintf("%d", n) }
