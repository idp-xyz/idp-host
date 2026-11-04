#!/usr/bin/env python3
"""Reference harness worker (ADR-0001 D31): the HOST's external-harness protocol in front of an
OpenAI-compatible chat endpoint, standard library only. It shows the whole contract a worker written
with any SDK must honour: one step of reasoning per request, text and tool calls streamed back,
nothing executed here - the HOST executes tools, persists events and gates confirmations.

  python3 worker.py --listen 127.0.0.1:18090 --upstream http://127.0.0.1:8080/v1 [--api-key-file F] [--token T]

POST /v1/turn (JSON, see adapters/providers/harnessworker) -> text/event-stream of JSON events:
  {"type":"text","delta":...} {"type":"reasoning","delta":...} {"type":"tool_call","id":...,"name":...,"arguments":...}
  {"type":"done","stopReason":...,"usage":{...}} {"type":"error","code":...,"message":...,"retryable":bool}
--token, when set, is the bearer the HOST must present. The worker never follows redirects and does
not read proxy settings from the environment.
"""
import argparse
import json
import sys
import urllib.error
import urllib.request
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

ARGS = None
OPENER = urllib.request.build_opener(urllib.request.ProxyHandler({}))


class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, *a, **k):
        return None


OPENER.add_handler(NoRedirect())


def to_openai(req):
    msgs = []
    if req.get("instructions"):
        msgs.append({"role": "system", "content": req["instructions"]})
    for m in req.get("messages", []):
        role = m.get("role")
        if role == "developer":
            msgs.append({"role": "system", "content": m.get("content", "")})
        elif role == "assistant":
            out = {"role": "assistant", "content": m.get("content") or None}
            if m.get("toolCalls"):
                out["tool_calls"] = [{"id": c["id"], "type": "function", "function": {"name": c["function"]["name"], "arguments": c["function"].get("arguments") or "{}"}} for c in m["toolCalls"]]
            msgs.append(out)
        elif role == "tool":
            content = m.get("content") or ""
            if not content and m.get("error"):
                content = "error: " + m["error"]
            msgs.append({"role": "tool", "tool_call_id": m.get("toolCallId"), "content": content})
        else:
            msgs.append({"role": role, "content": m.get("content", "")})
    body = {"model": req.get("model"), "messages": msgs, "stream": True, "stream_options": {"include_usage": True}}
    if req.get("tools"):
        body["tools"] = [{"type": "function", "function": {"name": t["name"], "description": t.get("description", ""), "parameters": t.get("parameters") or {"type": "object"}}} for t in req["tools"]]
    if req.get("temperature") is not None:
        body["temperature"] = req["temperature"]
    if req.get("maxOutputTokens"):
        body["max_tokens"] = req["maxOutputTokens"]
    return body


class Handler(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def log_message(self, *a):  # quiet
        pass

    def send_event(self, ev):
        self.wfile.write(("data: " + json.dumps(ev, separators=(",", ":")) + "\n\n").encode())
        self.wfile.flush()

    def do_POST(self):
        if self.path != "/v1/turn":
            self.send_error(404)
            return
        if ARGS.token and self.headers.get("Authorization") != "Bearer " + ARGS.token:
            self.send_error(401)
            return
        n = int(self.headers.get("Content-Length", "0"))
        try:
            req = json.loads(self.rfile.read(n))
        except Exception:
            self.send_error(400)
            return
        self.send_response(200)
        self.send_header("Content-Type", "text/event-stream")
        self.send_header("Cache-Control", "no-store")
        self.end_headers()
        headers = {"Content-Type": "application/json", "Accept": "text/event-stream"}
        if ARGS.api_key:
            headers["Authorization"] = "Bearer " + ARGS.api_key
        up = urllib.request.Request(ARGS.upstream.rstrip("/") + "/chat/completions", data=json.dumps(to_openai(req)).encode(), headers=headers, method="POST")
        calls = {}
        finish = None
        usage = None
        try:
            with OPENER.open(up, timeout=120) as resp:
                for raw in resp:
                    line = raw.decode("utf-8", "replace").strip()
                    if not line.startswith("data:"):
                        continue
                    data = line[5:].strip()
                    if data == "[DONE]":
                        break
                    chunk = json.loads(data)
                    if chunk.get("usage"):
                        usage = {"promptTokens": chunk["usage"].get("prompt_tokens", 0), "completionTokens": chunk["usage"].get("completion_tokens", 0)}
                    for ch in chunk.get("choices", []):
                        d = ch.get("delta") or {}
                        if d.get("content"):
                            self.send_event({"type": "text", "delta": d["content"]})
                        if d.get("reasoning_content"):
                            self.send_event({"type": "reasoning", "delta": d["reasoning_content"]})
                        for tc in d.get("tool_calls") or []:
                            idx = tc.get("index", 0)
                            cur = calls.setdefault(idx, {"id": "", "name": "", "arguments": ""})
                            cur["id"] = tc.get("id") or cur["id"]
                            fn = tc.get("function") or {}
                            cur["name"] = fn.get("name") or cur["name"]
                            cur["arguments"] += fn.get("arguments") or ""
                        if ch.get("finish_reason"):
                            finish = ch["finish_reason"]
        except urllib.error.HTTPError as e:
            self.send_event({"type": "error", "code": "upstream_%d" % e.code, "message": "upstream answered HTTP %d" % e.code, "retryable": e.code in (408, 429) or e.code >= 500})
            return
        except Exception as e:  # noqa: BLE001 - every failure becomes one protocol event
            self.send_event({"type": "error", "code": "upstream_unreachable", "message": str(e)[:200], "retryable": True})
            return
        for idx in sorted(calls):
            c = calls[idx]
            self.send_event({"type": "tool_call", "id": c["id"] or "call_%d" % idx, "name": c["name"], "arguments": c["arguments"] or "{}"})
        stop = {"stop": "end", "tool_calls": "tool_calls", "length": "length"}.get(finish, "tool_calls" if calls else "end")
        self.send_event({"type": "done", "stopReason": stop, "usage": usage or {"promptTokens": 0, "completionTokens": 0}})


def main():
    global ARGS
    p = argparse.ArgumentParser()
    p.add_argument("--listen", default="127.0.0.1:18090")
    p.add_argument("--upstream", required=True)
    p.add_argument("--api-key-file")
    p.add_argument("--token", default="")
    ARGS = p.parse_args()
    ARGS.api_key = open(ARGS.api_key_file).read().strip() if ARGS.api_key_file else ""
    host, port = ARGS.listen.rsplit(":", 1)
    srv = ThreadingHTTPServer((host, int(port)), Handler)
    print("listening on %s:%d" % srv.server_address, flush=True)
    try:
        srv.serve_forever()
    except KeyboardInterrupt:
        pass


if __name__ == "__main__":
    main()
