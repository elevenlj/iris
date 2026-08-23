#!/usr/bin/env node

const fs = require("fs");
const path = require("path");
const http = require("http");
const https = require("https");
const { spawn } = require("child_process");
const { pipeline } = require("stream/promises");
const { createWriteStream } = require("fs");

const packageJson = require("../package.json");

const owner = process.env.IRIS_GITHUB_OWNER || process.env.EASY_TERMINAL_GITHUB_OWNER || "elevenlj";
const repo = process.env.IRIS_GITHUB_REPO || process.env.EASY_TERMINAL_GITHUB_REPO || "iris";
const giteeRepo = process.env.IRIS_GITEE_REPO || process.env.EASY_TERMINAL_GITEE_REPO || "eleven_lj/iris";
const version = packageJson.version;
const platform = process.platform;
const arch = process.arch;
const requestTimeoutMs = Number(process.env.IRIS_DOWNLOAD_TIMEOUT_MS || process.env.EASY_TERMINAL_DOWNLOAD_TIMEOUT_MS || 120000);

const platformMap = {
  darwin: "darwin",
  linux: "linux",
  win32: "windows"
};

const archMap = {
  x64: "amd64",
  arm64: "arm64"
};

function fail(message) {
  console.error(`Iris install failed: ${message}`);
  process.exit(1);
}

if (process.env.IRIS_SKIP_DOWNLOAD === "1" || process.env.EASY_TERMINAL_SKIP_DOWNLOAD === "1") {
  process.exit(0);
}

const targetPlatform = platformMap[platform];
const targetArch = archMap[arch];

if (!targetPlatform || !targetArch) {
  fail(`unsupported platform: ${platform}/${arch}`);
}

const ext = targetPlatform === "windows" ? ".exe" : "";
const assetName = `iris-${targetPlatform}-${targetArch}${ext}`;
const urls = [
  `https://github.com/${owner}/${repo}/releases/download/v${version}/${assetName}`,
  `https://gitee.com/${giteeRepo}/releases/download/v${version}/${assetName}`
];
const vendorDir = path.resolve(__dirname, "..", "vendor");
const outPath = path.join(vendorDir, targetPlatform === "windows" ? "iris.exe" : "iris");

async function download(downloadUrl, redirects = 0) {
  if (redirects > 5) {
    throw new Error("too many redirects while downloading binary");
  }

  await fs.promises.mkdir(vendorDir, { recursive: true });

  await new Promise((resolve, reject) => {
    const mod = downloadUrl.startsWith("https:") ? https : http;
    const request = mod
      .get(downloadUrl, { headers: { "User-Agent": "iris-npm" } }, (res) => {
        if ([301, 302, 303, 307, 308].includes(res.statusCode || 0)) {
          res.resume();
          download(res.headers.location, redirects + 1).then(resolve, reject);
          return;
        }

        if (res.statusCode !== 200) {
          res.resume();
          reject(new Error(`download returned HTTP ${res.statusCode}`));
          return;
        }

        pipeline(res, createWriteStream(outPath)).then(resolve, reject);
      })
      .on("error", reject);
    request.setTimeout(requestTimeoutMs, () => {
      request.destroy(new Error(`download timed out after ${requestTimeoutMs}ms`));
    });
  });

  if (targetPlatform !== "windows") {
    await fs.promises.chmod(outPath, 0o755);
  }
}

async function downloadWithCurl(downloadUrl) {
  await fs.promises.mkdir(vendorDir, { recursive: true });

  await new Promise((resolve, reject) => {
    const timeoutSeconds = Math.max(1, Math.ceil(requestTimeoutMs / 1000));
    const child = spawn("curl", [
      "-fL",
      "--connect-timeout",
      "20",
      "--max-time",
      String(timeoutSeconds),
      "-H",
      "User-Agent: iris-npm",
      "-o",
      outPath,
      downloadUrl
    ], {
      stdio: ["ignore", "ignore", "pipe"]
    });

    let stderr = "";
    child.stderr.on("data", (chunk) => {
      stderr += chunk.toString();
    });
    child.on("error", reject);
    child.on("exit", (code) => {
      if (code === 0) {
        resolve();
        return;
      }
      reject(new Error((stderr.trim() || `curl exited with code ${code}`).split("\n").slice(-1)[0]));
    });
  });

  if (targetPlatform !== "windows") {
    await fs.promises.chmod(outPath, 0o755);
  }
}

async function installAgentHooks() {
  await new Promise((resolve, reject) => {
    const child = spawn(outPath, ["--install-agent-hooks"], {
      stdio: ["ignore", "ignore", "pipe"],
      env: process.env
    });
    let stderr = "";
    child.stderr.on("data", (chunk) => {
      stderr += chunk.toString();
    });
    child.on("error", reject);
    child.on("exit", (code) => {
      if (code === 0) {
        resolve();
        return;
      }
      reject(new Error((stderr.trim() || `hook installer exited with code ${code}`).split("\n").slice(-1)[0]));
    });
  });
}

async function main() {
  const failures = [];

  for (const url of urls) {
    try {
      console.log(`[iris] downloading ${assetName}`);
      console.log(`[iris] source: ${url}`);
      try {
        await downloadWithCurl(url);
      } catch (curlErr) {
        console.warn(`[iris] curl download failed, trying node downloader`);
        await download(url);
      }
      console.log(`[iris] installed binary to ${outPath}`);
      try {
        await installAgentHooks();
        console.log("[iris] installed Agent hooks and Feishu context skills");
      } catch (hookErr) {
        console.warn(`[iris] Agent integration setup deferred until first launch: ${hookErr.message}`);
      }
      return;
    } catch (err) {
      try {
        fs.rmSync(outPath, { force: true });
      } catch (_) {
      }
      failures.push(`${url}: ${err.message}`);
      console.warn(`[iris] download failed, trying next source`);
    }
  }

  fail(`could not download binary from any source.\n${failures.map((item) => `- ${item}`).join("\n")}`);
}

main();
