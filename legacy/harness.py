#!/usr/bin/env python3
"""harness - 多模型 agent 集群协作工具 (OpenAI 兼容端点, 零三方依赖)。

用法:
  harness.py ask "问题" --agent NAME         单发(衍生 agent 进程)
  harness.py chat --agent NAME               交互 REPL (/use /agents /inject /quit)
  harness.py brainstorm "议题" --rounds N     圆桌 + orchestrator 收敛
  harness.py council "问题"                   并行作答 + 互评 + 裁决
  harness.py task "描述"                      拆任务 + 分工 + critic 验收
  harness.py status [RUNDIR]                 查运行中/最近 run 的 agent 状态
  harness.py inject RUNDIR AGENT "msg"       向运行中的 agent 动态注入消息
  harness.py export [RUNDIR] [-o out.zip]    导出 run(默认最新)
  harness.py import FILE.zip                 导入 run 到 runs/
  harness.py agents | tools | check          检视

架构: 主进程持端点连接池/工具沙箱/事件总线/状态机/控制套接字;
       每个 agent 是 `harness.py _agent NAME` 衍生子进程(stdin/stdout JSONL IPC),
       模型/工具/总线全部回主进程 IPC 调用, 流式 delta 主进程直接回显。

配置根 (--home DIR > HARNESS_HOME > ./.harness > 脚本目录):
  endpoints.json  {"name": {"base","key"}}
  agents.json     [{name,model,endpoint,persona,temp,tools,permissions,watch,developer}]
  agents/*.md     frontmatter + persona 正文(与 json 合并,同名覆盖)
  harness.json    超时/工具轮数/沙箱根/reminder/sessions/role_map
  tools/*.py      插件: 暴露 TOOLS = {name: {"fn","schema"}}
  workspace/context/*.md  共享上下文 → 注入每个 agent system
  runs/           transcript.md events.jsonl state.json artifacts/ control.port
"""
import json, os, re, sys, time, uuid, socket, subprocess, argparse, threading
import urllib.request, urllib.error, importlib.util, fnmatch, zipfile, queue
from concurrent.futures import ThreadPoolExecutor

_orig_gai = socket.getaddrinfo
def _ipv4_only(host, port, family=0, type=0, proto=0, flags=0):
    return [a for a in _orig_gai(host, port, family, type, proto, flags)
            if a[0] == socket.AF_INET]
socket.getaddrinfo = _ipv4_only

SCRIPT_DIR = os.path.dirname(os.path.abspath(__file__))


def resolve_home(cli_home=None):
    for cand in (cli_home, os.environ.get("HARNESS_HOME"),
                 os.path.join(os.getcwd(), ".harness"), SCRIPT_DIR):
        if cand and os.path.exists(os.path.join(cand, "endpoints.json")):
            return os.path.abspath(cand)
    return SCRIPT_DIR


def load_json(root, name, default=None):
    p = os.path.join(root, name)
    if not os.path.exists(p):
        if default is not None:
            return default
        raise FileNotFoundError(p)
    with open(p, encoding="utf-8") as f:
        return json.load(f)


def _parse_frontmatter(text):
    m = re.match(r"^---\s*\n(.*?)\n---\s*\n(.*)", text, re.S)
    if not m:
        return {}, text
    meta, body = {}, m.group(2)
    lines = m.group(1).split("\n")
    i = 0
    while i < len(lines):
        mm = re.match(r"^(\w[\w-]*):\s*(.*)$", lines[i])
        if mm:
            k, v = mm.group(1), mm.group(2).strip()
            if v.startswith("[") and v.endswith("]"):
                meta[k] = [x.strip().strip('"\'') for x in v[1:-1].split(",") if x.strip()]
            elif v == "":
                items = []
                while i + 1 < len(lines) and re.match(r"^\s+-\s+", lines[i + 1]):
                    i += 1
                    items.append(re.sub(r"^\s+-\s+", "", lines[i]).strip())
                meta[k] = items if items else ""
            else:
                v = v.strip('"\'')
                meta[k] = float(v) if re.match(r"^-?\d+\.?\d*$", v) else v
        i += 1
    return meta, body


def load_agents(root):
    agents = {}
    for a in load_json(root, "agents.json", []):
        agents[a["name"]] = a
    adir = os.path.join(root, "agents")
    if os.path.isdir(adir):
        for fn in sorted(os.listdir(adir)):
            if not fn.endswith(".md"):
                continue
            meta, body = _parse_frontmatter(
                open(os.path.join(adir, fn), encoding="utf-8").read())
            name = meta.get("name") or fn[:-3]
            ent = agents.get(name, {})
            ent.update(meta)
            ent["name"] = name
            if body.strip():
                ent["persona"] = body.strip()
            agents[name] = ent
    for a in agents.values():
        a.setdefault("permissions", {"read": True, "write": False,
                                     "exec": False, "collab": True})
    return agents


# ---------------- 工具(主进程侧执行 = 权限裁决点) ----------------

GLOBAL, ROOT = {}, SCRIPT_DIR
ENDPOINTS, AGENTS = {}, {}
EP_OVERRIDE = None
NO_TOOLS = False
REMIND_OVERRIDE = None

TOOL_CLASS = {"read_file": "read", "list_dir": "read", "grep": "read",
              "write_file": "write", "run_cmd": "exec"}


def _norm(p):
    return os.path.normcase(os.path.realpath(p))


def _under(path, roots):
    rp = _norm(path)
    return any(rp == _norm(r) or rp.startswith(_norm(r) + os.sep) for r in roots)


def _resolve(path, roots):
    if not os.path.isabs(path):
        path = os.path.join(ROOT, path)
    path = os.path.realpath(path)
    if not _under(path, roots):
        raise PermissionError("path outside allowed roots: " + path)
    return path


def t_read_file(path, offset=0, limit=20000, **_):
    p = _resolve(path, GLOBAL["tool_roots"])
    data = open(p, encoding="utf-8", errors="replace").read()
    return data[offset:][:limit if limit else None] or "(empty)"


def t_list_dir(path=".", **_):
    p = _resolve(path, GLOBAL["tool_roots"])
    return "\n".join(("d " if os.path.isdir(os.path.join(p, e)) else "f ") + e
                     for e in sorted(os.listdir(p))[:500])


def t_grep(pattern, path=".", glob=None, context=2, max_results=40, **_):
    base = _resolve(path, GLOBAL["tool_roots"])
    try:
        r = subprocess.run(["rg", "--no-heading", "-n", "--context=%d" % context,
                            "--max-count=%d" % max_results]
                           + (["--glob=" + glob] if glob else []) + [pattern, base],
                           capture_output=True, text=True, timeout=30)
        return r.stdout or r.stderr or "(no match)"
    except FileNotFoundError:
        hits, rx = [], re.compile(pattern)
        for dp, _, fns in os.walk(base):
            for fn in fns:
                if glob and not re.match(glob.replace("*", ".*"), fn):
                    continue
                fp = os.path.join(dp, fn)
                try:
                    for i, ln in enumerate(open(fp, encoding="utf-8", errors="replace")):
                        if rx.search(ln):
                            hits.append("%s:%d:%s" % (fp, i + 1, ln.rstrip()))
                            if len(hits) >= max_results:
                                return "\n".join(hits)
                except OSError:
                    pass
        return "\n".join(hits) or "(no match)"


def t_run_cmd(cmd, timeout=60, **_):
    r = subprocess.run(cmd, shell=True, capture_output=True, text=True,
                       timeout=min(int(timeout), 300), cwd=ROOT,
                       encoding="utf-8", errors="replace")
    return "rc=%d\n%s\n%s" % (r.returncode, r.stdout[-9000:], r.stderr[-3000:])


def t_write_file(path, content, **_):
    p = _resolve(path, GLOBAL["write_roots"])
    os.makedirs(os.path.dirname(p), exist_ok=True)
    with open(p, "w", encoding="utf-8") as f:
        f.write(content)
    BUS.pub("main", "file.written", {"path": p})
    return "wrote %s (%dB)" % (p, len(content))


def _tool(fn, name, desc, props, required=()):
    return {"fn": fn, "schema": {"type": "function", "function": {
        "name": name, "description": desc,
        "parameters": {"type": "object", "properties": props,
                       "required": list(required)}}}}


TOOLS = {
    "read_file": _tool(t_read_file, "read_file", "读文件(限 tool_roots)",
        {"path": {"type": "string"}, "offset": {"type": "integer"},
         "limit": {"type": "integer"}}, ["path"]),
    "list_dir": _tool(t_list_dir, "list_dir", "列目录(限 tool_roots)",
        {"path": {"type": "string"}}),
    "grep": _tool(t_grep, "grep", "正则搜索文件内容(限 tool_roots)",
        {"pattern": {"type": "string"}, "path": {"type": "string"},
         "glob": {"type": "string"}, "context": {"type": "integer"},
         "max_results": {"type": "integer"}}, ["pattern"]),
    "run_cmd": _tool(t_run_cmd, "run_cmd", "执行 shell 命令(cwd=配置根,≤300s)",
        {"cmd": {"type": "string"}, "timeout": {"type": "integer"}}, ["cmd"]),
    "write_file": _tool(t_write_file, "write_file", "写文件(限 write_roots)",
        {"path": {"type": "string"}, "content": {"type": "string"}},
        ["path", "content"]),
}


def load_tool_plugins(root):
    tdir = os.path.join(root, "tools")
    if not os.path.isdir(tdir):
        return
    for fn in sorted(os.listdir(tdir)):
        if not fn.endswith(".py") or fn.startswith("_"):
            continue
        mod_name = fn[:-3]
        try:
            spec = importlib.util.spec_from_file_location(
                "harness_plugin_" + mod_name, os.path.join(tdir, fn))
            mod = importlib.util.module_from_spec(spec)
            spec.loader.exec_module(mod)
            for tn, tdef in (getattr(mod, "TOOLS", {}) or {}).items():
                key = tn if tn not in TOOLS else "%s.%s" % (mod_name, tn)
                TOOLS[key] = tdef
        except Exception as e:
            print("[plugin] %s 加载失败: %s" % (fn, e), file=sys.stderr)


def effective_tools(agent):
    """agent.tools 白名单 ∩ permissions 类别。返回 schema 列表。"""
    perm = agent.get("permissions") or {}
    if not perm.get("tools", True) or NO_TOOLS:
        return []
    out = []
    for tn in agent.get("tools", []):
        if tn not in TOOLS:
            continue
        cls = TOOL_CLASS.get(tn)      # 插件工具默认 read 类
        if cls is None:
            cls = "read"
        if perm.get(cls, True):
            out.append(TOOLS[tn]["schema"])
    return out


def check_tool_perm(agent, tname):
    perm = agent.get("permissions") or {}
    if not perm.get("tools", True):
        return "tools disabled for this agent"
    if tname not in (agent.get("tools") or []):
        return "tool not in agent allowlist: " + tname
    cls = TOOL_CLASS.get(tname, "read")
    if not perm.get(cls, True):
        return "permission denied: %s class" % cls
    return None


def run_tool(agent, name, argstr):
    deny = check_tool_perm(agent, name)
    if deny:
        return "tool denied: " + deny
    try:
        args = json.loads(argstr) if isinstance(argstr, str) else (argstr or {})
        out = TOOLS[name]["fn"](**args)
        if not isinstance(out, str):
            out = json.dumps(out, ensure_ascii=False)
    except Exception as e:
        out = "tool error: %s" % e
    mx = int(GLOBAL.get("tool_output_max", 12000))
    return out if len(out) <= mx else out[:mx] + "\n...[truncated %dB]" % (len(out) - mx)


# ---------------- 总线 + 状态机 ----------------

class Bus:
    """事件总线: pub/sub(fnmatch pattern)/wait; 所有事件进 run.events。"""
    def __init__(self, run=None):
        self.run = run
        self.subs = []          # [(agent_name, pattern, note)]
        self.history = []       # 最近事件(供 late joiner)
        self.cond = threading.Condition()

    def pub(self, who, event, data):
        ev = {"t": time.strftime("%T"), "from": who, "event": event, "data": data}
        with self.cond:
            self.history.append(ev)
            self.history = self.history[-500:]
            self.cond.notify_all()
        if self.run:
            self.run.event(kind="bus", **ev)
        # 派发给订阅者(经由其 AgentProc push; 主进程内 agent 忽略)
        for name, pat, note in list(self.subs):
            if fnmatch.fnmatchcase(event, pat):
                proc = PROCS.get(name)
                if proc:
                    proc.push({"type": "push", "event": "bus",
                               "data": {"matched": pat, "note": note, **ev}})

    def sub(self, who, pattern, note=""):
        self.subs.append((who, pattern, note))

    def wait(self, who, pattern, timeout=60):
        deadline = time.time() + timeout
        with self.cond:
            seen = len(self.history)
            while time.time() < deadline:
                for ev in self.history[seen:]:
                    if fnmatch.fnmatchcase(ev["event"], pattern):
                        return ev
                seen = len(self.history)
                self.cond.wait(timeout=max(0.1, deadline - time.time()))
        return None


BUS = Bus()
PROCS = {}   # agent_name -> AgentProc

STATES = ("boot", "idle", "thinking", "tooling", "waiting",
          "speaking", "done", "error", "stopped")


class StateMachine:
    def __init__(self, run):
        self.run = run
        self.states = {}

    def set(self, agent, state, detail=""):
        assert state in STATES, state
        self.states[agent] = {"state": state, "detail": detail,
                              "t": time.strftime("%T")}
        p = os.path.join(self.run.dir, "state.json")
        with open(p, "w", encoding="utf-8") as f:
            json.dump(self.states, f, ensure_ascii=False, indent=1)
        self.run.event(kind="state", agent=agent, state=state, detail=detail)
        BUS.pub("sm", "agent.state", {"agent": agent, "state": state})

    def snap(self):
        return dict(self.states)


class Run:
    def __init__(self, mode, topic):
        self.dir = os.path.join(ROOT, "runs",
                                time.strftime("%Y%m%d-%H%M%S") + "-"
                                + mode + "-" + uuid.uuid4().hex[:4])
        os.makedirs(os.path.join(self.dir, "artifacts"), exist_ok=True)
        self.transcript = ["# %s: %s" % (mode, topic),
                           "started " + time.strftime("%F %T"), ""]
        self.events = open(os.path.join(self.dir, "events.jsonl"), "a",
                           encoding="utf-8")
        self.calls = {}
        self.usage = {"prompt_tokens": 0, "completion_tokens": 0}
        self.sm = StateMachine(self)
        BUS.run = self

    def say(self, who, text, wrote=None):
        self.transcript.append("## %s\n%s\n" % (who, text))
        if wrote:
            self.transcript.append("*(artifacts: %s)*\n" % ", ".join(wrote))
        self.flush()

    def flush(self):
        with open(os.path.join(self.dir, "transcript.md"), "w", encoding="utf-8") as f:
            f.write("\n".join(self.transcript))

    def event(self, **kw):
        self.events.write(json.dumps(kw, ensure_ascii=False) + "\n")
        self.events.flush()


# ---------------- endpoint(流式) ----------------

SESSIONS = {}


def _save_sessions():
    if GLOBAL.get("sessions") == "persist":
        json.dump(SESSIONS, open(os.path.join(ROOT, ".sessions.json"), "w"), indent=1)


def session_for(agent_name):
    if GLOBAL.get("sessions") == "none":
        return None
    if agent_name not in SESSIONS:
        SESSIONS[agent_name] = "h-" + uuid.uuid4().hex[:16]
        _save_sessions()
    return SESSIONS[agent_name]


def _req(ep, body, sess):
    return urllib.request.Request(
        ep["base"].rstrip("/") + "/chat/completions",
        data=json.dumps(body).encode(),
        headers={"Content-Type": "application/json",
                 "Authorization": "Bearer " + ep.get("key", "none"),
                 "Accept": "text/event-stream",
                 **({"X-Session-Id": sess} if sess else {})})


def _open(req):
    last = None
    for attempt in range(3):
        try:
            return urllib.request.urlopen(req, timeout=int(GLOBAL.get("timeout_s", 300)))
        except urllib.error.HTTPError:
            raise
        except Exception as e:
            last = e
            time.sleep(2 * (attempt + 1))
    raise last


def _parse_sse(r, on_delta=None):
    """SSE 流 → (message, usage)。accumulate content + tool_calls delta。"""
    parts, tbuf, usage = [], {}, {}
    for raw in r:
        line = raw.decode("utf-8", "replace").strip()
        if not line.startswith("data:"):
            continue
        data = line[5:].strip()
        if data == "[DONE]":
            break
        try:
            d = json.loads(data)
        except ValueError:
            continue
        if d.get("usage"):
            usage = d["usage"]
        ch = (d.get("choices") or [{}])[0]
        delta = ch.get("delta") or {}
        if delta.get("content"):
            parts.append(delta["content"])
            if on_delta:
                on_delta(delta["content"], "content")
        if delta.get("reasoning_content") and on_delta:
            on_delta(delta["reasoning_content"], "reasoning")
        for tc in delta.get("tool_calls") or []:
            idx = tc.get("index", 0)
            acc = tbuf.setdefault(idx, {"id": "", "type": "function",
                                        "function": {"name": "", "arguments": ""}})
            if tc.get("id"):
                acc["id"] = tc["id"]
            fn = tc.get("function") or {}
            if fn.get("name"):
                acc["function"]["name"] += fn["name"]
            if fn.get("arguments"):
                acc["function"]["arguments"] += fn["arguments"]
    msg = {"role": "assistant", "content": "".join(parts)}
    if tbuf:
        msg["tool_calls"] = [tbuf[i] for i in sorted(tbuf)]
    return msg, usage


def call_model(agent, agent_name, messages, tools=None, on_delta=None):
    """→ (message, usage, session)。流式;developer 不支持时按 role_map 降级。"""
    ep = ENDPOINTS[EP_OVERRIDE or agent["endpoint"]]
    sess = session_for(agent_name)
    body = {"model": agent["model"], "messages": messages,
            "temperature": agent.get("temp", 0.5)}
    if tools:
        body["tools"] = tools
    if GLOBAL.get("stream", True):
        body["stream"] = True
        body["stream_options"] = {"include_usage": True}

    def _do(msgs):
        b = dict(body, messages=msgs)
        if b.get("stream"):
            r = _open(_req(ep, b, sess))
            hdrs = r.headers
            ct = hdrs.get("Content-Type", "")
            if "event-stream" in ct or "text/" in ct:
                msg, usage = _parse_sse(r, on_delta)
                return msg, hdrs, usage
            d = json.loads(r.read())
            return d["choices"][0]["message"], hdrs, d.get("usage") or {}
        d, hdrs = None, None
        r = _open(_req(ep, b, sess))
        d = json.loads(r.read())
        return d["choices"][0]["message"], r.headers, d.get("usage") or {}

    try:
        msg, hdrs, usage = _do(messages)
    except urllib.error.HTTPError as e:
        err = e.read().decode("utf-8", "replace")[:400]
        if e.code == 400 and any(m.get("role") == "developer" for m in messages):
            rm = GLOBAL.get("role_map", {}).get("developer", "system")
            messages = [{**m, "role": rm} if m["role"] == "developer" else m
                        for m in messages]
            msg, hdrs, usage = _do(messages)
        elif e.code == 400 and body.get("stream"):
            body.pop("stream")          # 端点不支持流式 → 降级一次性
            msg, hdrs, usage = _do(messages)
        else:
            raise RuntimeError("HTTP %s: %s" % (e.code, err))
    new_sess = hdrs.get("X-Session-Id")
    if new_sess and new_sess != sess:
        SESSIONS[agent_name] = new_sess
        _save_sessions()
    return msg, usage, SESSIONS.get(agent_name)


# ---------------- 消息栈 ----------------

_CTX_CACHE = [None, 0.0]


def shared_context():
    if _CTX_CACHE[0] is not None and time.time() - _CTX_CACHE[1] < 60:
        return _CTX_CACHE[0]
    parts = []
    cdir = os.path.join(ROOT, "workspace", "context")
    if os.path.isdir(cdir):
        for fn in sorted(os.listdir(cdir)):
            if fn.endswith(".md"):
                parts.append("### context/" + fn + "\n" +
                             open(os.path.join(cdir, fn), encoding="utf-8").read().strip())
    _CTX_CACHE[0], _CTX_CACHE[1] = "\n\n".join(parts), time.time()
    return _CTX_CACHE[0]


def sysmsg(agent, extra=""):
    s = agent.get("persona", "")
    ctx = shared_context()
    if ctx:
        s += "\n\n# 项目上下文\n" + ctx
    if extra:
        s += "\n\n" + extra
    return {"role": "system", "content": s}


def devmsgs(agent, extra=None):
    out = []
    dev = agent.get("developer")
    for t in (dev if isinstance(dev, list) else [dev] if dev else []):
        out.append({"role": "developer", "content": t})
    if extra:
        out.append({"role": "developer", "content": extra})
    return out


FILE_BLOCK = re.compile(r"```file:([^\n]+)\n(.*?)```", re.S)


def extract_artifacts(text, out_dir):
    written = []
    for rel, code in FILE_BLOCK.findall(text or ""):
        path = os.path.join(out_dir, rel.strip().lstrip("/"))
        os.makedirs(os.path.dirname(path), exist_ok=True)
        with open(path, "w", encoding="utf-8") as f:
            f.write(code)
        written.append(rel.strip())
        BUS.pub("main", "artifact.written", {"path": rel.strip()})
    return written


# ---------------- agent 衍生进程(主进程侧) ----------------

class AgentProc:
    """子进程 agent: stdin/stdout JSONL。子进程自持 persona/history/工具循环;
    模型/工具/总线经 IPC 回主进程执行。"""
    def __init__(self, name, run):
        self.name, self.run = name, run
        self.agent = AGENTS[name]
        env = dict(os.environ, PYTHONIOENCODING="utf-8", PYTHONUTF8="1")
        cmd = [sys.executable, os.path.abspath(__file__),
               "--home", ROOT, "_agent", name]
        self.p = subprocess.Popen(cmd, stdin=subprocess.PIPE, stdout=subprocess.PIPE,
                                  stderr=open(os.path.join(run.dir, name + ".stderr.log"), "w", encoding="utf-8"),
                                  text=True,
                                  encoding="utf-8", env=env, bufsize=1)
        self.pending = {}      # req id -> queue
        self.last_streamed = False
        self.req_n = [0]
        self.wlock = threading.Lock()
        self.done_evt = threading.Event()
        self.turn_res = None
        self.reader = threading.Thread(target=self._read_loop, daemon=True)
        self.reader.start()
        # watch 声明 → 自动订阅(需 collab)
        if (self.agent.get("permissions") or {}).get("collab", True):
            for w in self.agent.get("watch", []) or []:
                BUS.sub(name, w.get("on", "*"), w.get("note", ""))
        run.sm.set(name, "idle")

    def _send(self, obj):
        with self.wlock:
            self.p.stdin.write(json.dumps(obj, ensure_ascii=False) + "\n")
            self.p.stdin.flush()

    def push(self, obj):
        """主进程 → 子进程异步事件(bus.event/delta/inject)。"""
        try:
            self._send(obj)
        except (BrokenPipeError, OSError, ValueError):
            pass

    def _read_loop(self):
        for line in self.p.stdout:
            try:
                m = json.loads(line)
            except ValueError:
                continue
            t = m.get("type")
            if t == "req":
                threading.Thread(target=self._handle_req, args=(m,),
                                 daemon=True).start()
            elif t == "res" and m.get("cmd") == "turn":
                self.turn_res = m
                self.done_evt.set()
            elif t == "log":
                print("    [%s·log] %s" % (self.name, m.get("msg", "")[:120]),
                      flush=True)

    def _reply(self, rid, ok=True, result=None, error=None):
        self._send({"type": "res", "id": rid,
                    **({"result": result} if ok else {"error": error})})

    def _handle_req(self, m):
        rid, method, params = m["id"], m["method"], m.get("params") or {}
        perm = self.agent.get("permissions") or {}
        try:
            if method == "model.call":
                self.run.sm.set(self.name, "thinking")
                tag = "[%s] " % self.name
                print("\n" + tag, end="", flush=True)
                def on_delta(txt, kind):
                    if kind == "content":
                        self.last_streamed = True
                        print(txt, end="", flush=True)
                msg, usage, sess = call_model(self.agent, self.name,
                                              params["messages"],
                                              params.get("tools"),
                                              on_delta=on_delta)
                print("", flush=True)
                for k in ("prompt_tokens", "completion_tokens"):
                    self.run.usage[k] += int(usage.get(k, 0) or 0)
                self._reply(rid, result={"message": msg, "usage": usage})
            elif method == "tool.call":
                self.run.sm.set(self.name, "tooling",
                                params.get("name", ""))
                print("\n    [%s] tool %s(%s)" % (self.name, params.get("name"),
                      str(params.get("args"))[:70]), flush=True)
                res = run_tool(self.agent, params.get("name"),
                               params.get("args"))
                self.run.event(kind="tool", agent=self.name,
                               tool=params.get("name"),
                               args=str(params.get("args"))[:200])
                self._reply(rid, result={"result": res})
            elif method == "bus.pub":
                if not perm.get("collab", True):
                    self._reply(rid, ok=False, error="collab denied")
                else:
                    BUS.pub(self.name, params["event"], params.get("data"))
                    self._reply(rid, result={"ok": True})
            elif method == "bus.sub":
                if not perm.get("collab", True):
                    self._reply(rid, ok=False, error="collab denied")
                else:
                    BUS.sub(self.name, params["pattern"], params.get("note", ""))
                    self._reply(rid, result={"ok": True})
            elif method == "bus.wait":
                if not perm.get("collab", True):
                    self._reply(rid, ok=False, error="collab denied")
                else:
                    self.run.sm.set(self.name, "waiting", params.get("pattern", ""))
                    ev = BUS.wait(self.name, params["pattern"],
                                  float(params.get("timeout", 60)))
                    self._reply(rid, result={"event": ev})
            elif method == "context.get":
                self._reply(rid, result={"context": shared_context()})
            elif method == "state.set":
                st = params.get("state", "idle")
                self.run.sm.set(self.name, st if st in STATES else "idle",
                                params.get("detail", ""))
                self._reply(rid, result={"ok": True})
            else:
                self._reply(rid, ok=False, error="unknown method " + str(method))
        except Exception as e:
            self.run.sm.set(self.name, "error", str(e)[:120])
            self._reply(rid, ok=False, error=str(e)[:300])

    def turn(self, prompt, history=None, extra_sys="", prefill=None, timeout=None):
        self.last_streamed = False
        self.done_evt.clear()
        self.turn_res = None
        self._send({"type": "cmd", "cmd": "turn", "id": 0,
                    "params": {"prompt": prompt, "history": history or [],
                               "extra_sys": extra_sys, "prefill": prefill}})
        self.run.sm.set(self.name, "thinking")
        to = timeout or int(GLOBAL.get("turn_timeout_s", 1800))
        if not self.done_evt.wait(to):
            self.run.sm.set(self.name, "error", "turn timeout")
            return "*(turn timeout)*"
        r = self.turn_res or {}
        if not r.get("ok", True):
            return "*(agent error: %s)*" % r.get("error")
        self.run.sm.set(self.name, "done")
        text = (r.get("result") or {}).get("text", "")
        if not self.last_streamed and text:
            print(text, flush=True)
        wrote = extract_artifacts(text, os.path.join(self.run.dir, "artifacts"))
        self.run.say(self.name, text, wrote)
        return text

    def inject(self, text):
        self.push({"type": "push", "event": "inject", "data": {"text": text}})

    def stop(self):
        try:
            self._send({"type": "cmd", "cmd": "shutdown", "id": -1})
            self.p.wait(5)
        except Exception:
            self.p.kill()
        self.run.sm.set(self.name, "stopped")


# ---------------- agent 子进程本体 ----------------

def _child_ipc_call(w, r, pend, method, params, timeout=None):
    """子进程 → 主进程请求-响应。返回 result 或抛错。"""
    rid = w["seq"]()
    q = queue.Queue()
    pend[rid] = q
    w["send"]({"type": "req", "id": rid, "method": method, "params": params})
    try:
        m = q.get(timeout=timeout or int(GLOBAL.get("timeout_s", 300)) + 60)
        if "error" in m:
            raise RuntimeError(m["error"])
        return m.get("result")
    finally:
        pend.pop(rid, None)


def agent_main(name):
    """子进程入口: 自持 history/inbox; turn 命令驱动; 资源全走 IPC。"""
    agent = AGENTS[name]
    pend = {}
    inbox = queue.Queue()      # inject/bus push 落地
    history = []               # agent 私有对话历史
    seq = itertools_count()

    wlock = threading.Lock()
    def send(obj):
        with wlock:
            sys.stdout.write(json.dumps(obj, ensure_ascii=False) + "\n")
            sys.stdout.flush()
    w = {"send": send, "seq": seq}

    def ipc(method, params, timeout=None):
        return _child_ipc_call(w, None, pend, method, params, timeout)

    def drain_inbox(msgs):
        while not inbox.empty():
            ev = inbox.get_nowait()
            if ev.get("event") == "inject":
                msgs.append({"role": "user",
                             "content": "[注入消息] " + ev["data"]["text"]})
            elif ev.get("event") == "bus":
                d = ev["data"]
                msgs.append({"role": "developer", "content":
                    "[总线事件 %s] from=%s data=%s note=%s"
                    % (d.get("event"), d.get("from"),
                       json.dumps(d.get("data"), ensure_ascii=False)[:300],
                       d.get("note", ""))})

    def do_turn(params):
        nonlocal history
        history = params.get("history") or history
        tools = None
        tl = effective_tools(agent)
        if tl:
            tools = tl
        msgs = [sysmsg(agent, params.get("extra_sys", ""))]
        msgs += devmsgs(agent)
        msgs += history
        msgs.append({"role": "user", "content": params["prompt"]})
        if params.get("prefill"):
            msgs.append({"role": "assistant", "content": params["prefill"]})
        out = ""
        for _ in range(int(GLOBAL.get("max_tool_rounds", 12))):
            drain_inbox(msgs)
            r = ipc("model.call", {"messages": msgs, "tools": tools})
            msg = r["message"]
            tcs = msg.get("tool_calls") or []
            if not tcs:
                out = msg.get("content") or ""
                break
            msgs.append({"role": "assistant",
                         "content": msg.get("content") or "",
                         "tool_calls": tcs})
            for tc in tcs:
                fn = tc.get("function", {})
                try:
                    res = ipc("tool.call", {"name": fn.get("name"),
                                            "args": fn.get("arguments", "")})
                    content = res["result"]
                except Exception as e:
                    content = "tool ipc error: %s" % e
                msgs.append({"role": "tool",
                             "tool_call_id": tc.get("id", ""),
                             "content": content})
        else:
            out = "(max tool rounds) " + (msg.get("content") or "")
        return out

    cmds = queue.Queue()

    def reader():
        for line in sys.stdin:
            try:
                m = json.loads(line)
            except ValueError:
                continue
            t = m.get("type")
            if t == "res":
                q = pend.get(m.get("id"))
                if q:
                    q.put(m)
            elif t == "push":
                inbox.put(m)
            elif t == "cmd":
                cmds.put(m)
        cmds.put(None)

    threading.Thread(target=reader, daemon=True).start()

    # 主线程: 消费 cmd(turn/shutdown); ipc 请求的 res 由 reader 线程路由
    while True:
        m = cmds.get()
        if m is None or m["cmd"] == "shutdown":
            break
        if m["cmd"] == "turn":
            try:
                text = do_turn(m.get("params") or {})
                send({"type": "res", "id": m["id"], "cmd": "turn",
                      "ok": True, "result": {"text": text}})
            except Exception as e:
                send({"type": "res", "id": m["id"], "cmd": "turn",
                      "ok": False, "error": str(e)[:300]})


def itertools_count():
    i = [0]
    def nxt():
        i[0] += 1
        return i[0]
    return nxt


# ---------------- 主进程内 speak(inproc 回退路径) ----------------

def speak_inproc(run, name, prompt, history, extra_sys="", prefill=None):
    agent = AGENTS[name]
    msgs = [sysmsg(agent, extra_sys)] + devmsgs(agent, _reminder_for(name, run))
    msgs += history + [{"role": "user", "content": prompt}]
    if prefill:
        msgs.append({"role": "assistant", "content": prefill})
    tools = effective_tools(agent) or None
    print("  [%s] thinking..." % name, flush=True)
    run.sm.set(name, "thinking")
    t0, tool_log, seen = time.time(), [], {}
    usage, out, msg = {}, "", {}
    def _d(txt, kind):
        if kind == "content":
            print(txt, end="", flush=True)
    try:
        for _ in range(int(GLOBAL.get("max_tool_rounds", 12))):
            msg, usage, _ = call_model(agent, name, msgs, tools, on_delta=_d)
            for k in ("prompt_tokens", "completion_tokens"):
                run.usage[k] += int(usage.get(k, 0) or 0)
            tcs = msg.get("tool_calls") or []
            if not tcs:
                out = msg.get("content") or ""
                break
            run.sm.set(name, "tooling")
            msgs.append({"role": "assistant", "content": msg.get("content") or "",
                         "tool_calls": tcs})
            for tc in tcs:
                fn = tc.get("function", {})
                tn, ta = fn.get("name", "?"), fn.get("arguments", "")
                if (tn, ta) in seen:
                    res = seen[(tn, ta)] + "\n[duplicate call - 结果同上]"
                else:
                    res = run_tool(agent, tn, ta)
                    seen[(tn, ta)] = res
                tool_log.append({"tool": tn, "args": ta[:200]})
                print("\n    [%s] %s(%s)" % (name, tn, ta[:70]), flush=True)
                msgs.append({"role": "tool", "tool_call_id": tc.get("id", ""),
                             "content": res})
        else:
            out = "(max tool rounds) " + (msg.get("content") or "")
    except Exception as e:
        out = "*(call failed: %s)*" % e
        run.sm.set(name, "error", str(e)[:120])
    print("", flush=True)
    run.calls[name] = run.calls.get(name, 0) + 1
    wrote = extract_artifacts(out, os.path.join(run.dir, "artifacts"))
    run.say(name, out, wrote)
    run.sm.set(name, "done")
    run.event(agent=name, ms=int((time.time() - t0) * 1000), chars=len(out),
              usage=usage, wrote=wrote, tools=tool_log, session=SESSIONS.get(name))
    return out


def _reminder_for(agent_name, run):
    rem = GLOBAL.get("reminder") or {}
    text = REMIND_OVERRIDE if REMIND_OVERRIDE is not None else rem.get("text", "")
    every = int(rem.get("every", 0))
    if not text or not every:
        return None
    return text if run.calls.get(agent_name, 0) % every == 0 else None


# ---------------- 进程池 + 模式 ----------------

USE_PROCS = True


def spawn(name, run):
    if name not in PROCS or PROCS[name].p.poll() is not None:
        PROCS[name] = AgentProc(name, run)
    return PROCS[name]


def turn_of(run, name, prompt, history=None, **kw):
    """统一发言入口: 子进程优先,--inproc 时走主进程内循环。"""
    if USE_PROCS:
        return spawn(name, run).turn(prompt, history, **kw)
    return speak_inproc(run, name, prompt, history or [], **kw)


def stop_all(run):
    for p in PROCS.values():
        try:
            p.stop()
        except Exception:
            pass


def _synth(run, agent_name, extra_sys, history, user_prompt):
    msgs = [sysmsg(AGENTS[agent_name], extra_sys)] + history
    msgs += [{"role": "user", "content": user_prompt}]
    try:
        msg, usage, _ = call_model(AGENTS[agent_name], agent_name, msgs)
        for k in ("prompt_tokens", "completion_tokens"):
            run.usage[k] += int(usage.get(k, 0) or 0)
        return msg.get("content") or ""
    except Exception as e:
        return "*(synthesis failed: %s)*" % e


def ask(topic, agent_name, prefill=None):
    run = Run("ask", topic)
    serve_control(run)
    name = agent_name or next(iter(AGENTS))
    try:
        out = turn_of(run, name, topic, prefill=prefill)
        print("\n" + out)
    finally:
        stop_all(run)
    print("\nrun dir: %s | tokens %s" % (run.dir, run.usage))


def chat(agent_name):
    name = agent_name or next(iter(AGENTS))
    run = Run("chat", "interactive:" + name)
    serve_control(run)
    history = []
    print("chat %s (%s) | /use /agents /inject /send A msg /status /quit"
          % (name, AGENTS[name].get("model")))
    try:
        while True:
            try:
                line = input("\n>>> ").strip()
            except (EOFError, KeyboardInterrupt):
                break
            if not line:
                continue
            if line in ("/quit", "/q"):
                break
            if line == "/agents":
                for n, a in AGENTS.items():
                    print("  %-16s %-26s @%s" % (n, a.get("model", "?"),
                                                a.get("endpoint", "?")))
                continue
            if line == "/status":
                for n, s in run.sm.snap().items():
                    print("  %-16s %-10s %s" % (n, s["state"], s.get("detail", "")))
                continue
            if line.startswith("/use "):
                n = line[5:].strip()
                if n in AGENTS:
                    name, history = n, []
                    print("switched -> %s, history cleared" % n)
                else:
                    print("未知 agent " + n)
                continue
            if line.startswith("/inject "):
                spawn(name, run).inject(line[8:])
                print("(injected)")
                continue
            if line.startswith("/send "):
                try:
                    _, dst, msg = line.split(" ", 2)
                    spawn(dst.strip(), run).inject("[来自用户/chat 的消息] " + msg)
                    print("(sent to %s)" % dst)
                except ValueError:
                    print("用法: /send AGENT msg")
                continue
            out = turn_of(run, name, line, history)
            history += [{"role": "user", "content": line},
                        {"role": "assistant", "content": out}]
            if not USE_PROCS:
                print()  # inproc 已流式输出
            else:
                pass  # 子进程流式 delta 已由主进程回显
    finally:
        stop_all(run)
        run.say("system", "chat ended, tokens=%s" % run.usage)
        print("run dir: " + run.dir)


def brainstorm(topic, rounds, roster):
    run = Run("brainstorm", topic)
    serve_control(run)
    roster = [n for n in roster if n in AGENTS] or list(AGENTS)
    history = []
    brief = ("议题: %s\n你是圆桌成员。基于项目上下文和前面发言给出专业判断。"
             "可以: 提出假设、给判别实验、指出他人盲点、用工具取证。具体、可操作,400 字内。" % topic)
    try:
        for rd in range(1, rounds + 1):
            run.say("system", "### --- round %d ---" % rd)
            BUS.pub("main", "round.start", {"round": rd})
            for name in roster:
                out = turn_of(run, name,
                              brief if rd == 1 else "继续。回应前面新观点,深化或反驳。",
                              history)
                history.append({"role": "assistant", "name": name,
                                "content": "[%s] %s" % (name, out)})
                BUS.pub(name, "agent.spoke", {"round": rd, "chars": len(out)})
        orch = "orchestrator" if "orchestrator" in AGENTS else roster[0]
        out = _synth(run, orch,
                     "对圆桌全程收敛: (1)已共识假设 (2)判别实验清单(按信息量/成本) (3)分工。",
                     history, "收敛本轮讨论。")
        run.say("orchestrator(synthesis)", out,
                extract_artifacts(out, os.path.join(run.dir, "artifacts")))
    finally:
        stop_all(run)
    print("\nrun dir: %s | tokens %s" % (run.dir, run.usage))


def council(question, roster):
    run = Run("council", question)
    serve_control(run)
    roster = [n for n in roster if n in AGENTS] or list(AGENTS)
    answers = {}
    try:
        if USE_PROCS:
            procs = {n: spawn(n, run) for n in roster}
            def _ask(n):
                return n, procs[n].turn(
                    "问题: %s\n独立给出分析和结论(勿参考他人)。具体、可用工具取证。" % question)
        else:
            def _ask(n):
                return n, speak_inproc(run, n,
                    "问题: %s\n独立给出分析和结论(勿参考他人)。具体、可用工具取证。" % question, [])
        with ThreadPoolExecutor(max_workers=len(roster)) as ex:
            for n, out in ex.map(_ask, roster):
                answers[n] = out
                BUS.pub(n, "agent.spoke", {"chars": len(out)})
        history = [{"role": "assistant", "name": n, "content": "[%s] %s" % (n, a)}
                   for n, a in answers.items()]
        for name in roster:
            others = "\n\n".join("[%s] %s" % (n, a) for n, a in answers.items() if n != name)
            out = turn_of(run, name,
                          "问题: %s\n其他人的答案:\n%s\n评审: 最强点、最弱点、被漏掉的变量。"
                          % (question, others), history)
            history.append({"role": "assistant", "name": name,
                            "content": "[%s·评审] %s" % (name, out)})
        orch = "orchestrator" if "orchestrator" in AGENTS else roster[0]
        out = _synth(run, orch,
                     "综合作答与互评,给最终裁决: 结论 + 置信度 + 最关键验证实验。",
                     history, "对问题裁决: " + question)
        run.say("orchestrator(verdict)", out,
                extract_artifacts(out, os.path.join(run.dir, "artifacts")))
    finally:
        stop_all(run)
    print("\nrun dir: %s | tokens %s" % (run.dir, run.usage))


def task(desc, roster):
    run = Run("task", desc)
    serve_control(run)
    orch = "orchestrator" if "orchestrator" in AGENTS else roster[0]
    roster = [n for n in roster if n in AGENTS] or list(AGENTS)
    try:
        plan_raw = _synth(run, orch,
                          "把任务拆成可执行子任务,每项指定负责人(从 %s 选)。"
                          "只输出 JSON: [{id,owner,title,depends_on}]。" % roster,
                          [], "任务: " + desc)
        run.say("orchestrator(plan)", plan_raw)
        try:
            plan = json.loads(re.search(r"\[.*\]", plan_raw, re.S).group(0))
        except Exception:
            plan = [{"id": "t1", "owner": roster[0], "title": desc, "depends_on": []}]
        done = {}
        for step in plan:
            owner = step.get("owner") if step.get("owner") in AGENTS else roster[0]
            dep = "\n".join("[%s] %s" % (k, done[k][:1500])
                            for k in step.get("depends_on", []) if k in done)
            prompt = "子任务: %s\n" % step.get("title")
            if dep:
                prompt += "前序产出:\n%s\n" % dep
            prompt += "完成它。产出用 ```file:path 代码块或 write_file 工具写文件。"
            done[step.get("id", "?")] = turn_of(run, owner, prompt)
            BUS.pub(owner, "task.done", {"id": step.get("id")})
        if "critic" in AGENTS:
            allout = "\n\n".join("### %s\n%s" % (k, v[:2000]) for k, v in done.items())
            turn_of(run, "critic", "验收产出,列问题/缺口/下一步:\n" + allout)
    finally:
        stop_all(run)
    print("\nrun dir: %s | tokens %s" % (run.dir, run.usage))


# ---------------- 控制套接字(运行期管理) ----------------

def serve_control(run):
    """localhost 控制端口: status / inject / stop / state。端口写 run 目录。"""
    srv = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
    srv.bind(("127.0.0.1", 0))
    srv.listen(4)
    port = srv.getsockname()[1]
    with open(os.path.join(run.dir, "control.port"), "w") as f:
        f.write(str(port))
    with open(os.path.join(ROOT, "runs", "latest.port"), "w") as f:
        f.write("%s %s" % (port, run.dir))

    def handle(conn):
        try:
            line = conn.makefile("r", encoding="utf-8").readline()
            cmd = json.loads(line)
            op = cmd.get("op")
            if op == "status":
                res = {"states": run.sm.snap(), "usage": run.usage,
                       "procs": {n: (p.p.poll() is None) for n, p in PROCS.items()}}
            elif op == "inject":
                proc = PROCS.get(cmd["agent"])
                if proc:
                    proc.inject(cmd["msg"])
                    res = {"ok": True}
                else:
                    res = {"ok": False, "error": "no such live agent"}
            elif op == "state":
                res = run.sm.snap().get(cmd["agent"], {})
            else:
                res = {"error": "unknown op"}
            conn.sendall((json.dumps(res, ensure_ascii=False) + "\n").encode())
        except Exception as e:
            try:
                conn.sendall((json.dumps({"error": str(e)}) + "\n").encode())
            except Exception:
                pass
        finally:
            conn.close()

    def loop():
        while True:
            try:
                c, _ = srv.accept()
            except OSError:
                return
            threading.Thread(target=handle, args=(c,), daemon=True).start()

    threading.Thread(target=loop, daemon=True).start()
    run.control_port = port


def _ctl_call(rundir, obj):
    port_file = os.path.join(rundir, "control.port") if os.path.isdir(rundir) else None
    if not port_file or not os.path.exists(port_file):
        # 尝试 latest.port
        lp = os.path.join(ROOT, "runs", "latest.port")
        if os.path.exists(lp):
            port, _ = open(lp).read().split(" ", 1)
        else:
            return {"error": "no live run"}
    else:
        port = open(port_file).read().strip()
    try:
        s = socket.create_connection(("127.0.0.1", int(port)), timeout=5)
        s.sendall((json.dumps(obj, ensure_ascii=False) + "\n").encode())
        out = s.makefile("r", encoding="utf-8").readline()
        s.close()
        return json.loads(out)
    except (OSError, ValueError):
        return {"error": "control port dead"}


def cmd_status(rundir=None):
    live = _ctl_call(rundir or "", {"op": "status"})
    if "error" not in live:
        print("live run:")
        for n, s in live["states"].items():
            print("  %-16s %-10s %s" % (n, s["state"], s.get("detail", "")))
        print("  usage:", live["usage"])
        return
    # 无活 run: 读最近 state.json
    rdir = rundir
    if not rdir:
        rds = sorted(os.listdir(os.path.join(ROOT, "runs")))
        rds = [d for d in rds if os.path.isdir(os.path.join(ROOT, "runs", d))]
        if not rds:
            print("no runs")
            return
        rdir = os.path.join(ROOT, "runs", rds[-1])
    sf = os.path.join(rdir, "state.json")
    if os.path.exists(sf):
        for n, s in json.load(open(sf, encoding="utf-8")).items():
            print("  %-16s %-10s %s" % (n, s["state"], s.get("detail", "")))
    else:
        print("no state in " + rdir)


def cmd_inject(rundir, agent, msg):
    print(json.dumps(_ctl_call(rundir, {"op": "inject", "agent": agent,
                                        "msg": msg}), ensure_ascii=False))


def cmd_export(rundir=None, out=None):
    if not rundir:
        rds = sorted(d for d in os.listdir(os.path.join(ROOT, "runs"))
                     if os.path.isdir(os.path.join(ROOT, "runs", d)))
        rundir = os.path.join(ROOT, "runs", rds[-1])
    out = out or (rundir.rstrip(os.sep) + ".zip")
    with zipfile.ZipFile(out, "w", zipfile.ZIP_DEFLATED) as z:
        for dp, _, fns in os.walk(rundir):
            for fn in fns:
                fp = os.path.join(dp, fn)
                z.write(fp, os.path.relpath(fp, rundir))
    print("exported %s -> %s" % (rundir, out))


def cmd_import(zip_path):
    stamp = time.strftime("%Y%m%d-%H%M%S") + "-imported"
    dst = os.path.join(ROOT, "runs", stamp)
    with zipfile.ZipFile(zip_path) as z:
        z.extractall(dst)
    print("imported -> " + dst)


# ---------------- cli ----------------

def main():
    global ROOT, GLOBAL, ENDPOINTS, AGENTS
    global EP_OVERRIDE, NO_TOOLS, REMIND_OVERRIDE, USE_PROCS

    pre = argparse.ArgumentParser(add_help=False)
    pre.add_argument("--home")
    known, _ = pre.parse_known_args()
    ROOT = resolve_home(known.home)

    ENDPOINTS = load_json(ROOT, "endpoints.json")
    AGENTS = load_agents(ROOT)
    GLOBAL = load_json(ROOT, "harness.json", {
        "timeout_s": 300, "max_tool_rounds": 12, "tool_output_max": 12000,
        "turn_timeout_s": 1800, "stream": True,
        "tool_roots": [ROOT], "write_roots": [ROOT],
        "reminder": {"text": "", "every": 0}, "sessions": "run",
        "role_map": {"developer": "system"}})
    for k in ("tool_roots", "write_roots"):
        GLOBAL[k] = [os.path.realpath(p if os.path.isabs(p) else os.path.join(ROOT, p))
                     for p in GLOBAL[k]]
    load_tool_plugins(ROOT)
    if GLOBAL.get("sessions") == "persist" and os.path.exists(
            os.path.join(ROOT, ".sessions.json")):
        try:
            SESSIONS.update(json.load(open(os.path.join(ROOT, ".sessions.json"),
                                           encoding="utf-8")))
        except Exception:
            pass

    ap = argparse.ArgumentParser(prog="harness", description=__doc__,
                                 formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--home")
    ap.add_argument("--endpoint")
    ap.add_argument("--agents")
    ap.add_argument("--agent")
    ap.add_argument("--rounds", type=int, default=3)
    ap.add_argument("--no-tools", action="store_true")
    ap.add_argument("--inproc", action="store_true", help="agent 不派生子进程,主进程内跑")
    ap.add_argument("--no-stream", action="store_true")
    ap.add_argument("--reminder")
    ap.add_argument("--prefill")
    ap.add_argument("-o", "--out")
    ap.add_argument("cmd", nargs="?")
    ap.add_argument("args", nargs="*")
    a = ap.parse_args()

    if a.endpoint:
        if a.endpoint not in ENDPOINTS:
            sys.exit("未知端点 %s; 可选: %s" % (a.endpoint, list(ENDPOINTS)))
        EP_OVERRIDE = a.endpoint
    NO_TOOLS = a.no_tools
    REMIND_OVERRIDE = a.reminder
    USE_PROCS = not a.inproc
    if a.no_stream:
        GLOBAL["stream"] = False

    cmd = a.cmd
    if cmd == "_agent":
        agent_main(a.args[0])
        return
    if not cmd:
        ap.print_help()
        sys.exit(1)
    if cmd == "agents":
        for n, ag in AGENTS.items():
            p = ag.get("permissions", {})
            perm = "".join(c if p.get(k) else "-"
                           for c, k in (("r", "read"), ("w", "write"),
                                        ("x", "exec"), ("c", "collab")))
            tl = ",".join(ag.get("tools", [])) or "-"
            print("%-16s %-26s @%-8s temp=%s perm=%s tools=%s"
                  % (n, ag.get("model", "?"), ag.get("endpoint", "?"),
                     ag.get("temp"), perm, tl))
        return
    if cmd == "tools":
        for n, t in TOOLS.items():
            print("%-18s [%s] %s" % (n, TOOL_CLASS.get(n, "read"),
                                     t["schema"]["function"]["description"]))
        return
    if cmd == "check":
        for n, ag in AGENTS.items():
            try:
                m, u, s = call_model(ag, n,
                                     [{"role": "user", "content": "回复 ok 两个字"}])
                print("%-16s OK  sess=%s  %r" % (n, s, str(m.get("content"))[:40]))
            except Exception as e:
                print("%-16s FAIL %s" % (n, str(e)[:100]))
        return
    if cmd == "status":
        cmd_status(a.args[0] if a.args else None)
        return
    if cmd == "inject":
        if len(a.args) < 3:
            sys.exit("用法: inject RUNDIR AGENT msg")
        cmd_inject(a.args[0], a.args[1], " ".join(a.args[2:]))
        return
    if cmd == "export":
        cmd_export(a.args[0] if a.args else None, a.out)
        return
    if cmd == "import":
        if not a.args:
            sys.exit("用法: import FILE.zip")
        cmd_import(a.args[0])
        return
    if cmd == "chat":
        chat(a.agent)
        return

    if not a.args:
        sys.exit("需要 topic 参数")
    topic = " ".join(a.args)
    roster = (a.agents or ",".join(AGENTS)).split(",")
    {"ask": lambda: ask(topic, a.agent, a.prefill),
     "brainstorm": lambda: brainstorm(topic, a.rounds, roster),
     "council": lambda: council(topic, roster),
     "task": lambda: task(topic, roster)}[cmd]()


if __name__ == "__main__":
    main()
