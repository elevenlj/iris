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
const tmp = await fs.mkdtemp(path.join(os.tmpdir(), "iris-codex-e2e-"));

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
        IRIS_E2E_DEBUG: "1",
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

    await initializeIrisForCodexE2E();

    await createSession("real-codex-tui-e2e");
    await waitFor(() => evalExpr("window.irisApp.state.active && window.irisApp.state.socket && window.irisApp.state.socket.readyState === WebSocket.OPEN"));
    await waitForTerminalSize();
    const bootSnapshot = await completeCodexStartup();
    assertNoMojibake(bootSnapshot);
    assert.equal(/login|sign in/i.test(bootSnapshot), false, "Codex CLI should be logged in before running this e2e test");

    await waitForCodexReady();
    await runCodexPromptNotificationContentE2E();

    const modelSnapshot = await openCodexModelMenu();
    assertNoMojibake(modelSnapshot);
    assert.ok(modelSnapshot.includes("Access legacy models"), "Codex model menu should render help text");
    assertNumberedMenuLines(modelSnapshot, [1, 2, 3]);
    assert.equal(hasCollapsedNumberedMenu(modelSnapshot, [1, 2]), false, "Codex model menu should not collapse numbered items into one visual line");

    await submitComposer("1");
    const reasoningSnapshot = await waitForTerminalSnapshot("Select Reasoning Level", 20000);
    assertNoMojibake(reasoningSnapshot);
    assert.ok(reasoningSnapshot.includes("Press enter to confirm") || reasoningSnapshot.includes("esc to go back"), "Codex reasoning menu should render footer text");
    assertNumberedMenuLines(reasoningSnapshot, [1, 2, 3]);
    assert.equal(hasCollapsedNumberedMenu(reasoningSnapshot, [1, 2]), false, "Codex reasoning menu should not collapse numbered items into one visual line");

    await deleteActiveSession();
    console.log("codex tui e2e ok");
  } finally {
    await cdp?.close().catch(() => {});
    await terminateProcess(chrome);
    await terminateProcess(server);
    await fs.rm(tmp, { recursive: true, force: true }).catch(() => {});
  }
}

async function initializeIrisForCodexE2E() {
  await waitFor(() => evalExpr("document.querySelector('#settings-access-dialog').open === true"));
  await evalExpr(`
    document.querySelector('#settings-access-password').value = 'codex-e2e-password';
    document.querySelector('#settings-access-form').requestSubmit();
    true
  `);
  await waitFor(() => evalExpr("document.querySelector('#settings-access-dialog').open !== true"));
  await evalExpr(`(async () => {
    const config = await fetch('/api/config').then((response) => response.json());
    config.agents = (config.agents || []).map((agent) => agent.id === 'codex'
      ? { ...agent, command: 'codex --dangerously-bypass-approvals-and-sandbox' }
      : agent);
    config.default_agent_id = 'codex';
    config.agent_kind = 'codex';
    config.agent_command = 'codex --dangerously-bypass-approvals-and-sandbox';
    config.onboarding_completed = true;
    const updated = await fetch('/api/config', {
      method: 'PATCH',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(config),
    }).then((response) => response.json());
    window.irisApp.state.config = updated;
    if (document.querySelector('#onboarding-dialog').open) {
      document.querySelector('#onboarding-dialog').close();
    }
    return true;
  })()`);
}

async function completeCodexStartup() {
  let snapshot = await waitForAnyTerminalSnapshot([
    "OpenAI Codex", "model:", "directory:", "Update available!", "Do you trust the contents of this directory?",
  ], 45000);
  for (let attempt = 0; attempt < 6; attempt++) {
    if (snapshot.includes("OpenAI Codex") || snapshot.includes("model:") || snapshot.includes("directory:")) {
      return snapshot;
    }
    if (snapshot.includes("Update available!")) {
      await submitComposer("2");
      snapshot = await waitForAnyTerminalSnapshot(["OpenAI Codex", "model:", "directory:", "Do you trust the contents of this directory?"], 30000);
      continue;
    }
    if (snapshot.includes("Do you trust the contents of this directory?")) {
      await submitComposer("1");
      snapshot = await waitForAnyTerminalSnapshot(["OpenAI Codex", "model:", "directory:"], 30000);
      continue;
    }
    return snapshot;
  }
  throw new Error("Codex startup did not finish after handling interactive prompts");
}

async function runCodexPromptNotificationContentE2E() {
  const firstPrompt = "请只回复由 IRIS、E2E、ALPHA 用下划线拼接后的字符串，不要解释。";
  const secondPrompt = "请只回复由 IRIS、E2E、BETA 用下划线拼接后的字符串，不要解释。";

  await submitComposer(firstPrompt);
  await waitForTerminalSnapshot("IRIS_E2E_ALPHA", 90000);
  const firstContent = await waitForCurrentRoundContent("IRIS_E2E_ALPHA", 15000);
  assertNoMojibake(firstContent);
  assert.equal(firstContent.includes(firstPrompt), false, "current-round content should start after the first input anchor");
  await waitForCodexReady();

  await submitComposer(secondPrompt);
  await waitForTerminalSnapshot("IRIS_E2E_BETA", 90000);
  const secondContent = await waitForCurrentRoundContent("IRIS_E2E_BETA", 15000);
  assertNoMojibake(secondContent);
  assert.equal(secondContent.includes(secondPrompt), false, "current-round content should start after the second input anchor");
  assert.equal(secondContent.includes(firstPrompt), false, "current-round content should not include the previous input");
  assert.equal(secondContent.includes("IRIS_E2E_ALPHA"), false, "current-round content should not include the previous answer");
  await waitForCodexReady();
}

async function createSession(name) {
  await evalExpr(`document.querySelector('#session-name').value = ${JSON.stringify(name)}; document.querySelector('#new-session').requestSubmit(); true`);
}

async function submitComposer(value) {
  await evalExpr(`document.querySelector('#composer-input').value = ${JSON.stringify(value)}; document.querySelector('#composer button').click(); true`);
}

async function waitForTerminalSnapshot(text, timeoutMs = 10000) {
  let snapshot = "";
  try {
    await waitFor(async () => {
      snapshot = await terminalSnapshot();
      return snapshot.includes(text);
    }, timeoutMs);
  } catch (err) {
    throw new Error(`timed out waiting for terminal text ${JSON.stringify(text)}; snapshot=${JSON.stringify(previewSnapshot(snapshot))}`, { cause: err });
  }
  return snapshot;
}

async function waitForAnyTerminalSnapshot(texts, timeoutMs = 10000) {
  let snapshot = "";
  try {
    await waitFor(async () => {
      snapshot = await terminalSnapshot();
      return texts.some((text) => snapshot.includes(text));
    }, timeoutMs);
  } catch (err) {
    throw new Error(`timed out waiting for any terminal text ${JSON.stringify(texts)}; snapshot=${JSON.stringify(previewSnapshot(snapshot))}`, { cause: err });
  }
  return snapshot;
}

async function terminalSnapshot() {
  return await evalExpr("window.irisApp.terminalVisibleSnapshot()") || "";
}

async function waitForCurrentRoundContent(text, timeoutMs = 10000, options = {}) {
  let content = "";
  try {
    await waitFor(async () => {
      content = (await currentRoundContent(options)).content || "";
      return content.includes(text);
    }, timeoutMs);
  } catch (err) {
    const diagnostics = await safeCurrentRoundDiagnostics(content);
    throw new Error(`timed out waiting for current-round content ${JSON.stringify(text)}; diagnostics=${JSON.stringify(diagnostics)}`, { cause: err });
  }
  return content;
}

async function currentRoundContent(options = {}) {
  const sessionID = await evalExpr("window.irisApp.state.active");
  const query = options.fresh === false ? "?fresh=0" : "";
  return fetchJSON(`http://localhost:${port}/api/sessions/${encodeURIComponent(sessionID)}/current-round${query}`);
}

async function waitForCodexReady() {
  await waitFor(async () => {
    const snapshot = await terminalSnapshot();
    return /gpt-|model:|directory:/.test(snapshot) &&
      !/task is in progress|Working \(/i.test(snapshot);
  }, 20000);
  await new Promise((resolve) => setTimeout(resolve, 2500));
}

async function openCodexModelMenu() {
  let lastSnapshot = "";
  for (let attempt = 0; attempt < 4; attempt++) {
    await submitComposer("/model");
    try {
      return await waitForTerminalSnapshot("Select Model and Effort", 8000);
    } catch {
      lastSnapshot = await terminalSnapshot();
      if (!/task is in progress|Working \(/i.test(lastSnapshot)) {
        break;
      }
      await new Promise((resolve) => setTimeout(resolve, 3000));
    }
  }
  throw new Error(`Codex model menu did not open; snapshot=${JSON.stringify(previewSnapshot(lastSnapshot))}`);
}

async function waitForTerminalSize() {
  let size = null;
  await waitFor(async () => {
    size = await evalExpr("({ cols: window.irisApp.state.term?.cols || 0, rows: window.irisApp.state.term?.rows || 0 })");
    return size.cols >= 80 && size.rows >= 20;
  }, 8000);
  return size;
}

async function deleteActiveSession() {
  const sessions = await fetchJSON(`http://localhost:${port}/api/sessions`);
  for (const session of sessions) {
    await fetch(`http://localhost:${port}/api/sessions/${session.id}`, { method: "DELETE" });
  }
}

function assertNoMojibake(snapshot) {
  assert.equal(snapshot.includes("\uFFFD"), false, "terminal snapshot should not contain replacement characters");
}

function previewSnapshot(snapshot) {
  return String(snapshot || "").split(/\r?\n/).slice(-30).join("\n").slice(-4000);
}

function assertNumberedMenuLines(snapshot, numbers) {
  const lines = snapshot.split(/\r?\n/).map((line) => line.trimEnd());
  for (const number of numbers) {
    assert.ok(lines.some((line) => new RegExp(`^\\s*(?:›\\s*)?${number}\\.`).test(line)), `Codex menu should render ${number}. as its own visual line`);
  }
}

function hasCollapsedNumberedMenu(snapshot, numbers) {
  const lines = snapshot.split(/\r?\n/);
  return lines.some((line) => {
    const normalized = line.replace(/\s+/g, " ");
    return numbers.every((number) => normalized.includes(`${number}.`));
  });
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

async function safeCurrentRoundDiagnostics(content) {
  const sessionID = await evalExpr("window.irisApp?.state?.active || ''").catch(() => "");
  const snapshot = await evalExpr("window.irisApp?.terminalSnapshotMetadata?.() || null").catch(() => null);
  let session = null;
  try {
    const sessions = await fetchJSON(`http://localhost:${port}/api/sessions`);
    const current = sessions.find((item) => item.id === sessionID);
    if (current) session = { id: current.id, status: current.status, live: Boolean(current.live) };
  } catch {}
  const value = String(content || "");
  return {
    session,
    snapshot,
    content_length: value.length,
    content_line_count: value ? value.split(/\r?\n/).length : 0,
  };
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
