#!/usr/bin/env python3
"""mock OpenAI 端点: 冒烟测 harness 调度逻辑,不消耗真模型额度。
返回确定性的 canned 回复,内容带 agent 名便于核对 transcript。"""
import json
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer


class H(BaseHTTPRequestHandler):
    def do_POST(self):
        n = int(self.headers.get("Content-Length", 0))
        body = json.loads(self.rfile.read(n) or b"{}")
        msgs = body.get("messages", [])
        sys_prompt = msgs[0]["content"] if msgs and msgs[0]["role"] == "system" else ""
        persona = sys_prompt.split("\n")[0][:40] if sys_prompt else "?"
        last = msgs[-1]["content"][:60] if msgs else ""
        # TOOLTEST: 首轮发 3 个并行 tool_calls 测批量调度; 含 tool 结果后回复汇总
        has_toolres = any(m.get("role") == "tool" for m in msgs)
        with open("_mock_msgs.log", "a", encoding="utf-8") as f:
            f.write(str([(m.get("role"), str(m.get("content"))[:25])
                        for m in msgs]) + chr(10))
        want_tool = any("TOOLTEST" in str(m.get("content", "")) for m in msgs)
        if want_tool and not has_toolres:
            tcs = [
                {"id": "c1", "type": "function", "function": {
                    "name": "write_file",
                    "arguments": '{\"path\":\"_tooltest.txt\",\"content\":\"TC-DATA\"}'}},
                {"id": "c2", "type": "function", "function": {
                    "name": "list_dir", "arguments": '{\"path\":\".\"}'}},
                {"id": "c3", "type": "function", "function": {
                    "name": "read_file",
                    "arguments": '{\"path\":\"_tooltest.txt\"}'}},
            ]
            self._json(200, {"choices": [{"message": {
                "role": "assistant", "content": "", "tool_calls": tcs}}]})
            return
        if has_toolres:
            toolres = [str(m.get("content", ""))[:50] for m in msgs
                       if m.get("role") == "tool"]
            content = f"[mock:{body.get('model','?')}] tool_results={toolres}"
        else:
            content = f"[mock:{body.get('model','?')}] persona={persona} last_user={last}"
        self._json(200, {"choices": [{"message": {"role": "assistant", "content": content}}]})

    def do_GET(self):
        self._json(200, {"object": "list", "data": [{"id": "mock-1", "object": "model"}]})

    def _json(self, code, obj):
        b = json.dumps(obj).encode()
        self.send_response(code)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(b)))
        self.end_headers()
        self.wfile.write(b)

    def log_message(self, *a):
        pass


if __name__ == "__main__":
    ThreadingHTTPServer(("127.0.0.1", 8399), H).serve_forever()
