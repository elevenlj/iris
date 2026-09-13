import assert from "node:assert/strict";
import fs from "node:fs/promises";
import http from "node:http";
import os from "node:os";
import path from "node:path";
import { spawn } from "node:child_process";

const root = path.resolve(new URL("..", import.meta.url).pathname);
const iris = process.env.IRIS_TEST_BINARY || path.join(root, "iris");
const aiden = process.env.AIDEN_BIN || "aiden";
const tmp = await fs.mkdtemp(path.join(os.tmpdir(), "iris-aiden-hook-e2e-"));
const waiters = new Map();
const relaying = new Set();

const server = http.createServer(async (request, response) => {
  try {
    const chunks = [];
    for await (const chunk of request) chunks.push(chunk);
    const payload = JSON.parse(Buffer.concat(chunks).toString("utf8"));
    const match = request.url?.match(/^\/bots\/hook-test\/api\/sessions\/([^/]+)\/hook\/turn-ended$/);
    assert.ok(match, request.url);
    const queue = waiters.get(match[1]);
    // Native Aiden 2.0.2 uses its global hook even with --settings. Relay that
    // real event through the candidate binary without changing global config.
    if (match[1] === "aiden" && !payload.last_assistant_message) {
      assert.ok(!relaying.has(payload.session_id), "candidate did not enrich Aiden reply");
      relaying.add(payload.session_id);
      const result = await run(iris, ["--claude-stop"], {
        ...process.env,
        IRIS_API_URL: `http://127.0.0.1:${server.address().port}/bots/hook-test`,
        IRIS_SESSION_ID: match[1],
        IRIS_SESSION_TOKEN: `${match[1]}-token`,
      }, root, JSON.stringify(payload));
      assert.equal(result.code, 0, result.stderr);
      relaying.delete(payload.session_id);
      response.writeHead(200);
      response.end('{"accepted":true}');
      return;
    }
    assert.ok(queue?.length, `unexpected hook for ${match[1]}`);
    queue.shift().resolve(payload);
    if (queue.length === 0) waiters.delete(match[1]);
    response.writeHead(200, { "Content-Type": "application/json" });
    response.end('{"accepted":true}');
  } catch (error) {
    response.writeHead(500);
    response.end();
    for (const queue of waiters.values()) for (const waiter of queue) waiter.reject(error);
    waiters.clear();
  }
});

try {
  await new Promise((resolve, reject) => {
    server.once("error", reject);
    server.listen(0, "127.0.0.1", resolve);
  });
  const address = server.address();
  const settings = JSON.stringify({
    hooks: {
      Stop: [{ hooks: [{ type: "command", command: `${shellQuote(iris)} --claude-stop`, timeout: 5 }] }],
    },
  });
  const claudeHome = path.join(tmp, "claude_home");
  await fs.mkdir(claudeHome);
  await fs.writeFile(path.join(claudeHome, "settings.json"), settings);
  const claudeAuth = path.join(os.homedir(), ".claude.json");
  try {
    await fs.symlink(claudeAuth, path.join(claudeHome, ".claude.json"));
  } catch (error) {
    if (error.code !== "ENOENT") throw error;
  }

  const scenarios = [
    { mode: "native", name: "Aiden", prefix: [], flags: ["--permission-mode", "agentFull", "--no-tools"] },
    { mode: "codex", name: "Aiden X Codex", prefix: ["x", "codex", "--model", process.env.AIDEN_CODEX_MODEL || "gpt-5.6-sol"], flags: ["--dangerously-bypass-approvals-and-sandbox"] },
    { mode: "claude", name: "Aiden X Claude Code", prefix: ["x", "claude"], flags: ["--dangerously-skip-permissions"] },
  ].filter((scenario) => !process.env.AIDEN_E2E_MODE || scenario.mode === process.env.AIDEN_E2E_MODE);
  for (const scenario of scenarios) {
    const sessionKey = scenario.name.toLowerCase().replaceAll(" ", "-");
    const secret = `IRIS_${sessionKey.replaceAll("-", "_")}_SECRET`;
    const startMarker = `IRIS_${sessionKey.replaceAll("-", "_")}_START_OK`;
    const resumeMarker = `IRIS_${sessionKey.replaceAll("-", "_")}_RESUME_OK`;
    const env = {
      ...process.env,
      IRIS_API_URL: `http://127.0.0.1:${address.port}/bots/hook-test`,
      IRIS_SESSION_ID: sessionKey,
      IRIS_SESSION_TOKEN: `${sessionKey}-token`,
      ...(scenario.mode === "native" ? { AIDEN_USE_1X_AGENT: "1", CHECK_CLI_UPDATE: "false" } : {}),
      ...(scenario.mode === "codex" ? {} : { CLAUDE_CONFIG_DIR: claudeHome }),
    };

    const startHook = waitForHook(sessionKey);
    const start = await run(aiden, scenario.mode === "codex" ? [
      ...scenario.prefix, "exec", ...scenario.flags, `记住暗号 ${secret}，只回复 ${startMarker}。`,
    ] : [
      ...scenario.prefix, ...(scenario.mode === "native" ? [] : ["--print"]),
      ...scenario.flags, `记住暗号 ${secret}，只回复 ${startMarker}。`,
    ], env, scenario.mode === "native" ? tmp : root, undefined, scenario.mode === "native" ? startHook : undefined);
    assert.equal(start.code, 0, `${scenario.name} start failed\n${start.stderr}`);
    if (scenario.mode !== "native") assert.ok(start.stdout.includes(startMarker), start.stdout);
    const firstPayload = await withTimeout(startHook, `${scenario.name} start hook`);
    if (scenario.mode === "codex") assert.equal(firstPayload.type, "agent-turn-complete");
    else assert.equal(firstPayload.hook_event_name, "Stop");
    const firstSessionID = firstPayload.session_id || firstPayload["thread-id"];
    assert.ok(firstSessionID, JSON.stringify(firstPayload));
    assert.ok((firstPayload.last_assistant_message || firstPayload["last-assistant-message"]).includes(startMarker));

    const resumeHook = waitForHook(sessionKey);
    const resumed = await run(aiden, scenario.mode === "codex" ? [
      ...scenario.prefix, "exec", "resume", firstSessionID, ...scenario.flags,
      `回复上一轮记住的暗号，并追加 ${resumeMarker}。`,
    ] : [
      ...scenario.prefix, ...(scenario.mode === "native" ? [] : ["--print"]), "--resume", firstSessionID,
      ...scenario.flags, `回复上一轮记住的暗号，并追加 ${resumeMarker}。`,
    ], env, scenario.mode === "native" ? tmp : root, undefined, scenario.mode === "native" ? resumeHook : undefined);
    assert.equal(resumed.code, 0, `${scenario.name} resume failed\n${resumed.stderr}`);
    if (scenario.mode !== "native") assert.ok(resumed.stdout.includes(secret) && resumed.stdout.includes(resumeMarker), resumed.stdout);
    const secondPayload = await withTimeout(resumeHook, `${scenario.name} resume hook`);
    const secondSessionID = secondPayload.session_id || secondPayload["thread-id"];
    assert.ok(secondSessionID, JSON.stringify(secondPayload));
    assert.equal(secondSessionID, firstSessionID);
    assert.ok((secondPayload.last_assistant_message || secondPayload["last-assistant-message"]).includes(secret));
    assert.ok((secondPayload.last_assistant_message || secondPayload["last-assistant-message"]).includes(resumeMarker));
    console.log(`${scenario.name} start, completion hook, and resume ok`);
  }
} finally {
  waiters.clear();
  server.closeAllConnections?.();
  await new Promise((resolve) => server.close(resolve));
  await fs.rm(tmp, { recursive: true, force: true });
}

function waitForHook(sessionKey) {
  return new Promise((resolve, reject) => {
    const queue = waiters.get(sessionKey) || [];
    queue.push({ resolve, reject });
    waiters.set(sessionKey, queue);
  });
}

function withTimeout(promise, label) {
  return Promise.race([
    promise,
    new Promise((_, reject) => setTimeout(() => reject(new Error(`${label} timed out`)), 15000)),
  ]);
}

function shellQuote(value) {
  return `'${String(value).replaceAll("'", `'\\''`)}'`;
}

function run(command, args, env, cwd = root, input, stopAfter) {
  return new Promise((resolve, reject) => {
    // Native --print uses an in-memory checkpointer. Exercise the actual TUI
    // instead; this opt-in integration test requires a macOS terminal.
    if (stopAfter && !process.stdin.isTTY) return reject(new Error("Native Aiden integration test requires a terminal"));
    const child = spawn(stopAfter ? "/usr/bin/script" : command, stopAfter ? ["-q", "/dev/null", command, ...args] : args, { cwd, env, detached: Boolean(stopAfter), stdio: [stopAfter ? "inherit" : input === undefined ? "ignore" : "pipe", "pipe", "pipe"] });
    if (input !== undefined) child.stdin.end(input);
    let completed = false;
    if (stopAfter) stopAfter.then(() => { completed = true; process.kill(-child.pid, "SIGTERM"); }, () => process.kill(-child.pid, "SIGTERM"));
    let stdout = "";
    let stderr = "";
    const timeout = setTimeout(() => {
      if (stopAfter) process.kill(-child.pid, "SIGTERM");
      else child.kill("SIGTERM");
      reject(new Error(`${command} timed out\n${stderr}`));
    }, 180000);
    child.stdout.on("data", (chunk) => { stdout += chunk.toString(); });
    child.stderr.on("data", (chunk) => { stderr += chunk.toString(); });
    child.once("error", (error) => {
      clearTimeout(timeout);
      reject(error);
    });
    child.once("exit", (code) => {
      clearTimeout(timeout);
      resolve({ code: completed ? 0 : code, stdout, stderr });
    });
  });
}
