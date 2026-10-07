import json, sys, http.server
LOG = sys.argv[2]
def log(rec):
    with open(LOG, "a") as f: f.write(json.dumps(rec) + "\n")
def chunk(delta, finish=None, usage=None):
    c = {"id":"c1","object":"chat.completion.chunk","created":0,"model":"mock","choices":[{"index":0,"delta":delta,"finish_reason":finish}]}
    if usage: c["usage"] = usage
    return "data: " + json.dumps(c) + "\n\n"
class H(http.server.BaseHTTPRequestHandler):
    def do_POST(self):
        n = int(self.headers.get("content-length", 0)); body = self.rfile.read(n)
        try: j = json.loads(body)
        except Exception: j = None
        hdrs = {k.lower(): v for k, v in self.headers.items()}
        tools = [t.get("function", t).get("name") for t in (j or {}).get("tools", [])] if isinstance(j, dict) else []
        log({"path": self.path, "auth": hdrs.get("authorization"), "x-api-key": hdrs.get("x-api-key"), "x-extra": hdrs.get("x-extra"), "model": (j or {}).get("model"), "tools": tools, "msg_roles": [m.get("role") for m in (j or {}).get("messages", [])] if isinstance(j, dict) else None})
        if self.path.endswith("/chat/completions"):
            self.send_response(200); self.send_header("content-type", "text/event-stream"); self.end_headers()
            roles = [m.get("role") for m in j.get("messages", [])]
            last_user = [m for m in j.get("messages", []) if m.get("role") == "user"][-1]
            content = last_user.get("content"); content = content if isinstance(content, str) else json.dumps(content)
            if "slow" in content:
                import time; self.wfile.write(chunk({"role":"assistant","content":"partial"}).encode()); self.wfile.flush(); time.sleep(20); return
            if "usetool" in content and "tool" not in roles:
                out = chunk({"role":"assistant","tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"mcp__bridge__echo","arguments":"{\"text\":\"hi\"}"}}]})
                out += chunk({}, "tool_calls", {"prompt_tokens":1,"completion_tokens":1,"total_tokens":2})
            else:
                out = chunk({"role":"assistant","content":"ok"}) + chunk({}, "stop", {"prompt_tokens":1,"completion_tokens":1,"total_tokens":2})
            out += "data: [DONE]\n\n"
            self.wfile.write(out.encode()); return
        self.send_response(401); self.send_header("content-type","application/json"); self.end_headers()
        self.wfile.write(b'{"error":{"type":"authentication_error","message":"mock 401"}}')
    def log_message(self, *a): pass
http.server.ThreadingHTTPServer(("127.0.0.1", int(sys.argv[1])), H).serve_forever()
