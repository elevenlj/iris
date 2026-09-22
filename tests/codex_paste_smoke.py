"""Optional: IRIS_SMOKE_AGENT=traecli python3 tests/codex_paste_smoke.py.

Defaults to Codex. Uses an isolated home and local API; no real API requests.
"""
import fcntl
import gzip
import json
import os
import pty
import select
import struct
import subprocess
import sys
import tempfile
import termios
import threading
import time
import urllib.request
from http.server import BaseHTTPRequestHandler, HTTPServer

if len(sys.argv) > 1 and sys.argv[1] == "--notify":
    urllib.request.urlopen(urllib.request.Request(sys.argv[2], data=sys.argv[3].encode())).close()
    sys.exit(0)

agent = os.environ.get("IRIS_SMOKE_AGENT", "codex")
requests = []
notifications = []


class Handler(BaseHTTPRequestHandler):
    def log_message(self, *args):
        pass

    def do_POST(self):
        body = self.rfile.read(int(self.headers.get("Content-Length", 0)))
        if self.path in ("/notify", "/api/sessions/smoke/hook/turn-ended"):
            if self.path != "/notify":
                assert self.headers.get("X-Iris-Agent-Token") == "smoke-token"
            notifications.append(json.loads(body))
            self.send_response(200)
            self.end_headers()
            return
        if self.headers.get("Content-Encoding") == "gzip":
            body = gzip.decompress(body)
        requests.append(json.loads(body))
        response = {"id": "resp_test", "object": "response", "status": "completed",
                    "output": [{"id": "msg_test", "type": "message", "role": "assistant",
                                "status": "completed", "content": [{"type": "output_text", "text": "OK", "annotations": []}]}],
                    "usage": {"input_tokens": 1, "output_tokens": 1, "total_tokens": 2}}
        events = [
            {"type": "response.output_item.done", "output_index": 0, "item": response["output"][0]},
            {"type": "response.completed", "response": response},
        ]
        data = "".join("event: " + event["type"] + "\ndata: " + json.dumps(event) + "\n\n" for event in events).encode()
        self.send_response(200)
        self.send_header("Content-Type", "text/event-stream")
        self.send_header("Content-Length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)


server = HTTPServer(("127.0.0.1", 0), Handler)
threading.Thread(target=server.serve_forever, daemon=True).start()
with tempfile.TemporaryDirectory(prefix="iris-paste-check-") as work:
    master, slave = pty.openpty()
    fcntl.ioctl(slave, termios.TIOCSWINSZ, struct.pack("HHHH", 36, 120, 0, 0))
    env = {k: v for k, v in os.environ.items() if not k.startswith(("IRIS_", "EASY_TERMINAL_", "OPENAI_", "CODEX_", "TRAE_"))}
    env.update(CODEX_HOME=work, TRAE_HOME=work, TERM="xterm-256color")
    provider = 'model_providers.localtest={name="localtest",base_url="http://127.0.0.1:%d/v1",wire_api="responses",requires_openai_auth=false,request_max_retries=0}' % server.server_port
    notify = json.dumps([sys.executable, os.path.abspath(__file__), "--notify", f"http://127.0.0.1:{server.server_port}/notify"])
    if os.environ.get("IRIS_SMOKE_IRIS"):
        notify = json.dumps([os.environ["IRIS_SMOKE_IRIS"], "--codex-notify"])
        env.update(IRIS_API_URL=f"http://127.0.0.1:{server.server_port}", IRIS_SESSION_ID="smoke", IRIS_SESSION_TOKEN="smoke-token")
    flags = ["--yolo"] if agent == "traecli" else ["--no-alt-screen", "--sandbox", "read-only", "-a", "never"]
    proc = subprocess.Popen([agent, *flags,
                             "-c", 'model_provider="localtest"', "-c", provider,
                             "-c", "notify=" + notify, "-c", "check_for_update_on_startup=false"],
                            cwd=work, env=env, stdin=slave, stdout=slave, stderr=slave)
    os.close(slave)

    def drain(seconds):
        end = time.monotonic() + seconds
        while time.monotonic() < end:
            if select.select([master], [], [], 0.03)[0]:
                chunk = os.read(master, 65536)
                if b"\x1b[6n" in chunk:
                    os.write(master, b"\x1b[1;1R")

    try:
        drain(2)
        os.write(master, b"\r")  # Trust only this empty temporary directory.
        drain(2)
        text = sys.argv[1] if len(sys.argv) > 1 else "IRIS paste boundary test 中文 multi-line\n" * 800 + "END-IRIS-PASTE"
        payload = ("\x1b[200~" + text + "\x1b[201~").encode()
        pos = 0
        while pos < len(payload):
            pos += os.write(master, payload[pos:])
        drain(0.5)
        assert not requests, "submitted before Enter"
        os.write(master, b"\r")
        drain(5)
        submitted = [r for r in requests if any(
            c.get("text") == text for item in r.get("input", [])
            if isinstance(item, dict) and item.get("role") == "user"
            for c in item.get("content", []) if isinstance(c, dict))]
        assert len(submitted) == 1, f"expected one complete submission, got {len(submitted)}"
        # Some CLIs also run a separate conversation-title task.
        completed = [n for n in notifications if n.get("input-messages") == [text]]
        assert len(completed) == 1, f"expected one user-turn completion, got {completed}"
        assert completed[0]["type"] == "agent-turn-complete", completed
        assert completed[0]["last-assistant-message"] == "OK", completed
        print(f"PASS: {agent}, {len(text.encode())} UTF-8 bytes, one complete submission after Enter, one final-reply notification")
    finally:
        proc.terminate()
        proc.wait(timeout=5)
        os.close(master)
        server.shutdown()
        server.server_close()
