"""Optional: python3 tests/codex_paste_smoke.py. Uses local Codex, no real API."""
import fcntl
import gzip
import json
import os
import pty
import select
import struct
import subprocess
import tempfile
import termios
import threading
import time
from http.server import BaseHTTPRequestHandler, HTTPServer

requests = []


class Handler(BaseHTTPRequestHandler):
    def log_message(self, *args):
        pass

    def do_POST(self):
        body = self.rfile.read(int(self.headers.get("Content-Length", 0)))
        if self.headers.get("Content-Encoding") == "gzip":
            body = gzip.decompress(body)
        requests.append(json.loads(body))
        response = {"id": "resp_test", "object": "response", "status": "completed",
                    "output": [{"id": "msg_test", "type": "message", "role": "assistant",
                                "status": "completed", "content": [{"type": "output_text", "text": "OK", "annotations": []}]}],
                    "usage": {"input_tokens": 1, "output_tokens": 1, "total_tokens": 2}}
        event = {"type": "response.completed", "response": response}
        data = ("event: response.completed\ndata: " + json.dumps(event) + "\n\n").encode()
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
    env = {k: v for k, v in os.environ.items() if not k.startswith(("IRIS_", "EASY_TERMINAL_", "OPENAI_", "CODEX_"))}
    env.update(CODEX_HOME=work, TERM="xterm-256color")
    provider = 'model_providers.localtest={name="localtest",base_url="http://127.0.0.1:%d/v1",wire_api="responses",requires_openai_auth=false,request_max_retries=0}' % server.server_port
    proc = subprocess.Popen(["codex", "--no-alt-screen", "--sandbox", "read-only", "-a", "never",
                             "-c", 'model_provider="localtest"', "-c", provider,
                             "-c", "check_for_update_on_startup=false"],
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
        text = "IRIS paste boundary test 中文 multi-line\n" * 800 + "END-IRIS-PASTE"
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
        print(f"PASS: {len(text.encode())} UTF-8 bytes, no submission before Enter, one complete submission after Enter")
    finally:
        proc.terminate()
        proc.wait(timeout=5)
        os.close(master)
        server.shutdown()
        server.server_close()
