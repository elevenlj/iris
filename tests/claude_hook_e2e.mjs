import assert from "node:assert/strict";
import fs from "node:fs/promises";
import http from "node:http";
import os from "node:os";
import path from "node:path";
import { spawn } from "node:child_process";

const root = path.resolve(new URL("..", import.meta.url).pathname);
const iris = path.join(root, "iris");
const claude = process.env.CLAUDE_BIN || "claude";
const tmp = await fs.mkdtemp(path.join(os.tmpdir(), "iris-claude-hook-e2e-"));
const irisSessionID = "claude-hook-e2e";
const hookToken = "claude-hook-e2e-token";
const marker = "CLAUDE_HOOK_E2E_OK";

let callbackResolve;
let callbackReject;
const callback = new Promise((resolve, reject) => {
  callbackResolve = resolve;
  callbackReject = reject;
});

const server = http.createServer(async (request, response) => {
  try {
    assert.equal(request.method, "POST");
    assert.equal(request.url, `/api/sessions/${irisSessionID}/hook/turn-ended`);
    assert.equal(request.headers["x-iris-agent-token"], hookToken);
    const chunks = [];
    for await (const chunk of request) chunks.push(chunk);
    const payload = JSON.parse(Buffer.concat(chunks).toString("utf8"));
    assert.equal(payload.hook_event_name, "Stop");
    assert.match(payload.session_id, /^[0-9a-f-]{36}$/i);
    assert.ok(payload.last_assistant_message.includes(marker), JSON.stringify(payload));
    response.writeHead(200, { "Content-Type": "application/json" });
    response.end('{"accepted":true}');
    callbackResolve(payload);
  } catch (error) {
    response.writeHead(500);
    response.end();
    callbackReject(error);
  }
});

try {
  await new Promise((resolve, reject) => {
    server.once("error", reject);
    server.listen(0, "127.0.0.1", resolve);
  });
  const address = server.address();
  const settingsPath = path.join(tmp, "settings.json");
  await fs.writeFile(settingsPath, JSON.stringify({
    hooks: {
      Stop: [{
        hooks: [{
          type: "command",
          command: `${shellQuote(iris)} --claude-stop`,
          timeout: 5,
        }],
      }],
    },
  }, null, 2));

  const result = await run(claude, [
    "-p",
    `只回复 ${marker}，不要添加其他内容。`,
    "--settings",
    settingsPath,
    "--output-format",
    "text",
  ], {
    ...process.env,
    IRIS_API_URL: `http://127.0.0.1:${address.port}`,
    IRIS_SESSION_ID: irisSessionID,
    IRIS_SESSION_TOKEN: hookToken,
  });
  assert.equal(result.code, 0, result.stderr);
  assert.ok(result.stdout.includes(marker), result.stdout);
  await Promise.race([
    callback,
    new Promise((_, reject) => setTimeout(() => reject(new Error("Claude Stop hook callback timed out")), 10000)),
  ]);
  console.log("claude hook e2e ok");
} finally {
  await new Promise((resolve) => server.close(resolve));
  await fs.rm(tmp, { recursive: true, force: true });
}

function shellQuote(value) {
  return `'${String(value).replaceAll("'", `'\\''`)}'`;
}

function run(command, args, env) {
  return new Promise((resolve, reject) => {
    const child = spawn(command, args, {
      cwd: root,
      env,
      stdio: ["ignore", "pipe", "pipe"],
    });
    let stdout = "";
    let stderr = "";
    const timeout = setTimeout(() => {
      child.kill("SIGTERM");
      reject(new Error(`Claude E2E timed out\n${stderr}`));
    }, 120000);
    child.stdout.on("data", (chunk) => { stdout += chunk.toString(); });
    child.stderr.on("data", (chunk) => { stderr += chunk.toString(); });
    child.once("error", (error) => {
      clearTimeout(timeout);
      reject(error);
    });
    child.once("exit", (code) => {
      clearTimeout(timeout);
      resolve({ code, stdout, stderr });
    });
  });
}
