"""Optional: IRIS_SMOKE_AGENT=traecli (or aiden-codex) python3 tests/codex_paste_smoke.py.

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
is_aiden = agent == "aiden-codex"
requests = []
notifications = []
terminal_output = bytearray()


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
    env = {k: v for k, v in os.environ.items() if not k.startswith(("IRIS_", "EASY_TERMINAL_", "OPENAI_", "CODEX_", "TRAE_", "CLAUDE_", "AIDEN_"))}
    env.update(HOME=work, XDG_CONFIG_HOME=work, CODEX_HOME=work, TRAE_HOME=work, CLAUDE_CONFIG_DIR=work, TERM="xterm-256color")
    provider = 'model_providers.localtest={name="localtest",base_url="http://127.0.0.1:%d/v1",wire_api="responses",requires_openai_auth=false,request_max_retries=0}' % server.server_port
    notify = json.dumps([sys.executable, os.path.abspath(__file__), "--notify", f"http://127.0.0.1:{server.server_port}/notify"])
    if os.environ.get("IRIS_SMOKE_IRIS"):
        route_file = os.path.join(work, "notify-route.json")
        with open(route_file, "w") as route:
            json.dump(dict(url=f"http://127.0.0.1:{server.server_port}", session_id="smoke", token="smoke-token"), route)
        os.chmod(route_file, 0o600)
        notify = json.dumps([os.environ["IRIS_SMOKE_IRIS"], "--codex-notify", "--route-file", route_file])
        # A shared daemon can inherit a different terminal's route.
        env.update(IRIS_API_URL="http://127.0.0.1:1", IRIS_SESSION_ID="wrong", IRIS_SESSION_TOKEN="wrong")
    flags = ["--yolo"] if agent == "traecli" else ["--no-alt-screen", "--sandbox", "read-only", "-a", "never"]
    system_prompt = os.environ.get("IRIS_SMOKE_SYSTEM_PROMPT")
    if system_prompt and agent != "traecli" and not is_aiden:
        flags += ["-c", "developer_instructions=" + json.dumps(system_prompt)]
    launch = [agent, *flags,
              "-c", 'model_provider="localtest"', "-c", provider,
              "-c", "notify=" + notify, "-c", "check_for_update_on_startup=false"]
    if is_aiden:
        private_home = os.path.join(work, "private-codex")
        os.mkdir(private_home)
        os.mkdir(os.path.join(work, "sessions"))
        os.symlink(os.path.join(work, "sessions"), os.path.join(private_home, "sessions"))
        with open(os.path.join(private_home, "config.toml"), "w") as config:
            config.write("notify = " + notify + "\ncheck_for_update_on_startup = false\n")
        # Aiden's own test mode; this provider only serves the local mock API.
        env.update(SKIP_LOGIN="1", AIDEN_DISABLE_CODE_ADOPTION="1", AIDEN_DISABLE_CODE_ADOPTION_DAEMON="1")
        launch = ["aiden", "x", "codex", "--provider", "localtest", "--model", "gpt-5.4",
                  "--base-url", f"http://127.0.0.1:{server.server_port}/v1", "--api-key", "local-test",
                  "--env", "CODEX_HOME=" + private_home, *flags]
    proc = subprocess.Popen(launch,
                            cwd=work, env=env, stdin=slave, stdout=slave, stderr=slave)
    os.close(slave)

    def drain(seconds):
        end = time.monotonic() + seconds
        while time.monotonic() < end:
            if select.select([master], [], [], 0.03)[0]:
                chunk = os.read(master, 65536)
                terminal_output.extend(chunk)
                if b"\x1b[6n" in chunk:
                    os.write(master, b"\x1b[1;1R")

    try:
        drain(10 if is_aiden else 2)
        os.write(master, b"\r")  # Trust only this empty temporary directory.
        drain(2)
        text = sys.argv[1] if len(sys.argv) > 1 else "IRIS paste boundary test 中文 multi-line\n" * 800 + "END-IRIS-PASTE"
        if system_prompt and (agent == "traecli" or is_aiden):
            text = "【机器人指令】\n" + system_prompt + "\n\n【当前请求】\n" + text
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
        assert len(submitted) == 1, f"expected one complete submission, got {len(submitted)}: " + terminal_output[-4000:].decode(errors="replace")
        if system_prompt:
            assert system_prompt in json.dumps(submitted[0], ensure_ascii=False), "missing bot instructions"
        # Some CLIs also run a separate conversation-title task.
        completed = [n for n in notifications if n.get("input-messages") == [text]]
        assert len(completed) == 1, f"expected one user-turn completion, got {completed}"
        assert completed[0]["type"] == "agent-turn-complete", completed
        assert completed[0]["last-assistant-message"] == "OK", completed
        print(f"PASS: {agent}, {len(text.encode())} UTF-8 bytes, one complete submission after Enter, one final-reply notification")
        if system_prompt:
            # Resume the exact thread and change the bot configuration, not its history.
            thread_id = completed[0]["thread-id"]
            proc.terminate()
            proc.wait(timeout=5)
            os.close(master)
            master, slave = pty.openpty()
            fcntl.ioctl(slave, termios.TIOCSWINSZ, struct.pack("HHHH", 36, 120, 0, 0))
            updated_prompt = system_prompt + "_UPDATED"
            args = [agent, "resume", thread_id] + proc.args[1:]
            if is_aiden:
                args = proc.args[:3] + ["resume", thread_id] + proc.args[3:]
                # No private DB/cache: recovery must find an existing shared rollout.
                resume_home = os.path.join(work, "resume-codex")
                os.mkdir(resume_home)
                os.symlink(os.path.join(work, "sessions"), os.path.join(resume_home, "sessions"))
                with open(os.path.join(resume_home, "config.toml"), "w") as config:
                    config.write("notify = " + notify + "\ncheck_for_update_on_startup = false\n")
                args[args.index("--env") + 1] = "CODEX_HOME=" + resume_home
            elif agent != "traecli":
                args += ["-c", "developer_instructions=" + json.dumps(updated_prompt)]
            requests.clear()
            notifications.clear()
            terminal_output.clear()
            proc = subprocess.Popen(args, cwd=work, env=env, stdin=slave, stdout=slave, stderr=slave)
            os.close(slave)
            drain(10 if is_aiden else 4)
            if b"Continue without trusting" in terminal_output:
                # The mock-provider test does not need any lifecycle hooks.
                os.write(master, b"3\r")
                drain(2)
            elif is_aiden and b"Trust and continue" in terminal_output:
                os.write(master, b"\r")  # Only the empty temporary test workspace.
                drain(3)
            resume_text = "resume-check"
            if agent == "traecli" or is_aiden:
                resume_text = "【机器人指令】\n" + updated_prompt + "\n\n【当前请求】\n" + resume_text
            os.write(master, ("\x1b[200~" + resume_text + "\x1b[201~").encode())
            drain(0.5)
            os.write(master, b"\r")
            drain(5)
            assert requests, "resume did not submit: " + terminal_output[-3000:].decode(errors="replace")
            resumed = [r for r in requests if any(
                c.get("text") == resume_text for item in r.get("input", [])
                if isinstance(item, dict) and item.get("role") == "user"
                for c in item.get("content", []) if isinstance(c, dict))]
            assert resumed, f"resume input missing from {len(requests)} requests"
            assert updated_prompt in json.dumps(resumed[0], ensure_ascii=False), "resume retained stale developer instructions"
            assert any(n.get("last-assistant-message") == "OK" for n in notifications), "resume completion missing"
            print(f"PASS: {agent} resumed exact thread with updated bot instructions")
    finally:
        proc.terminate()
        proc.wait(timeout=5)
        os.close(master)
        server.shutdown()
        server.server_close()
