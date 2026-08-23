import assert from "node:assert/strict";
import crypto from "node:crypto";
import fs from "node:fs/promises";
import http from "node:http";
import net from "node:net";
import os from "node:os";
import path from "node:path";
import { spawn } from "node:child_process";

const root = path.resolve(new URL("..", import.meta.url).pathname);
const bin = path.join(root, "iris");
const port = process.env.E2E_PORT ? Number(process.env.E2E_PORT) : await freePort();
const chromePort = process.env.E2E_CHROME_PORT ? Number(process.env.E2E_CHROME_PORT) : await freePort();
const tmp = await fs.mkdtemp(path.join(os.tmpdir(), "iris-browser-e2e-"));

let server;
let chrome;
let cdp;

async function main() {
try {
  await fs.mkdir(path.join(tmp, "data", "uploads"), { recursive: true });
  await fs.mkdir(path.join(tmp, "log"), { recursive: true });
  server = spawn(bin, {
    cwd: tmp,
    env: {
      ...process.env,
      PORT: String(port),
      IRIS_HOME: tmp,
      TERMINAL_WORKING_DIR: root,
      AGENT_MONITOR_DB: path.join(tmp, "iris.db"),
      AGENT_MONITOR_UPLOADS_DIR: path.join(tmp, "data", "uploads"),
      AGENT_MONITOR_LOG_DIR: path.join(tmp, "log"),
    },
    stdio: ["ignore", "pipe", "pipe"],
  });
  pipeLogs(server, "server");
  await waitForHTTP(`http://localhost:${port}/api/sessions`);

  chrome = spawn("/Applications/Google Chrome.app/Contents/MacOS/Google Chrome", [
    "--headless=new",
    "--disable-gpu",
    "--no-first-run",
    "--no-default-browser-check",
    `--remote-debugging-port=${chromePort}`,
    `--user-data-dir=${path.join(tmp, "chrome-profile")}`,
    "about:blank",
  ], { stdio: ["ignore", "pipe", "pipe"] });
  pipeLogs(chrome, "chrome");
  await waitForHTTP(`http://localhost:${chromePort}/json/version`);

  const pageInfo = await singlePageTarget();
  cdp = await CDPClient.connect(pageInfo.webSocketDebuggerUrl);
  await cdp.send("Page.enable");
  await cdp.send("Runtime.enable");
  await cdp.send("Page.navigate", { url: `http://localhost:${port}` });
  await waitFor(() => evalExpr("document.readyState === 'complete' || document.readyState === 'interactive'"));
  await waitFor(() => evalExpr("Boolean(window.irisApp && document.querySelector('#session-name'))"));

  await initializeIrisForE2E();

  await createSession("browser-e2e");
  await waitFor(() => evalExpr("document.querySelectorAll('.session').length === 1"));
  await waitFor(() => evalExpr("document.querySelector('.session').className.includes('session-running')"));
  await waitFor(() => evalExpr("window.irisApp.state.active && window.irisApp.state.socket && window.irisApp.state.socket.readyState === WebSocket.OPEN"));
  const browserTerminalSize = await waitForTerminalSize();
  assert.ok(browserTerminalSize.cols >= 80 && browserTerminalSize.rows >= 20, "browser should fit terminal to the visible shell");
  assert.equal(await evalExpr("window.irisApp.standardTerminal.cols"), 120, "browser should use the standard terminal width");
  assert.equal(await evalExpr("window.irisApp.standardTerminal.rows"), 36, "browser should use the standard terminal height");
  await waitFor(() => fetchJSON(`http://localhost:${port}/api/sessions`).then((sessions) => sessions[0]?.status === "waiting"), 7000);
  await waitFor(() => evalExpr("document.querySelector('.session').className.includes('session-waiting')"), 7000);

  await clickNotify();
  const notifyResponse = await fetchJSON(`http://localhost:${port}/api/sessions`);
  assert.equal(notifyResponse[0].notify_on_waiting, true, "notification toggle should PATCH notify_on_waiting=true");

  await fillComposer("stty size");
  await click("document.querySelector('#composer button').click()");
  await waitForOutput(`${browserTerminalSize.rows} ${browserTerminalSize.cols}`);

  await fillComposer("echo BROWSER_BUTTON_E2E");
  await click("document.querySelector('#composer button').click()");
  await waitForOutput("BROWSER_BUTTON_E2E");
  await waitForTerminalSnapshot("BROWSER_BUTTON_E2E");

  await fillComposer("printf '中文快照_OK\\n'");
  await click("document.querySelector('#composer button').click()");
  await waitForOutput("中文快照_OK");
  const cjkSnapshot = await waitForTerminalSnapshot("中文快照_OK");
  assert.equal(cjkSnapshot.includes("\uFFFD"), false, "terminal snapshot should not contain replacement characters");

  await fillComposer("for i in $(seq 1 50); do printf 'FULL_BUFFER_E2E_%02d\\n' \"$i\"; done");
  await click("document.querySelector('#composer button').click()");
  await waitForOutput("FULL_BUFFER_E2E_50");
  const fullBufferSnapshot = await waitForTerminalSnapshot("FULL_BUFFER_E2E_01");
  assert.ok(fullBufferSnapshot.includes("FULL_BUFFER_E2E_50"), "terminal snapshot should include full scrollback output");

  const activeSessionID = await evalExpr("window.irisApp.state.active");
  await cdp.send("Page.navigate", { url: `http://localhost:${port}/?session=${encodeURIComponent(activeSessionID)}` });
  await waitFor(() => evalExpr("Boolean(window.irisApp && document.querySelector('#session-name'))"));
  await waitFor(() => evalExpr(`window.irisApp.state.active === ${JSON.stringify(activeSessionID)}`));
  await waitFor(() => evalExpr("window.irisApp.state.socket && window.irisApp.state.socket.readyState === WebSocket.OPEN"));
  const reconnectedSnapshot = await waitForTerminalSnapshot("中文快照_OK");
  assert.ok(reconnectedSnapshot.includes("BROWSER_BUTTON_E2E"), "browser reconnect should keep earlier terminal history");

  await fillComposer("plain enter line");
  await keydownComposer({ key: "Enter", metaKey: false, ctrlKey: false });
  assert.equal(await evalExpr("document.querySelector('#composer-input').value"), "plain enter line", "plain Enter should not send");

  await fillComposer("echo BROWSER_CMD_ENTER_E2E");
  await keydownComposer({ key: "Enter", metaKey: true, ctrlKey: false });
  await waitForOutput("BROWSER_CMD_ENTER_E2E");

  await runTUILikeSnapshotE2E();

  await pasteImageIntoTerminal();
  // The paste handler intentionally inserts the uploaded path without
  // submitting it. Execute the pending shell line so the assertion observes
  // deterministic PTY output instead of depending on terminal-driver echo.
  await evalExpr("window.irisApp.queueTerminalInput('\\r').then(() => true)");
  await waitForOutput("/data/uploads/");
  await waitForOutput(".png");

  await openQuickDialogAndAdd("pwd");
  assert.equal(await evalExpr("document.querySelectorAll('.quick-chip').length"), 1, "quick command chip should be added");
  assert.equal(await evalExpr("document.querySelector('.quick-chip span').textContent"), "pwd", "quick command chip should display command text");

  const headlessTarget = await createSessionViaAPI("headless-target");
  await cdp.send("Page.navigate", { url: `http://localhost:${port}/?session=${encodeURIComponent(headlessTarget.id)}` });
  await waitFor(() => evalExpr("Boolean(window.irisApp && document.querySelector('#session-name'))"));
  await waitFor(() => evalExpr(`window.irisApp.state.active === ${JSON.stringify(headlessTarget.id)}`));
  await waitFor(() => evalExpr("window.irisApp.state.socket && window.irisApp.state.socket.readyState === WebSocket.OPEN"));

  await deleteActiveSession();
  assert.deepEqual(await fetchJSON(`http://localhost:${port}/api/sessions`), [], "test session should be cleaned up");
  console.log("browser e2e ok");
} finally {
  await cdp?.close().catch(() => {});
  await terminateProcess(chrome);
  await terminateProcess(server);
  await fs.rm(tmp, { recursive: true, force: true }).catch(() => {});
}

async function initializeIrisForE2E() {
  await waitFor(() => evalExpr("document.querySelector('#settings-access-dialog').open === true"));
  await evalExpr(`
    document.querySelector('#settings-access-password').value = 'browser-e2e-password';
    document.querySelector('#settings-access-form').requestSubmit();
    true
  `);
  await waitFor(() => evalExpr("document.querySelector('#settings-access-dialog').open !== true"));
  await evalExpr(`(async () => {
    const config = await fetch('/api/config').then((response) => response.json());
    config.agent_kind = 'custom';
    config.agent_command = 'bash --noprofile --norc';
    config.onboarding_completed = true;
    const updated = await fetch('/api/config', {
      method: 'PATCH',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(config),
    }).then((response) => response.json());
    window.irisApp.state.config = updated;
    document.querySelector('#onboarding-dialog').close();
    return true;
  })()`);
}
}

async function createSession(name) {
  await evalExpr(`document.querySelector('#session-name').value = ${JSON.stringify(name)}; document.querySelector('#new-session').requestSubmit(); true`);
}

async function createSessionViaAPI(name) {
  const res = await fetch(`http://localhost:${port}/api/sessions`, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ name }),
  });
  if (!res.ok) throw new Error(`create session returned ${res.status}`);
  return res.json();
}

async function clickNotify() {
  await waitFor(() => evalExpr("Boolean(document.querySelector('.notify-input'))"));
  await click("document.querySelector('.notify-input').click()");
  await waitFor(() => evalExpr("document.querySelector('.notify-input').checked === true"));
}

async function fillComposer(value) {
  await evalExpr(`document.querySelector('#composer-input').value = ${JSON.stringify(value)}; true`);
}

async function keydownComposer(event) {
  await evalExpr(`
    document.querySelector('#composer-input').dispatchEvent(new KeyboardEvent('keydown', {
      key: ${JSON.stringify(event.key)},
      metaKey: ${Boolean(event.metaKey)},
      ctrlKey: ${Boolean(event.ctrlKey)},
      bubbles: true,
      cancelable: true
    }));
    true
  `);
}

async function runTUILikeSnapshotE2E() {
  const screen1 = [
    "\x1b[2J\x1b[3J\x1b[H",
    "Codex Mock TUI\n",
    "• Working (1s • esc to interrupt)\n",
  ].join("");
  const screen2 = [
    "\x1b[2J\x1b[3J\x1b[H",
    "Select Model and Effort\n",
    "Access legacy models by running codex -m <model_name> or in your config.toml\n",
    "› 1. gpt-5.5 (current)   Frontier model for complex coding, research, and real-world work.\n",
    "  2. gpt-5.4             Strong model for everyday coding.\n",
    "  3. gpt-5.4-mini        Small, fast, and cost-efficient model for simpler coding tasks.\n",
    "Press enter to confirm or esc to go back\n",
    "TUI_FINAL_READY\n",
  ].join("");
  const script = [
    `process.stdout.write(${JSON.stringify(screen1)})`,
    `setTimeout(() => process.stdout.write(${JSON.stringify(screen2)}), 120)`,
  ].join("; ");
  await fillComposer(`node -e ${shellSingleQuote(script)}`);
  await click("document.querySelector('#composer button').click()");
  const snapshot = await waitForTerminalLine("TUI_FINAL_READY");
  assertVisibleLinesInOrder(snapshot, [
    "Select Model and Effort",
    "Access legacy models by running codex -m <model_name> or in your config.toml",
    "› 1. gpt-5.5 (current)   Frontier model for complex coding, research, and real-world work.",
    "  2. gpt-5.4             Strong model for everyday coding.",
    "  3. gpt-5.4-mini        Small, fast, and cost-efficient model for simpler coding tasks.",
    "Press enter to confirm or esc to go back",
    "TUI_FINAL_READY",
  ]);
  assert.equal(snapshot.includes("Working (1s"), false, "TUI snapshot should use the final repaint, not stale transient text");
}

async function waitForTerminalLine(expected, timeoutMs = 10000) {
  let snapshot = "";
  await waitFor(async () => {
    snapshot = await evalExpr("window.irisApp.terminalVisibleSnapshot()") || "";
    return snapshot.split(/\r?\n/).some((line) => line.trimEnd() === expected);
  }, timeoutMs);
  return snapshot;
}

function shellSingleQuote(text) {
  return "'" + String(text).replace(/'/g, "'\\''") + "'";
}

function assertVisibleLinesInOrder(snapshot, expectedLines) {
  const lines = snapshot.split(/\r?\n/).map((line) => line.trimEnd());
  let cursor = 0;
  for (const expected of expectedLines) {
    const index = lines.findIndex((line, i) => i >= cursor && line === expected);
    assert.notEqual(index, -1, `terminal snapshot should contain visual line: ${expected}; snapshot=${JSON.stringify(snapshot.slice(-5000))}`);
    cursor = index + 1;
  }
}

async function pasteImageIntoTerminal() {
  await evalExpr(`
    (() => {
      const png = Uint8Array.from([
        0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a,
        0x00, 0x00, 0x00, 0x0d, 0x49, 0x48, 0x44, 0x52,
        0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x01,
        0x08, 0x06, 0x00, 0x00, 0x00, 0x1f, 0x15, 0xc4,
        0x89, 0x00, 0x00, 0x00, 0x0a, 0x49, 0x44, 0x41,
        0x54, 0x78, 0x9c, 0x63, 0x00, 0x01, 0x00, 0x00,
        0x05, 0x00, 0x01, 0x0d, 0x0a, 0x2d, 0xb4, 0x00,
        0x00, 0x00, 0x00, 0x49, 0x45, 0x4e, 0x44, 0xae,
        0x42, 0x60, 0x82
      ]);
      const file = new File([png], "paste.png", { type: "image/png" });
      const data = new DataTransfer();
      data.items.add(file);
      document.querySelector("#terminal").dispatchEvent(new ClipboardEvent("paste", {
        clipboardData: data,
        bubbles: true,
        cancelable: true
      }));
      return true;
    })()
  `);
}

async function waitForOutput(text) {
  await waitFor(() => fetchJSON(`http://localhost:${port}/api/sessions`).then((sessions) => {
    const live = sessions.find((session) => session.live);
    if (!live) return false;
    return fetchJSON(`http://localhost:${port}/api/sessions/${live.id}/output`).then((out) => out.content.includes(text));
  }), 8000);
}

async function waitForTerminalSnapshot(text) {
  let snapshot = "";
  await waitFor(async () => {
    snapshot = await evalExpr("window.irisApp.terminalVisibleSnapshot()");
    return typeof snapshot === "string" && snapshot.includes(text);
  }, 8000);
  return snapshot;
}

async function waitForTerminalSize() {
  let size = null;
  await waitFor(async () => {
    size = await evalExpr("({ cols: window.irisApp.state.term?.cols || 0, rows: window.irisApp.state.term?.rows || 0 })");
    return size.cols >= 80 && size.rows >= 20;
  }, 8000);
  return size;
}

async function openQuickDialogAndAdd(text) {
  await click("document.querySelector('.add-quick').click()");
  await waitFor(() => evalExpr("document.querySelector('#quick-dialog').open === true"));
  await evalExpr(`
    document.querySelector('#quick-text').value = ${JSON.stringify(text)};
    document.querySelector('#quick-form').requestSubmit();
    true
  `);
  await waitFor(() => evalExpr("document.querySelectorAll('.quick-chip').length === 1"));
}

async function deleteActiveSession() {
  const sessions = await fetchJSON(`http://localhost:${port}/api/sessions`);
  for (const session of sessions) {
    await fetch(`http://localhost:${port}/api/sessions/${session.id}`, { method: "DELETE" });
  }
}

async function click(script) {
  await evalExpr(`${script}; true`);
}

async function evalExpr(expression) {
  const result = await cdp.send("Runtime.evaluate", {
    expression,
    awaitPromise: true,
    returnByValue: true,
  });
  if (result.exceptionDetails) {
    throw new Error(result.exceptionDetails.text || "Runtime.evaluate failed");
  }
  return result.result?.value;
}

async function singlePageTarget() {
  let pages = [];
  await waitFor(async () => {
    const targets = await fetchJSON(`http://localhost:${chromePort}/json/list`);
    pages = targets.filter((target) => target.type === "page" && target.webSocketDebuggerUrl);
    return pages.length > 0;
  });
  const [primary, ...extras] = pages;
  for (const extra of extras) {
    await fetch(`http://localhost:${chromePort}/json/close/${encodeURIComponent(extra.id)}`, { method: "PUT" }).catch(() => {});
  }
  return primary;
}

async function fetchJSON(url, options) {
  const res = await fetch(url, options);
  if (!res.ok) throw new Error(`${url} returned ${res.status}`);
  return res.json();
}

async function freePort() {
  return new Promise((resolve, reject) => {
    const srv = net.createServer();
    srv.once("error", reject);
    srv.listen(0, "127.0.0.1", () => {
      const address = srv.address();
      const portNumber = typeof address === "object" && address ? address.port : 0;
      srv.close(() => resolve(portNumber));
    });
  });
}

async function waitForHTTP(url, timeoutMs = 10000) {
  await waitFor(async () => {
    try {
      const res = await fetch(url);
      return res.ok;
    } catch {
      return false;
    }
  }, timeoutMs);
}

async function waitFor(fn, timeoutMs = 10000) {
  const started = Date.now();
  let lastError;
  while (Date.now() - started < timeoutMs) {
    try {
      if (await fn()) return;
    } catch (err) {
      lastError = err;
    }
    await new Promise((resolve) => setTimeout(resolve, 100));
  }
  throw lastError || new Error("timed out waiting for condition");
}

function pipeLogs(proc, name) {
  proc.stdout?.on("data", (chunk) => {
    if (process.env.E2E_VERBOSE) process.stdout.write(`[${name}] ${chunk}`);
  });
  proc.stderr?.on("data", (chunk) => {
    if (process.env.E2E_VERBOSE) process.stderr.write(`[${name}] ${chunk}`);
  });
}

async function terminateProcess(proc) {
  if (!proc) return;
  if (proc.exitCode === null && proc.signalCode === null) {
    proc.kill("SIGTERM");
    if (!(await waitForProcessExit(proc, 3000))) {
      proc.kill("SIGKILL");
      await waitForProcessExit(proc, 3000);
    }
  }
  proc.stdout?.destroy();
  proc.stderr?.destroy();
}

function waitForProcessExit(proc, timeoutMs) {
  if (proc.exitCode !== null || proc.signalCode !== null) {
    return Promise.resolve(true);
  }
  return new Promise((resolve) => {
    const timer = setTimeout(() => {
      proc.off("exit", onExit);
      resolve(false);
    }, timeoutMs);
    const onExit = () => {
      clearTimeout(timer);
      resolve(true);
    };
    proc.once("exit", onExit);
  });
}

class CDPClient {
  constructor(socket, pathName, host) {
    this.socket = socket;
    this.pathName = pathName;
    this.host = host;
    this.nextID = 1;
    this.pending = new Map();
    this.buffer = Buffer.alloc(0);
    this.connected = false;
    socket.on("data", (chunk) => this.onData(chunk));
    socket.on("error", (err) => {
      for (const { reject } of this.pending.values()) reject(err);
      this.pending.clear();
    });
  }

  static async connect(wsURL) {
    const url = new URL(wsURL);
    const socket = net.connect(Number(url.port), url.hostname);
    const client = new CDPClient(socket, `${url.pathname}${url.search}`, url.host);
    await client.handshake();
    return client;
  }

  handshake() {
    return new Promise((resolve, reject) => {
      const key = crypto.randomBytes(16).toString("base64");
      const req = [
        `GET ${this.pathName} HTTP/1.1`,
        `Host: ${this.host}`,
        "Upgrade: websocket",
        "Connection: Upgrade",
        `Sec-WebSocket-Key: ${key}`,
        "Sec-WebSocket-Version: 13",
        "\r\n",
      ].join("\r\n");
      const onData = (chunk) => {
        this.buffer = Buffer.concat([this.buffer, chunk]);
        const marker = this.buffer.indexOf("\r\n\r\n");
        if (marker === -1) return;
        const head = this.buffer.slice(0, marker).toString("utf8");
        this.buffer = this.buffer.slice(marker + 4);
        this.socket.off("data", onData);
        if (!head.includes(" 101 ")) {
          reject(new Error(`CDP websocket handshake failed: ${head}`));
          return;
        }
        this.connected = true;
        if (this.buffer.length) this.onData(Buffer.alloc(0));
        resolve();
      };
      this.socket.on("data", onData);
      this.socket.once("error", reject);
      this.socket.write(req);
    });
  }

  send(method, params = {}) {
    const id = this.nextID++;
    const payload = JSON.stringify({ id, method, params });
    this.socket.write(encodeFrame(payload));
    return new Promise((resolve, reject) => {
      this.pending.set(id, { resolve, reject });
    });
  }

  onData(chunk) {
    if (!this.connected) return;
    this.buffer = Buffer.concat([this.buffer, chunk]);
    for (;;) {
      const frame = decodeFrame(this.buffer);
      if (!frame) return;
      this.buffer = this.buffer.slice(frame.bytes);
      if (frame.opcode === 8) return;
      if (frame.opcode !== 1) continue;
      const msg = JSON.parse(frame.payload.toString("utf8"));
      if (msg.id && this.pending.has(msg.id)) {
        const pending = this.pending.get(msg.id);
        this.pending.delete(msg.id);
        if (msg.error) pending.reject(new Error(JSON.stringify(msg.error)));
        else pending.resolve(msg.result);
      }
    }
  }

  async close() {
    this.socket.end();
    this.socket.destroy();
  }
}

function encodeFrame(text) {
  const payload = Buffer.from(text);
  const mask = crypto.randomBytes(4);
  let header;
  if (payload.length < 126) {
    header = Buffer.from([0x81, 0x80 | payload.length]);
  } else if (payload.length < 65536) {
    header = Buffer.alloc(4);
    header[0] = 0x81;
    header[1] = 0x80 | 126;
    header.writeUInt16BE(payload.length, 2);
  } else {
    header = Buffer.alloc(10);
    header[0] = 0x81;
    header[1] = 0x80 | 127;
    header.writeBigUInt64BE(BigInt(payload.length), 2);
  }
  const masked = Buffer.alloc(payload.length);
  for (let i = 0; i < payload.length; i++) masked[i] = payload[i] ^ mask[i % 4];
  return Buffer.concat([header, mask, masked]);
}

function decodeFrame(buffer) {
  if (buffer.length < 2) return null;
  const opcode = buffer[0] & 0x0f;
  let length = buffer[1] & 0x7f;
  let offset = 2;
  if (length === 126) {
    if (buffer.length < 4) return null;
    length = buffer.readUInt16BE(2);
    offset = 4;
  } else if (length === 127) {
    if (buffer.length < 10) return null;
    length = Number(buffer.readBigUInt64BE(2));
    offset = 10;
  }
  const masked = (buffer[1] & 0x80) !== 0;
  let mask;
  if (masked) {
    if (buffer.length < offset + 4) return null;
    mask = buffer.slice(offset, offset + 4);
    offset += 4;
  }
  if (buffer.length < offset + length) return null;
  const payload = Buffer.from(buffer.slice(offset, offset + length));
  if (masked) {
    for (let i = 0; i < payload.length; i++) payload[i] ^= mask[i % 4];
  }
  return { opcode, payload, bytes: offset + length };
}

await main();
