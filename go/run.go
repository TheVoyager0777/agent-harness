package main

// Run 生命周期 + 事件总线 + 统一状态机。

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

func randHex(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// ---------- Bus ----------

type BusEvent struct {
	T     string         `json:"t"`
	From  string         `json:"from"`
	Event string         `json:"event"`
	Data  map[string]any `json:"data"`
}

type busSub struct {
	who, pattern, note string
}

var busMu sync.Mutex
var busSubs []busSub
var busHist []BusEvent
var busCond = sync.NewCond(&busMu)
var busRun *Run

var matchCache = map[string]*regexp.Regexp{}

func fnmatch(pat, s string) bool {
	// fnmatch: * ? [set]; 对事件名够用
	rx, ok := matchCache[pat]
	if !ok {
		var b strings.Builder
		b.WriteString("^")
		for _, c := range pat {
			switch c {
			case '*':
				b.WriteString(".*")
			case '?':
				b.WriteString(".")
			case '[', ']', '.', '+', '(', ')', '|', '^', '$', 92: // 92=backslash
				b.WriteByte(92)
				b.WriteByte(byte(c))
			default:
				b.WriteByte(byte(c))
			}
		}
		b.WriteString("$")
		r, err := regexp.Compile(b.String())
		if err != nil {
			return pat == s
		}
		rx = r
		matchCache[pat] = r
	}
	return rx.MatchString(s)
}

func BusPub(who, event string, data map[string]any) {
	ev := BusEvent{T: time.Now().Format("15:04:05"), From: who, Event: event, Data: data}
	busMu.Lock()
	busHist = append(busHist, ev)
	if len(busHist) > 500 {
		busHist = busHist[len(busHist)-500:]
	}
	busCond.Broadcast()
	subs := append([]busSub{}, busSubs...)
	busMu.Unlock()
	if busRun != nil {
		busRun.Event(map[string]any{"kind": "bus", "t": ev.T, "from": who,
			"event": event, "data": data})
	}
	for _, s := range subs {
		if fnmatch(s.pattern, event) {
			if p := Procs[s.who]; p != nil {
				d := map[string]any{"matched": s.pattern, "note": s.note,
					"event": event, "from": who, "data": data}
				p.Push(map[string]any{"type": "push", "event": "bus", "data": d})
			}
		}
	}
}

func BusSub(who, pattern, note string) {
	busMu.Lock()
	busSubs = append(busSubs, busSub{who, pattern, note})
	busMu.Unlock()
}

func BusWait(who, pattern string, timeout time.Duration) *BusEvent {
	deadline := time.Now().Add(timeout)
	busMu.Lock()
	defer busMu.Unlock()
	seen := len(busHist)
	for time.Now().Before(deadline) {
		for _, ev := range busHist[seen:] {
			if fnmatch(pattern, ev.Event) {
				return &ev
			}
		}
		seen = len(busHist)
		// cond 无超时等待 → 用定时 Broadcast 兜底
		go func() { time.Sleep(200 * time.Millisecond); busCond.Broadcast() }()
		busCond.Wait()
	}
	return nil
}

// ---------- 状态机 ----------

var StateList = map[string]bool{
	"boot": true, "idle": true, "thinking": true, "tooling": true,
	"waiting": true, "speaking": true, "done": true, "error": true, "stopped": true,
}

type AgentState struct {
	State  string `json:"state"`
	Detail string `json:"detail"`
	T      string `json:"t"`
}

type StateMachine struct {
	mu     sync.Mutex
	run    *Run
	States map[string]AgentState
}

func NewStateMachine(run *Run) *StateMachine {
	return &StateMachine{run: run, States: map[string]AgentState{}}
}

func (sm *StateMachine) Set(agent, state, detail string) {
	if !StateList[state] {
		state = "idle"
	}
	sm.mu.Lock()
	sm.States[agent] = AgentState{state, detail, time.Now().Format("15:04:05")}
	snap := sm.States
	sm.mu.Unlock()
	b, _ := json.MarshalIndent(snap, "", " ")
	os.WriteFile(filepath.Join(sm.run.Dir, "state.json"), b, 0o644)
	sm.run.Event(map[string]any{"kind": "state", "agent": agent,
		"state": state, "detail": detail})
	BusPub("sm", "agent.state", map[string]any{"agent": agent, "state": state})
}

func (sm *StateMachine) Snap() map[string]AgentState {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	out := map[string]AgentState{}
	for k, v := range sm.States {
		out[k] = v
	}
	return out
}

// ---------- Run ----------

type Run struct {
	Dir     string
	Mode    string
	Ep      string // 本 run 的端点覆盖
	Usage   map[string]int
	Calls   map[string]int
	SM      *StateMachine
	evMu    sync.Mutex
	evFile  *os.File
	trMu    sync.Mutex
	transcr []string
}

func NewRun(mode, topic string) *Run {
	dir := filepath.Join(Root, "runs",
		time.Now().Format("20060102-150405")+"-"+mode+"-"+randHex(2))
	os.MkdirAll(filepath.Join(dir, "artifacts"), 0o755)
	ef, _ := os.OpenFile(filepath.Join(dir, "events.jsonl"),
		os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	r := &Run{Dir: dir, Mode: mode, evFile: ef,
		Usage: map[string]int{"prompt_tokens": 0, "completion_tokens": 0},
		Calls: map[string]int{},
		transcr: []string{"# " + mode + ": " + topic,
			"started " + time.Now().Format("2006-01-02 15:04:05"), ""}}
	r.SM = NewStateMachine(r)
	r.Ep = JobEp
	busRun = r
	return r
}

func (r *Run) Say(who, text string, wrote []string) {
	r.trMu.Lock()
	defer r.trMu.Unlock()
	r.transcr = append(r.transcr, fmt.Sprintf("## %s\n%s\n", who, text))
	if len(wrote) > 0 {
		r.transcr = append(r.transcr,
			fmt.Sprintf("*(artifacts: %s)*\n", strings.Join(wrote, ", ")))
	}
	r.flushLocked()
}

func (r *Run) flushLocked() {
	os.WriteFile(filepath.Join(r.Dir, "transcript.md"),
		[]byte(strings.Join(r.transcr, "\n")), 0o644)
}

func (r *Run) Event(kv map[string]any) {
	r.evMu.Lock()
	defer r.evMu.Unlock()
	b, _ := json.Marshal(kv)
	r.evFile.Write(append(b, '\n'))
}

var fileBlockRe = regexp.MustCompile("(?s)```file:([^\n]+)\n(.*?)```")

func (r *Run) ExtractArtifacts(text string) []string {
	var wrote []string
	for _, m := range fileBlockRe.FindAllStringSubmatch(text, -1) {
		rel := strings.TrimPrefix(strings.TrimSpace(m[1]), "/")
		path := filepath.Join(r.Dir, "artifacts", rel)
		os.MkdirAll(filepath.Dir(path), 0o755)
		os.WriteFile(path, []byte(m[2]), 0o644)
		wrote = append(wrote, rel)
		BusPub("main", "artifact.written", map[string]any{"path": rel})
	}
	return wrote
}
