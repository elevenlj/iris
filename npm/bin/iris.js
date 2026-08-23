#!/usr/bin/env node

const fs = require("fs");
const path = require("path");
const { spawn } = require("child_process");

const exeName = process.platform === "win32" ? "iris.exe" : "iris";
const configuredBinary = process.env.IRIS_BINARY || process.env.EASY_TERMINAL_BINARY;
const binaryPath = configuredBinary
  ? path.resolve(configuredBinary)
  : path.resolve(__dirname, "..", "vendor", exeName);

if (!fs.existsSync(binaryPath)) {
  console.error("Iris binary is missing. Reinstall the package or set IRIS_BINARY to a local binary.");
  process.exit(1);
}

const child = spawn(binaryPath, process.argv.slice(2), {
  stdio: "inherit",
  env: process.env
});

child.on("exit", (code, signal) => {
  if (signal) {
    process.kill(process.pid, signal);
    return;
  }
  process.exit(code ?? 0);
});

child.on("error", (err) => {
  console.error(err.message);
  process.exit(1);
});
