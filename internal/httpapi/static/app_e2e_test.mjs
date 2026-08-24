import assert from "node:assert/strict";
import fs from "node:fs";
import vm from "node:vm";

class FakeElement {
  constructor(id = "", tag = "div") {
    this.id = id;
    this.tag = tag;
    this.children = [];
    this.value = "";
    this.checked = false;
    this.readOnly = false;
    this.textContent = "";
    this.className = "";
    this.title = "";
    this.type = "";
    this.dataset = {};
    this.style = {};
    this.onclick = null;
    this.onchange = null;
    this.oninput = null;
    this.onkeydown = null;
    this.onsubmit = null;
    this.parent = null;
    this._bySelector = new Map();
    this._listeners = new Map();
    this._hidden = false;
    this.classList = {
      add: (name) => {
        const classes = new Set(this.className.split(/\s+/).filter(Boolean));
        classes.add(name);
        if (name === "hidden") this.hidden = true;
        this.className = [...classes].join(" ");
      },
      remove: (name) => {
        const classes = new Set(this.className.split(/\s+/).filter(Boolean));
        classes.delete(name);
        if (name === "hidden") this.hidden = false;
        this.className = [...classes].join(" ");
      },
      toggle: (name, force) => {
        const classes = new Set(this.className.split(/\s+/).filter(Boolean));
      const enabled = force === undefined ? !classes.has(name) : Boolean(force);
        if (enabled) {
          classes.add(name);
          if (name === "hidden") this.hidden = true;
        } else {
          classes.delete(name);
          if (name === "hidden") this.hidden = false;
        }
        this.className = [...classes].join(" ");
        return enabled;
      },
    };
  }

  set innerHTML(value) {
    this._innerHTML = value;
    this.children = [];
    this._bySelector = new Map();
    if (value.includes("notify-input")) {
      for (const selector of [
        ".session-name",
        ".notify-input",
        ".notify-state",
        ".delete-btn",
        ".notify-row",
        ".start-btn",
      ]) {
        const child = new FakeElement("", selector === ".notify-input" ? "input" : "div");
        child.parent = this;
        this._bySelector.set(selector, child);
      }
      this._bySelector.get(".notify-input").type = "checkbox";
    }
    if (value.includes("chip-close")) {
      const span = new FakeElement("", "span");
      const close = new FakeElement("", "button");
      this._bySelector.set("span", span);
      this._bySelector.set(".chip-close", close);
    }
    if (value.includes("<strong")) {
      this._bySelector.set("strong", new FakeElement("", "strong"));
      this._bySelector.set("span", new FakeElement("", "span"));
    }
    if (value.includes("<code")) {
      this._bySelector.set("code", new FakeElement("", "code"));
    }
    if (value.includes("preset-edit")) {
      this._bySelector.set(".preset-edit", new FakeElement("", "button"));
      this._bySelector.set(".preset-delete", new FakeElement("", "button"));
    }
    if (value.includes("preset-command-input")) {
      const input = new FakeElement("", "input");
      input.className = "preset-command-input";
      const remove = new FakeElement("", "button");
      remove.className = "preset-command-remove";
      this._bySelector.set(".preset-command-input", input);
      this._bySelector.set(".preset-command-remove", remove);
    }
  }

  get innerHTML() {
    return this._innerHTML || "";
  }

  set hidden(value) {
    this._hidden = Boolean(value);
  }

  get hidden() {
    return Boolean(this._hidden);
  }

  querySelector(selector) {
    return this._bySelector.get(selector) || this.querySelectorAll(selector)[0] || null;
  }

  querySelectorAll(selector) {
    if (selector === "span") return this.children.filter((child) => child.tag === "span");
    const out = [];
    const visit = (node) => {
      if (selector.startsWith(".") && node.className.split(/\s+/).includes(selector.slice(1))) out.push(node);
      for (const child of node.children || []) visit(child);
      for (const child of node._bySelector?.values?.() || []) visit(child);
    };
    visit(this);
    return out;
  }

  appendChild(child) {
    child.parent = this;
    this.children.push(child);
    return child;
  }

  remove() {
    if (!this.parent) return;
    const index = this.parent.children.indexOf(this);
    if (index >= 0) this.parent.children.splice(index, 1);
    this.parent = null;
  }

  focus() {
    this.focused = true;
  }

  select() {
    this.selected = true;
  }

  setSelectionRange(start, end) {
    this.selectionRange = [start, end];
  }

  clear() {
    this.cleared = true;
  }

  getBoundingClientRect() {
    return this.rect || { left: 0, width: 8 };
  }

  requestSubmit() {
    this.onsubmit?.({ preventDefault() {} });
  }

  showModal() {
    this.open = true;
  }

  close() {
    this.open = false;
  }

  addEventListener(type, listener) {
    const listeners = this._listeners.get(type) || [];
    listeners.push(listener);
    this._listeners.set(type, listeners);
  }

  async dispatchEvent(event) {
    event.target ||= this;
    for (const listener of this._listeners.get(event.type) || []) {
      await listener(event);
    }
  }
}

const ids = [
  "sessions",
  "quick-list",
  "composer-input",
  "composer",
  "new-session",
  "session-name",
  "session-search",
  "quick-form",
  "quick-text",
  "quick-dialog",
  "quick-cancel",
  "onboarding-dialog",
  "onboarding-default-session-name",
  "onboarding-agent-preset",
  "onboarding-agent-custom-name",
  "onboarding-agent-custom-command",
  "onboarding-custom-agent-fields",
  "onboarding-agent-status",
  "onboarding-config",
  "onboarding-later",
  "settings-security-warning",
  "settings-current-password",
  "settings-new-password",
  "settings-confirm-password",
  "settings-password-format",
  "settings-password-match",
  "settings-password-save",
  "settings-password-status",
  "environment-check-start",
  "environment-check-result",
  "config-open",
  "config-dialog",
  "config-form",
  "config-cancel",
  "config-save",
  "config-prev",
  "config-next",
  "config-error",
  "help-open",
  "help-dialog",
  "help-close",
  "lark-register-start",
  "lark-register-panel",
  "lark-register-status",
  "lark-register-code",
  "lark-register-link",
  "lark-register-qr",
  "lark-app-console-link",
  "lark-copy-contact-scope",
  "lark-copy-group-scope",
  "lark-permission-status",
  "lark-test-start",
  "lark-test-result",
  "cfg-fast-waiting",
  "cfg-conservative-waiting",
  "cfg-auto-refresh-interval",
  "cfg-headless-snapshot-timeout",
  "cfg-lark-max-lines",
  "cfg-lark-fallback-tail-lines",
  "cfg-lark-merge-wrapped-lines",
  "cfg-lark-app-id",
  "cfg-lark-app-secret",
  "cfg-lark-receive-id",
  "cfg-lark-default-session-name",
  "cfg-lark-session-chat-prefix",
  "cfg-lark-ignore-prefix",
  "cfg-lark-auto-summary-prompt",
  "cfg-lark-mention-enabled",
  "cfg-prestart-command",
  "cfg-drop-patterns",
  "drop-rule-list",
  "drop-rule-add",
  "cfg-lark-custom-shortcuts",
  "custom-shortcut-list",
  "custom-shortcut-add",
  "cfg-session-name-presets",
  "cfg-session-start-presets",
  "cfg-agent-preset",
  "cfg-agent-custom-name",
  "cfg-agent-custom-command",
  "cfg-custom-agent-fields",
  "cfg-default-workspace-dir",
  "cfg-workspace-options",
  "workspace-option-list",
  "workspace-option-add",
  "agent-preset-status",
  "preset-session-name",
  "preset-save",
  "preset-clear",
  "preset-status",
  "preset-list",
  "start-preset-code",
  "start-preset-save",
  "start-preset-clear",
  "start-preset-status",
  "start-preset-list",
  "prestart-command-list",
  "prestart-command-add",
  "startup-json-toggle",
  "startup-json-preview",
  "active-title",
  "terminal",
];
const elements = Object.fromEntries(ids.map((id) => [id, new FakeElement(id)]));
elements["lark-register-panel"].hidden = true;
elements["startup-json-preview"].hidden = true;
const helpTabs = ["help-start", "help-terminal"].map((targetID, index) => {
  const tab = new FakeElement("", "button");
  tab.dataset.helpTarget = targetID;
  tab.className = index === 0 ? "help-tab active" : "help-tab";
  return tab;
});
const configTabs = ["config-lark", "config-session", "config-security", "config-workspaces"].map((targetID, index) => {
  const tab = new FakeElement("", "button");
  tab.dataset.configTarget = targetID;
  tab.className = index === 0 ? "config-tab active" : "config-tab";
  return tab;
});
const configPanels = ["config-lark", "config-session", "config-security", "config-workspaces"].map((id, index) => {
  const panel = new FakeElement(id, "section");
  panel.className = index === 0 ? "config-panel active" : "config-panel";
  return panel;
});
const helpPanels = ["help-start", "help-terminal"].map((id, index) => {
  const panel = new FakeElement(id, "section");
  panel.className = index === 0 ? "help-panel active" : "help-panel";
  return panel;
});
let terminalDOMRows = [];

function terminalRow(segments) {
  const row = new FakeElement("", "div");
  row.rect = { left: 0, width: 960 };
  row.children = segments.map(([text, left]) => {
    const span = new FakeElement("", "span");
    span.textContent = text;
    span.rect = { left, width: text.length * 8 };
    return span;
  });
  row.textContent = segments.map(([text]) => text).join("");
  return row;
}

const fetchCalls = [];
const sentMessages = [];
const localStorageData = new Map();

function withoutSnapshotRenderMetadata(message) {
  const copy = { ...message };
  delete copy.render_epoch;
  delete copy.buffer_type;
  delete copy.buffer_at_capacity;
  delete copy.anchor_guard_active;
  delete copy.continuity_version;
  delete copy.anchor_guard_line;
  delete copy.cursor_line;
  return copy;
}

function assertSnapshotRenderMetadata(message, bufferType = "unknown", atCapacity = false, guardActive = false, guardLine = -1, cursorLine = undefined) {
  assert.equal(message.continuity_version, 2);
  assert.ok(Number.isInteger(message.render_epoch) && message.render_epoch > 0, "snapshot should carry a positive render epoch");
  assert.equal(message.buffer_type, bufferType);
  assert.equal(message.buffer_at_capacity, atCapacity);
  assert.equal(message.anchor_guard_active, guardActive);
  assert.equal(message.anchor_guard_line, guardLine);
  assert.ok(Number.isInteger(message.cursor_line), "snapshot should carry a cursor line identity");
  if (cursorLine !== undefined) assert.equal(message.cursor_line, cursorLine);
}

class FakeWebSocket {
  static OPEN = 1;
}

const context = {
  console,
  setInterval() {},
  clearTimeout,
  setTimeout,
  requestAnimationFrame(callback) {
    return setTimeout(callback, 0);
  },
  TextDecoder,
  URLSearchParams,
  FormData: class {
    append() {}
  },
  WebSocket: FakeWebSocket,
  Terminal: class {
    constructor() {
      this.cols = 120;
      this.rows = 36;
    }
    loadAddon() {}
    open() {}
    onData() {}
    dispose() {}
    write(_text, callback) {
      callback?.();
    }
    clear() {}
    get buffer() {
      return { active: { length: 0, viewportY: 0, getLine() { return null; } } };
    }
  },
  FitAddon: { FitAddon: class { fit() {} } },
  location: { protocol: "http:", host: "localhost:8080" },
  document: {
    body: new FakeElement("body", "body"),
    getElementById(id) {
      return elements[id];
    },
    createElement(tag) {
      return new FakeElement("", tag);
    },
    querySelectorAll(selector) {
      if (selector === "#terminal .xterm-rows > div") return terminalDOMRows;
      if (selector === ".help-tab") return helpTabs;
      if (selector === ".help-panel") return helpPanels;
      if (selector === ".config-tab") return configTabs;
      if (selector === ".config-panel") return configPanels;
      return [];
    },
    execCommand(command) {
      if (command !== "copy") return false;
      context.copiedText = this.body.children.at(-1)?.value || "";
      return true;
    },
    addEventListener() {},
  },
  window: {
    addEventListener() {},
    removeEventListener() {},
    localStorage: {
      getItem(key) {
        return localStorageData.get(key) || null;
      },
      setItem(key, value) {
        localStorageData.set(key, String(value));
      },
    },
  },
  navigator: {
    clipboard: {
      async writeText(text) {
        context.clipboardWriteCount++;
        context.copiedText = text;
      },
    },
  },
  copiedText: "",
  clipboardWriteCount: 0,
  fetch: async (path, options = {}) => {
    fetchCalls.push({ path, options });
    if (path === "/api/sessions" && !options.method) {
      return jsonResponse([]);
    }
    if (path === "/api/quick-commands" && !options.method) {
      return jsonResponse([]);
    }
    if (path === "/api/settings/security/status" && !options.method) {
      return jsonResponse({ configured: true, skipped: false, authenticated: true, onboarding_required: true });
    }
    if (path === "/api/config" && !options.method) {
      return jsonResponse({
        fast_waiting_transition_ms: 300,
        conservative_waiting_transition_ms: 700,
        lark_auto_refresh_interval_ms: 5000,
        headless_snapshot_timeout_ms: 10000,
        lark_notify_max_lines: 300,
        lark_notify_fallback_tail_lines: 100,
        lark_notify_merge_wrapped_lines: false,
        lark_app_id: "app-id",
        lark_app_secret: "secret",
        lark_notify_receive_id: "ou_1",
        lark_mention_enabled: true,
        lark_default_session_name: "默认会话",
        lark_session_chat_prefix: "ET · ",
        lark_ignore_message_prefix: "/i",
        lark_auto_summary_prompt: "总结上一轮输出",
        onboarding_completed: false,
        session_pre_start_command: "",
        lark_notify_drop_line_patterns: [],
        lark_custom_shortcuts: [],
        session_name_presets: {},
        session_start_presets: {},
        agent_kind: "codex",
        agent_name: "Codex",
        agent_command: "codex --dangerously-bypass-approvals-and-sandbox",
        workspace_options: [],
      });
    }
    if (path === "/api/config" && options.method === "PATCH") {
      return jsonResponse(JSON.parse(options.body));
    }
    if (path === "/api/lark-app-registration" && options.method === "POST") {
      return jsonResponse({
        device_code: "dev-1",
        user_code: "USER-1",
        verification_uri_complete: "https://open.feishu.cn/page/cli?user_code=USER-1",
        expires_in: 3600,
        interval: 5,
        brand: "feishu",
      });
    }
    if (path === "/api/config/lark-test" && options.method === "POST") {
      return jsonResponse({
        ok: true,
        steps: [
          { name: "配置完整性", ok: true, message: "必填项已填写" },
          { name: "发送测试通知", ok: true, message: "已发送" },
        ],
      });
    }
    if (path === "/api/environment-check" && options.method === "POST") {
      return jsonResponse({
        ok: false,
        checked_at: "2026-08-23T08:00:00Z",
        steps: [
          { id: "node", name: "Node.js", status: "ok", message: "运行正常（v22.0.0）" },
          { id: "headless_browser", name: "Headless 浏览器", status: "error", message: "未找到 Chrome、Chromium 或 Edge" },
          { id: "feishu_app", name: "飞书应用", status: "warning", message: "尚未完成飞书应用配置" },
        ],
      });
    }
    if (path === "/api/sessions/sess-1/uploads" && options.method === "POST") {
      return jsonResponse({ path: "/tmp/iris-test/paste.png" }, 201);
    }
    return jsonResponse({}, 200);
  },
};
context.window.window = context.window;

function jsonResponse(data, status = 200) {
  return {
    ok: status >= 200 && status < 300,
    status,
    json: async () => data,
  };
}

vm.createContext(context);
vm.runInContext(fs.readFileSync(new URL("./app.js", import.meta.url), "utf8"), context);
await Promise.resolve();
await Promise.resolve();

const app = context.window.irisApp;

const originalSetTimeout = context.setTimeout;
const originalClearTimeout = context.clearTimeout;
const originalRequestAnimationFrame = context.requestAnimationFrame;
let delayedPaintCallback = null;
context.requestAnimationFrame = (callback) => {
  delayedPaintCallback = callback;
  return 1;
};
let paintResolutions = 0;
const delayedPaintStarted = Date.now();
await context.waitForNextPaint().then(() => { paintResolutions++; });
assert.ok(Date.now() - delayedPaintStarted >= 35, "a throttled animation frame should use the bounded timer fallback");
assert.equal(paintResolutions, 1);
delayedPaintCallback?.();
await Promise.resolve();
assert.equal(paintResolutions, 1, "a late animation frame must not resolve the paint wait twice");

let clearedPaintTimers = 0;
context.setTimeout = (callback, delay) => originalSetTimeout(callback, delay);
context.clearTimeout = (timer) => {
  clearedPaintTimers++;
  originalClearTimeout(timer);
};
context.requestAnimationFrame = (callback) => originalSetTimeout(callback, 0);
await context.waitForNextPaint();
assert.equal(clearedPaintTimers, 1, "a foreground animation frame should clear its fallback timer");
context.setTimeout = originalSetTimeout;
context.clearTimeout = originalClearTimeout;
context.requestAnimationFrame = originalRequestAnimationFrame;
assert.ok(app, "app test API is exposed");
assert.equal(app.standardTerminal.cols, 120);
assert.equal(app.standardTerminal.rows, 36);
assert.equal(app.standardTerminal.fontFamily, "Menlo, Consolas, monospace");
assert.equal(app.terminalWebSocketURL("sess-1"), "ws://localhost:8080/api/sessions/sess-1/ws");
context.location.search = "?session=sess-1&headless=1";
assert.equal(app.terminalWebSocketURL("sess-1"), "ws://localhost:8080/api/sessions/sess-1/ws?headless=1");
context.location.search = "";
assert.equal(app.standardTerminal.fontSize, 13);
assert.equal(app.standardTerminal.lineHeight, 1.2);

await app.loadConfig();
await app.maybeShowOnboarding();
assert.equal(elements["onboarding-dialog"].open, true, "first visit should require an Agent choice");
elements["onboarding-agent-preset"].value = "codex";
await elements["onboarding-config"].onclick();
let onboardingPatch = fetchCalls.find((call) => call.path === "/api/config" && call.options.method === "PATCH");
assert.ok(onboardingPatch, "onboarding should PATCH config");
let onboardingConfig = JSON.parse(onboardingPatch.options.body);
assert.equal(onboardingConfig.onboarding_completed, true, "onboarding should be marked as completed in config");
assert.equal(onboardingConfig.agent_kind, "codex");
assert.equal(onboardingConfig.lark_default_session_name, "默认会话");
await app.openConfigDialog();
assert.ok(configTabs[0].className.includes("active"), "settings should start from Feishu tab");
assert.equal(elements["config-prev"].disabled, true, "previous should be disabled on first config tab");
assert.equal(elements["config-next"].disabled, false, "next should be enabled on first config tab");
elements["config-next"].onclick();
assert.ok(configTabs[1].className.includes("active"), "next should move to the next config tab");
assert.equal(elements["config-prev"].disabled, false, "previous should be enabled after moving forward");
elements["config-prev"].onclick();
assert.ok(configTabs[0].className.includes("active"), "previous should move back to Feishu config tab");
elements["config-next"].onclick();
elements["config-next"].onclick();
elements["config-next"].onclick();
assert.ok(configTabs[3].className.includes("active"), "next should stop at the last config tab");
assert.equal(elements["config-next"].disabled, true, "next should be disabled on last config tab");
await app.openConfigDialog("config-security");
elements["settings-current-password"].value = "old-password";
elements["settings-new-password"].value = "new-password";
elements["settings-confirm-password"].value = "different-password";
elements["settings-confirm-password"].oninput();
assert.match(elements["settings-password-match"].textContent, /不一致/, "password confirmation should show a live mismatch hint");
await elements["settings-password-save"].onclick();
assert.equal(fetchCalls.some((call) => call.path === "/api/settings/security/password"), false, "mismatched passwords must not be submitted");
elements["settings-confirm-password"].value = "new-password";
elements["settings-confirm-password"].oninput();
assert.match(elements["settings-password-match"].textContent, /一致/, "matching passwords should show a live success hint");
await elements["settings-password-save"].onclick();
const passwordChangeCall = fetchCalls.find((call) => call.path === "/api/settings/security/password");
assert.deepEqual(JSON.parse(passwordChangeCall.options.body), {
  current_password: "old-password",
  new_password: "new-password",
  confirm_password: "new-password",
}, "password change should send an explicit confirmation");
elements["config-dialog"].close();
await app.maybeShowOnboarding();
assert.notEqual(elements["onboarding-dialog"].open, true, "completed onboarding should not reopen");

app.state.active = "sess-1";
app.state.socket = {
  readyState: FakeWebSocket.OPEN,
  send(payload) {
    sentMessages.push(JSON.parse(payload));
  },
};
app.state.term = {
  cols: 160,
  rows: 48,
  resize(cols, rows) {
    this.cols = cols;
    this.rows = rows;
  },
};
app.resizeTerm();
assert.deepEqual(sentMessages.pop(), { type: "resize", cols: 160, rows: 48 });
app.state.fit = {
  fit() {
    app.state.term.resize(132, 40);
  },
};
app.resizeTerm();
assert.deepEqual(sentMessages.pop(), { type: "resize", cols: 132, rows: 40 });
app.state.fit = null;
sentMessages.length = 0;
context.location.search = "?headless=1";
app.state.term = {
  cols: 120,
  rows: 36,
  resize(cols, rows) {
    this.cols = cols;
    this.rows = rows;
  },
};
app.state.fit = {
  fit() {
    app.state.term.resize(140, 42);
  },
};
app.resizeTerm();
assert.equal(sentMessages.length, 0, "headless terminal should not send resize");
assert.equal(app.state.term.cols, 120, "headless terminal should keep fixed cols");
assert.equal(app.state.term.rows, 36, "headless terminal should keep fixed rows");
app.syncHeadlessTerminalSize(150, 44);
assert.equal(sentMessages.length, 0, "headless terminal size sync should not send resize");
assert.equal(app.state.term.cols, 150, "headless terminal should follow backend cols");
assert.equal(app.state.term.rows, 44, "headless terminal should follow backend rows");
context.location.search = "";
app.state.fit = null;

elements["composer-input"].value = "echo button";
elements.composer.requestSubmit();
assert.deepEqual(sentMessages.pop(), { type: "submit", data: "echo button" });
assert.equal(elements["composer-input"].value, "");

app.state.term = {
  cols: 120,
  rows: 2,
  buffer: {
    active: {
      length: 4,
      viewportY: 1,
      getLine(index) {
        const values = ["old hidden", "visible one", "visible two", "new hidden"];
        return { isWrapped: false, translateToString: () => values[index] };
      },
    },
  },
};
app.state.pendingTerminalWrite = Promise.resolve();
await app.syncSnapshotNow();
let snapshotMessage = sentMessages.pop();
assertSnapshotRenderMetadata(snapshotMessage);
assert.deepEqual(withoutSnapshotRenderMetadata(snapshotMessage), { type: "snapshot", data: "old hidden\nvisible one\nvisible two\nnew hidden", source: "buffer" });

app.state.term = {
  cols: 12,
  rows: 6,
  buffer: {
    active: {
      length: 6,
      viewportY: 0,
      getLine(index) {
        const values = [
          { raw: "› a long    ", trimmedLength: 9, isWrapped: false },
          { raw: "input       ", trimmedLength: 5, isWrapped: true },
          { raw: "real output    ", trimmedLength: 12, isWrapped: false },
          { raw: "continues    ", trimmedLength: 9, isWrapped: true },
          { raw: "            ", trimmedLength: 0, isWrapped: false },
          { raw: "next paragraph", trimmedLength: 14, isWrapped: false },
        ];
        const value = values[index];
        return {
          isWrapped: value.isWrapped,
          length: value.raw.length,
          getCell(column) {
            const occupied = column < value.trimmedLength;
            const char = occupied ? value.raw[column] : "";
            return {
              getCode: () => char ? char.codePointAt(0) : 0,
              getChars: () => char,
            };
          },
          translateToString(trimRight, start = 0, end = value.raw.length) {
            const text = value.raw.slice(start, end);
            return trimRight ? text.trimEnd() : text;
          },
        };
      },
    },
  },
};
await app.syncSnapshotNow("snapshot-request-1");
snapshotMessage = sentMessages.pop();
assertSnapshotRenderMetadata(snapshotMessage);
assert.deepEqual(withoutSnapshotRenderMetadata(snapshotMessage), {
  type: "snapshot",
  data: "› a long input\nreal output continues\n\nnext paragraph",
  source: "buffer",
  request_id: "snapshot-request-1",
});

sentMessages.length = 0;
app.state.terminalInputQueue = Promise.resolve();
await app.queueTerminalInput("\r");
assert.deepEqual(sentMessages.pop(), {
  type: "input",
  data: "\r",
  baseline_snapshot: "› a long input\nreal output continues\n\nnext paragraph",
  baseline_source: "buffer",
  baseline_continuity_version: 2,
  baseline_render_epoch: snapshotMessage.render_epoch + 1,
  baseline_buffer_type: "unknown",
  baseline_buffer_at_capacity: false,
  baseline_anchor_guard_active: false,
  baseline_anchor_guard_line: -1,
  baseline_cursor_line: -1,
});
await app.queueTerminalInput("x");
assert.deepEqual(sentMessages.pop(), { type: "input", data: "x" });
await app.queueTerminalInput("pasted first line\npasted second line\r");
const pastedMultilineInput = sentMessages.pop();
assert.equal(pastedMultilineInput.type, "input");
assert.equal(pastedMultilineInput.data, "pasted first line\npasted second line\r");
assert.equal(
  pastedMultilineInput.baseline_snapshot,
  "› a long input\nreal output continues\n\nnext paragraph",
  "a pasted input chunk containing line breaks must still carry one pre-input baseline",
);
assert.equal(pastedMultilineInput.baseline_continuity_version, 2);
assert.equal(pastedMultilineInput.baseline_render_epoch, snapshotMessage.render_epoch + 2);

sentMessages.length = 0;
const snapshotContext = {
  socket: app.state.socket,
  term: app.state.term,
  generation: app.state.terminalGeneration,
  sessionID: app.state.active,
};
const concurrentSnapshotIDs = Array.from({ length: 10 }, (_, index) => `snapshot-concurrent-${index + 1}`);
await Promise.all(concurrentSnapshotIDs.map((requestID) => app.enqueueSnapshotSync(requestID, snapshotContext)));
assert.deepEqual(
  sentMessages.filter((message) => message.type === "snapshot").map((message) => message.request_id),
  concurrentSnapshotIDs,
  "a snapshot batch should answer every concurrent request exactly once and in order",
);

sentMessages.length = 0;
const oldBatchContext = { ...snapshotContext };
const oldBatchIDs = Array.from({ length: 10 }, (_, index) => `snapshot-old-generation-${index + 1}`);
const oldBatch = Promise.all(oldBatchIDs.map((requestID) => app.enqueueSnapshotSync(requestID, oldBatchContext)));
app.state.terminalGeneration++;
const batchGeneration = app.state.terminalGeneration;
const newBatchTerm = {
  cols: 120,
  rows: 2,
  buffer: {
    active: {
      length: 1,
      getLine() {
        return { isWrapped: false, translateToString: () => "new generation snapshot" };
      },
    },
  },
};
const newBatchSocket = {
  readyState: FakeWebSocket.OPEN,
  send(payload) {
    sentMessages.push(JSON.parse(payload));
  },
};
app.state.term = newBatchTerm;
app.state.socket = newBatchSocket;
const newBatchContext = {
  socket: newBatchSocket,
  term: newBatchTerm,
  generation: batchGeneration,
  sessionID: app.state.active,
};
const newBatchIDs = Array.from({ length: 10 }, (_, index) => `snapshot-new-generation-${index + 1}`);
const newBatch = Promise.all(newBatchIDs.map((requestID) => app.enqueueSnapshotSync(requestID, newBatchContext)));
await Promise.all([oldBatch, newBatch]);
const generationMessages = sentMessages.filter((message) => message.type === "snapshot");
assert.deepEqual(
  generationMessages.map((message) => message.request_id),
  newBatchIDs,
  "switching terminal generation must drop the whole old batch and preserve the new batch",
);
assert.ok(
  generationMessages.every((message) => message.data === "new generation snapshot"),
  "new-generation responses must never contain the old terminal buffer",
);
app.state.term = snapshotContext.term;
app.state.socket = snapshotContext.socket;
app.state.terminalGeneration = snapshotContext.generation;

sentMessages.length = 0;
const closingSocket = {
  readyState: FakeWebSocket.OPEN,
  send(payload) {
    sentMessages.push(JSON.parse(payload));
  },
};
app.state.socket = closingSocket;
const closingContext = { ...snapshotContext, socket: closingSocket };
const closingRequest = app.enqueueSnapshotSync("snapshot-closing-socket", closingContext);
closingSocket.readyState = 3;
await closingRequest;
assert.equal(
  sentMessages.some((message) => message.request_id === "snapshot-closing-socket"),
  false,
  "a socket closed during capture must not receive a late response",
);
app.state.socket = snapshotContext.socket;

sentMessages.length = 0;
let unstableRevision = 0;
const unstableTerm = {
  cols: 120,
  rows: 2,
  buffer: {
    active: {
      length: 1,
      getLine() {
        unstableRevision++;
        return { isWrapped: false, translateToString: () => `redraw-${unstableRevision}` };
      },
    },
  },
};
app.state.terminalGeneration++;
app.state.term = unstableTerm;
const unstableContext = {
  socket: app.state.socket,
  term: unstableTerm,
  generation: app.state.terminalGeneration,
  sessionID: app.state.active,
};
await app.enqueueSnapshotSync("snapshot-never-stable", unstableContext);
assert.equal(
  sentMessages.some((message) => message.request_id === "snapshot-never-stable"),
  false,
  "a continuously redrawing TUI must fail closed instead of returning an incoherent boundary",
);
app.state.term = snapshotContext.term;
app.state.terminalGeneration = snapshotContext.generation;

sentMessages.length = 0;
const staleTerm = app.state.term;
const staleSocket = app.state.socket;
const staleGeneration = app.state.terminalGeneration;
const staleRequest = app.syncSnapshotNow("snapshot-stale-generation", {
  socket: staleSocket,
  term: staleTerm,
  generation: staleGeneration,
  sessionID: app.state.active,
});
app.state.terminalGeneration++;
app.state.term = { ...staleTerm };
app.state.socket = {
  readyState: FakeWebSocket.OPEN,
  send(payload) {
    sentMessages.push(JSON.parse(payload));
  },
};
await staleRequest;
assert.equal(
  sentMessages.some((message) => message.request_id === "snapshot-stale-generation"),
  false,
  "a terminal replacement should cancel an in-flight snapshot from the old generation",
);
app.state.term = staleTerm;
app.state.socket = staleSocket;
app.state.terminalGeneration = staleGeneration;

sentMessages.length = 0;
const markerPhysicalLines = 140;
const markerNormalBuffer = {
  type: "normal",
  length: markerPhysicalLines,
  baseY: markerPhysicalLines - 2,
  cursorY: 1,
  getLine(index) {
    return {
      isWrapped: index % 2 === 1,
      translateToString: () => index % 2 === 0 ? `logical-${index / 2}:` : " continuation",
    };
  },
};
const markerAlternateBuffer = {
  type: "alternate",
  length: 2,
  baseY: 0,
  cursorY: 1,
  getLine(index) {
    return { isWrapped: false, translateToString: () => `alternate-${index}` };
  },
};
let markerActiveBuffer = markerNormalBuffer;
const registeredMarkers = [];
const markerTerm = {
  cols: 120,
  rows: 2,
  options: { scrollback: markerPhysicalLines - 2 },
  buffer: {
    normal: markerNormalBuffer,
    alternate: markerAlternateBuffer,
    get active() {
      return markerActiveBuffer;
    },
  },
  registerMarker(offset) {
    const callbacks = [];
    const marker = {
      line: markerNormalBuffer.baseY + markerNormalBuffer.cursorY + offset,
      isDisposed: false,
      onDispose(callback) {
        callbacks.push(callback);
      },
      dispose() {
        if (this.isDisposed) return;
        this.isDisposed = true;
        callbacks.forEach((callback) => callback());
      },
    };
    registeredMarkers.push({ offset, marker });
    return marker;
  },
};
app.state.term = markerTerm;
app.state.terminalInputQueue = Promise.resolve();
await app.queueTerminalInput("\r");
const guardedBaseline = sentMessages.pop();
assert.equal(guardedBaseline.baseline_buffer_type, "normal");
assert.equal(guardedBaseline.baseline_buffer_at_capacity, true);
assert.equal(guardedBaseline.baseline_anchor_guard_active, true);
assert.equal(guardedBaseline.baseline_continuity_version, 2);
assert.equal(guardedBaseline.baseline_anchor_guard_line, 6);
assert.equal(guardedBaseline.baseline_cursor_line, 69);
assert.equal(registeredMarkers[0].offset, -127, "guard should start at the earliest physical row covering the last 64 logical lines");

registeredMarkers[0].marker.dispose();
assert.ok(app.state.snapshotRenderEpoch > guardedBaseline.baseline_render_epoch, "automatic marker disposal should advance render epoch");
await app.syncSnapshotNow();
const disposedGuardSnapshot = sentMessages.pop();
assertSnapshotRenderMetadata(disposedGuardSnapshot, "normal", true, false);
assert.equal(registeredMarkers.length, 1, "ordinary refresh must not renew a disposed guard");

sentMessages.length = 0;
const markerSnapshotContext = {
  socket: app.state.socket,
  term: markerTerm,
  generation: app.state.terminalGeneration,
  sessionID: app.state.active,
};
const refreshBeforeBaseline = app.enqueueSnapshotSync("refresh-before-baseline", markerSnapshotContext, "refresh");
const inputBaselineOne = app.enqueueSnapshotSync("input-baseline-1", markerSnapshotContext, "input_baseline");
const inputBaselineTwo = app.enqueueSnapshotSync("input-baseline-2", markerSnapshotContext, "input_baseline");
await Promise.all([refreshBeforeBaseline, inputBaselineOne, inputBaselineTwo]);
const purposeMessages = sentMessages.filter((message) => message.type === "snapshot");
assert.deepEqual(
  purposeMessages.map((message) => message.request_id),
  ["refresh-before-baseline", "input-baseline-1", "input-baseline-2"],
  "different-purpose batches should stay FIFO while matching baseline requests share one capture",
);
assert.equal(registeredMarkers.length, 2, "one input_baseline batch should prepare exactly one marker");
assertSnapshotRenderMetadata(purposeMessages[0], "normal", true, false);
assertSnapshotRenderMetadata(purposeMessages[1], "normal", true, true, 6);
assertSnapshotRenderMetadata(purposeMessages[2], "normal", true, true, 6);
const secondGuardEpoch = purposeMessages[1].render_epoch;
markerActiveBuffer = markerAlternateBuffer;
await app.syncSnapshotNow();
const switchedBufferSnapshot = sentMessages.pop();
assertSnapshotRenderMetadata(switchedBufferSnapshot, "alternate", false, false);
assert.ok(switchedBufferSnapshot.render_epoch > secondGuardEpoch, "active buffer switch should invalidate the guard and advance epoch");
assert.equal(registeredMarkers[1].marker.isDisposed, true, "buffer switch should release the old marker");

const invalidMarker = {
  line: 7,
  isDisposed: false,
  dispose() {
    this.isDisposed = true;
  },
};
const invalidMarkerTerm = {
  cols: 120,
  rows: 2,
  options: { scrollback: markerPhysicalLines - 2 },
  buffer: { active: markerNormalBuffer, normal: markerNormalBuffer, alternate: markerAlternateBuffer },
  registerMarker() {
    return invalidMarker;
  },
};
app.state.term = invalidMarkerTerm;
app.state.terminalInputQueue = Promise.resolve();
await app.queueTerminalInput("\r");
const invalidMarkerBaseline = sentMessages.pop();
assert.equal(invalidMarkerBaseline.baseline_anchor_guard_active, false, "a marker not exactly on the requested physical line must be rejected");
assert.equal(invalidMarkerBaseline.baseline_anchor_guard_line, -1);
assert.equal(invalidMarkerBaseline.baseline_cursor_line, 69);
assert.equal(invalidMarker.isDisposed, true);

sentMessages.length = 0;
let trackedBufferLength = 4;
const normalTrackedBuffer = {
  type: "normal",
  get length() {
    return trackedBufferLength;
  },
  getLine(index) {
    return { isWrapped: false, translateToString: () => `normal-${index}` };
  },
};
const alternateTrackedBuffer = {
  type: "alternate",
  length: 2,
  getLine(index) {
    return { isWrapped: false, translateToString: () => `alternate-${index}` };
  },
};
let activeTrackedBuffer = normalTrackedBuffer;
const continuityTerm = {
  cols: 120,
  rows: 2,
  options: { scrollback: 2 },
  buffer: {
    normal: normalTrackedBuffer,
    alternate: alternateTrackedBuffer,
    get active() {
      return activeTrackedBuffer;
    },
  },
};
app.state.term = continuityTerm;
await app.syncSnapshotNow();
const capacitySnapshot = sentMessages.pop();
assertSnapshotRenderMetadata(capacitySnapshot, "normal", true);

trackedBufferLength = 3;
await app.syncSnapshotNow();
const regressedSnapshot = sentMessages.pop();
assertSnapshotRenderMetadata(regressedSnapshot, "normal", false);
assert.ok(regressedSnapshot.render_epoch > capacitySnapshot.render_epoch, "buffer length regression should advance render epoch");

activeTrackedBuffer = alternateTrackedBuffer;
await app.syncSnapshotNow();
const alternateSnapshot = sentMessages.pop();
assertSnapshotRenderMetadata(alternateSnapshot, "alternate", false);
assert.ok(alternateSnapshot.render_epoch > regressedSnapshot.render_epoch, "active buffer replacement should advance render epoch");

app.state.terminalGeneration++;
await app.syncSnapshotNow();
const generationSnapshot = sentMessages.pop();
assertSnapshotRenderMetadata(generationSnapshot, "alternate", false);
assert.ok(generationSnapshot.render_epoch > alternateSnapshot.render_epoch, "terminal generation change should advance render epoch");

app.state.term = {
  cols: 120,
  rows: 2,
  buffer: {
    active: {
      length: 0,
      viewportY: 0,
      getLine() {
        return null;
      },
    },
  },
};

terminalDOMRows = [
  terminalRow([["/model", 0]]),
  terminalRow([["/model choose what model and reasoning effort to use", 0]]),
  terminalRow([["Select Model and Effort", 0]]),
  terminalRow([["Access legacy models by running codex -m <model_name>", 0]]),
  terminalRow([["› 1. gpt-5.5 (current)", 0], ["Frontier model", 240]]),
  terminalRow([["  2. gpt-5.4", 0], ["Strong model", 240]]),
  terminalRow([]),
];
await app.syncSnapshotNow();
snapshotMessage = sentMessages.pop();
assertSnapshotRenderMetadata(snapshotMessage, "dom");
assert.deepEqual(withoutSnapshotRenderMetadata(snapshotMessage), {
  type: "snapshot",
  data: "/model\n/model choose what model and reasoning effort to use\nSelect Model and Effort\nAccess legacy models by running codex -m <model_name>\n› 1. gpt-5.5 (current)        Frontier model\n  2. gpt-5.4                  Strong model",
  source: "dom",
});

terminalDOMRows = [
  terminalRow([["Select Reasoning Level for gpt-5.5", 0]]),
  terminalRow([["1. Low                  Fast responses with lighter reasoning", 0]]),
  terminalRow([["2. Medium (default)     Balances speed and reasoning depth for everyday tasks", 0]]),
  terminalRow([["3. High                 Greater reasoning depth for complex problems", 0]]),
  terminalRow([["› 4. Extra high (current)  Extra high reasoning depth for complex problems", 0]]),
  terminalRow([["Press enter to confirm or esc to go back", 0]]),
];
await app.syncSnapshotNow();
snapshotMessage = sentMessages.pop();
assertSnapshotRenderMetadata(snapshotMessage, "dom");
assert.deepEqual(withoutSnapshotRenderMetadata(snapshotMessage), {
  type: "snapshot",
  data: "Select Reasoning Level for gpt-5.5\n1. Low                  Fast responses with lighter reasoning\n2. Medium (default)     Balances speed and reasoning depth for everyday tasks\n3. High                 Greater reasoning depth for complex problems\n› 4. Extra high (current)  Extra high reasoning depth for complex problems\nPress enter to confirm or esc to go back",
  source: "dom",
});
terminalDOMRows = [
  terminalRow([["alpha", 0], ["beta", 40.3]]),
  terminalRow([["left", 0], ["right", 80]]),
];
await app.syncSnapshotNow();
snapshotMessage = sentMessages.pop();
assertSnapshotRenderMetadata(snapshotMessage, "dom");
assert.deepEqual(withoutSnapshotRenderMetadata(snapshotMessage), {
  type: "snapshot",
  data: "alphabeta\nleft      right",
  source: "dom",
});
terminalDOMRows = [];
sentMessages.length = 0;

elements["help-open"].onclick();
assert.equal(elements["help-dialog"].open, true, "help dialog should open from topbar button");
helpTabs[1].onclick();
assert.ok(helpTabs[1].className.includes("active"), "clicked help tab should become active");
assert.ok(helpPanels[1].className.includes("active"), "target help panel should become active");
elements["help-close"].onclick();
assert.equal(elements["help-dialog"].open, false, "help dialog should close");

await Promise.resolve(elements["lark-register-start"].onclick());
await Promise.resolve();
assert.equal(elements["lark-register-panel"].hidden, false, "lark registration panel should show");
assert.equal(elements["lark-register-code"].textContent, "USER-1");
assert.equal(elements["lark-register-link"].href, "https://open.feishu.cn/page/cli?user_code=USER-1");
assert.ok(elements["lark-register-qr"].src.includes("/api/lark-app-registration/qr?text="));
assert.equal(elements["lark-app-console-link"].href, "https://open.feishu.cn/app/app-id/auth");
elements["lark-copy-contact-scope"].onclick();
await Promise.resolve();
assert.equal(context.copiedText, "contact:user.base:readonly");
assert.equal(context.clipboardWriteCount, 0, "HTTP-compatible selection copy should run synchronously before Clipboard API");
elements["lark-copy-group-scope"].onclick();
await Promise.resolve();
assert.equal(context.copiedText, "im:message.group_msg");
context.navigator.clipboard.writeText = async () => { throw new Error("clipboard denied"); };
await elements["lark-copy-contact-scope"].onclick();
assert.equal(context.copiedText, "contact:user.base:readonly");
assert.equal(elements["lark-permission-status"].textContent, "已复制 Scope：contact:user.base:readonly");

elements["composer-input"].value = "line one";
let prevented = false;
elements["composer-input"].onkeydown({
  key: "Enter",
  metaKey: false,
  ctrlKey: false,
  preventDefault() {
    prevented = true;
  },
});
assert.equal(prevented, false, "plain Enter should keep textarea newline behavior");
assert.equal(sentMessages.length, 0, "plain Enter should not send");

elements["composer-input"].value = "echo command-enter";
elements["composer-input"].onkeydown({
  key: "Enter",
  metaKey: true,
  ctrlKey: false,
  preventDefault() {
    prevented = true;
  },
});
assert.deepEqual(sentMessages.pop(), { type: "submit", data: "echo command-enter" });

let pastePrevented = false;
await elements.terminal.dispatchEvent({
  type: "paste",
  clipboardData: {
    files: [],
    items: [{
      kind: "file",
      type: "image/png",
      getAsFile() {
        return { name: "paste.png", type: "image/png" };
      },
    }],
  },
  preventDefault() {
    pastePrevented = true;
  },
});
await Promise.resolve();
assert.equal(pastePrevented, true, "terminal image paste should prevent default paste handling");
assert.deepEqual(sentMessages.pop(), { type: "input", data: " /tmp/iris-test/paste.png " });

app.state.sessions = [{
  id: "sess-1",
  name: "A",
  status: "running",
  live: true,
  updated_at: new Date().toISOString(),
  notify_on_waiting: false,
  notifications_available: true,
}];
app.renderSessions();
const card = elements.sessions.children[0];
assert.ok(card.className.includes("session-running"), "running session card should have running class");
assert.equal(elements["active-title"].textContent, "A（running）", "active title should show the latest session status");
app.state.sessions[0].status = "waiting";
app.renderSessions();
assert.equal(elements["active-title"].textContent, "A（waiting）", "session refresh should update the active title status");
const notify = card.querySelector(".notify-input");
notify.checked = true;
await notify.onchange({ stopPropagation() {}, target: notify });
assert.ok(fetchCalls.some((call) => call.path === "/api/sessions/sess-1" && call.options.method === "PATCH" && call.options.body.includes('"notify_on_waiting":true')));

await app.loadConfig();
elements["cfg-fast-waiting"].value = "450";
elements["cfg-conservative-waiting"].value = "900";
elements["cfg-lark-max-lines"].value = "120";
elements["cfg-lark-fallback-tail-lines"].value = "80";
elements["cfg-lark-app-id"].value = "new-app";
elements["cfg-lark-app-secret"].value = "new-secret";
elements["cfg-lark-receive-id"].value = "ou_new";
elements["cfg-lark-default-session-name"].value = "默认会话";
elements["cfg-lark-session-chat-prefix"].value = "DEV ·";
elements["cfg-lark-ignore-prefix"].value = "/silent";
elements["cfg-lark-auto-summary-prompt"].value = "总结上一轮输出";
elements["cfg-lark-mention-enabled"].checked = false;
elements["cfg-prestart-command"].value = "source ~/.zshrc";
elements["drop-rule-add"].onclick();
let dropRuleRow = elements["drop-rule-list"].children.find((node) => node.className === "drop-rule-row");
dropRuleRow.children[1].value = "噪声";
dropRuleRow.children[1].oninput();
dropRuleRow.children[2].value = "noise";
dropRuleRow.children[2].oninput();
elements["drop-rule-add"].onclick();
dropRuleRow = elements["drop-rule-list"].children.filter((node) => node.className === "drop-rule-row")[1];
dropRuleRow.children[1].value = "调试";
dropRuleRow.children[1].oninput();
dropRuleRow.children[2].value = "debug";
dropRuleRow.children[2].oninput();
assert.deepEqual(JSON.parse(elements["cfg-drop-patterns"].value), [
  { title: "噪声", kind: "line", pattern: "noise", action: "", groups: [] },
  { title: "调试", kind: "line", pattern: "debug", action: "", groups: [] },
], "drop rule editor should write JSON");
elements["custom-shortcut-add"].onclick();
const shortcutRow = elements["custom-shortcut-list"].children.find((node) => node.className === "custom-shortcut-row");
shortcutRow.children[0].value = "状态";
shortcutRow.children[0].oninput();
shortcutRow.children[1].value = "git status";
shortcutRow.children[1].oninput();
assert.deepEqual(JSON.parse(elements["cfg-lark-custom-shortcuts"].value), [{ label: "状态", command: "git status" }], "custom shortcut editor should write JSON");
elements["cfg-session-name-presets"].value = JSON.stringify({ "会话 A": { commands: ["pwd"] } });
elements["cfg-session-start-presets"].value = JSON.stringify({ "1": { commands: ["codex"] } });
app.renderNamePresets();
app.renderStartPresets();
assert.equal(elements["preset-list"].children.length, 1, "name preset list should mirror JSON");
assert.equal(elements["start-preset-list"].children.length, 1, "start preset list should mirror JSON");
elements["prestart-command-list"].children[0].querySelector(".preset-command-input").value = "source ~/.zshrc";
elements["prestart-command-list"].children[0].querySelector(".preset-command-input").oninput();
assert.equal(elements["cfg-prestart-command"].value, "source ~/.zshrc", "prestart row editor should sync textarea");
elements["preset-session-name"].value = "开发";
app.saveNamePresetFromForm();
let devPreset = elements["preset-list"].children.find((child) => child.children[0]?.children?.[0]?.textContent === "开发");
let devAddRow = devPreset.children.find((node) => node.className === "preset-command-add-row");
devAddRow.children[0].value = "cd project/dev";
devAddRow.children[1].onclick();
devPreset = elements["preset-list"].children.find((child) => child.children[0]?.children?.[0]?.textContent === "开发");
devAddRow = devPreset.children.find((node) => node.className === "preset-command-add-row");
devAddRow.children[0].value = "codex";
devAddRow.children[1].onclick();
let editedNamePresets = JSON.parse(elements["cfg-session-name-presets"].value);
assert.deepEqual(editedNamePresets["开发"], { commands: ["cd project/dev", "codex"] }, "visual preset editor should write JSON");
elements["start-preset-code"].value = "dev";
elements["start-preset-save"].onclick();
let startPreset = elements["start-preset-list"].children.find((child) => child.children[0]?.children?.[0]?.textContent === "dev");
let startAddRow = startPreset.children.find((node) => node.className === "preset-command-add-row");
startAddRow.children[0].value = "opencode";
startAddRow.children[1].onclick();
let editedStartPresets = JSON.parse(elements["cfg-session-start-presets"].value);
assert.deepEqual(editedStartPresets.dev, { commands: ["opencode"] }, "visual start preset editor should write JSON");
startPreset = elements["start-preset-list"].children.find((child) => child.children[0]?.children?.[0]?.textContent === "dev");
let startCommandRow = startPreset.children.find((node) => node.className === "preset-command-display").children[0];
startCommandRow.children[1].children[0].onclick();
startPreset = elements["start-preset-list"].children.find((child) => child.children[0]?.children?.[0]?.textContent === "dev");
startCommandRow = startPreset.children.find((node) => node.className === "preset-command-display").children[0];
startCommandRow.children[0].value = "claude --dangerously-skip-permissions";
await app.testLarkConfig();
let editedStartConfig = fetchCalls.filter((call) => call.path === "/api/config/lark-test" && call.options.method === "POST").at(-1);
assert.deepEqual(JSON.parse(editedStartConfig.options.body).session_start_presets.dev, { commands: ["claude --dangerously-skip-permissions"] }, "config read should flush pending edited start preset command");
elements["start-preset-code"].value = "0";
elements["start-preset-save"].onclick();
assert.match(elements["start-preset-status"].textContent, /0 保留/, "start preset editor should reject reserved 0");
elements["startup-json-toggle"].onclick();
assert.equal(elements["startup-json-preview"].hidden, false, "json preview should open");
assert.ok(elements["startup-json-preview"].value.includes('"开发"'), "json preview should show current presets");
elements["startup-json-preview"].value = JSON.stringify({
  session_pre_start_command: "source ~/.zshrc\nexport A=1",
  session_name_presets: { "JSON会话": { commands: ["pwd"] } },
  session_start_presets: { agent: { commands: ["opencode"] } },
}, null, 2);
elements["startup-json-preview"].oninput();
assert.equal(elements["cfg-prestart-command"].value, "source ~/.zshrc\nexport A=1", "json editor should sync prestart command");
assert.deepEqual(JSON.parse(elements["cfg-session-name-presets"].value), { "JSON会话": { commands: ["pwd"] } }, "json editor should sync name presets");
assert.deepEqual(JSON.parse(elements["cfg-session-start-presets"].value), { agent: { commands: ["opencode"] } }, "json editor should sync start presets");
assert.equal(elements["start-preset-list"].children[0].children[0].children[0].textContent, "agent", "json editor should sync visual start preset list");
elements["startup-json-preview"].value = "{";
elements["startup-json-preview"].oninput();
await assert.rejects(() => app.saveConfig(), /启动配置 JSON 格式不正确/, "invalid startup json should block save");
elements["startup-json-preview"].value = JSON.stringify({
  session_pre_start_command: "source ~/.zshrc",
  session_name_presets: { "开发": { commands: ["cd project/dev", "codex"] } },
  session_start_presets: { "1": { commands: ["codex"] } },
}, null, 2);
elements["startup-json-preview"].oninput();
elements["cfg-agent-preset"].value = "codex";
elements["cfg-agent-preset"].onchange();
let generatedStartPresets = JSON.parse(elements["cfg-session-start-presets"].value);
assert.equal(generatedStartPresets["999999"], undefined, "Agent selection should no longer depend on a hidden start preset");
elements["cfg-agent-preset"].value = "claude";
elements["cfg-agent-preset"].onchange();
assert.match(elements["agent-preset-status"].textContent, /claude --dangerously-skip-permissions/, "Claude preset should use unattended permission mode");
elements["cfg-agent-preset"].value = "custom";
elements["cfg-agent-custom-name"].value = "";
elements["cfg-agent-custom-command"].value = "";
elements["cfg-agent-preset"].onchange();
assert.equal(elements["cfg-agent-preset"].value, "custom", "empty custom preset should remain selected while the user enters a command");
assert.equal(elements["cfg-custom-agent-fields"].hidden, false, "custom Agent fields should remain visible");
elements["cfg-agent-custom-name"].value = "My Agent";
elements["cfg-agent-custom-command"].value = "my-agent --run";
elements["cfg-agent-custom-command"].onchange();
assert.match(elements["agent-preset-status"].textContent, /my-agent --run/, "custom Agent command should be reflected in status");
app.state.config = {
  ...app.state.config,
  agent_kind: "codex",
  agent_name: "Codex",
  agent_command: "codex --dangerously-bypass-approvals-and-sandbox",
};
app.openConfigDialog("config-session");
assert.equal(elements["cfg-agent-preset"].value, "codex", "Agent should be selected from saved Agent config");
elements["startup-json-preview"].value = JSON.stringify({
  session_pre_start_command: "source ~/.zshrc",
  session_name_presets: { "开发": { commands: ["cd project/dev", "codex"] } },
  session_start_presets: { "1": { commands: ["codex"] } },
}, null, 2);
elements["startup-json-preview"].oninput();
elements["cfg-fast-waiting"].value = "450";
elements["cfg-conservative-waiting"].value = "";
elements["cfg-auto-refresh-interval"].value = "6000";
elements["cfg-headless-snapshot-timeout"].value = "15000";
elements["cfg-lark-max-lines"].value = "";
elements["cfg-lark-fallback-tail-lines"].value = "";
elements["cfg-lark-merge-wrapped-lines"].checked = true;
elements["cfg-lark-app-id"].value = "new-app";
elements["cfg-lark-mention-enabled"].checked = false;
elements["cfg-lark-session-chat-prefix"].value = "DEV ·";
elements["cfg-lark-ignore-prefix"].value = "/ignore";
elements["cfg-lark-auto-summary-prompt"].value = "请总结上一轮输出";
elements["cfg-drop-patterns"].value = JSON.stringify([
  { title: "噪声", pattern: "noise" },
  { title: "调试", pattern: "debug" },
]);
elements["cfg-lark-custom-shortcuts"].value = JSON.stringify([{ label: "状态", command: "git status" }]);
elements["cfg-lark-default-session-name"].value = "Claude 会话";
elements["cfg-agent-preset"].value = "custom";
elements["cfg-agent-custom-name"].value = "Claude 私有助手";
elements["cfg-agent-custom-command"].value = "claude --dangerously-skip-permissions";
elements["cfg-agent-preset"].onchange();
await app.testLarkConfig();
assert.ok(fetchCalls.some((call) => call.path === "/api/config/lark-test" && call.options.method === "POST"), "lark config test should POST /api/config/lark-test");
assert.equal(elements["lark-test-result"].children.length, 2, "lark test result should render steps");
const environmentResult = await app.checkEnvironment();
const environmentCall = fetchCalls.find((call) => call.path === "/api/environment-check" && call.options.method === "POST");
assert.ok(environmentCall, "environment check should POST the current unsaved form config");
assert.equal(JSON.parse(environmentCall.options.body).agent_command, "claude --dangerously-skip-permissions");
assert.equal(environmentResult.ok, false);
assert.equal(elements["environment-check-result"].children.length, 3, "environment check should render every check item");
assert.equal(elements["environment-check-result"].children[0].className, "environment-check-step ok");
assert.equal(elements["environment-check-result"].children[1].className, "environment-check-step error");
assert.equal(elements["environment-check-result"].children[2].className, "environment-check-step warning");
assert.equal(elements["environment-check-start"].disabled, false);
assert.equal(elements["environment-check-start"].textContent, "重新检测");
await app.saveConfig();
const configPatch = fetchCalls.filter((call) => call.path === "/api/config" && call.options.method === "PATCH").at(-1);
assert.ok(configPatch, "config form should PATCH /api/config");
const patchedConfig = JSON.parse(configPatch.options.body);
assert.equal(patchedConfig.fast_waiting_transition_ms, 450);
assert.equal(patchedConfig.conservative_waiting_transition_ms, 700);
assert.equal(patchedConfig.lark_auto_refresh_interval_ms, 6000);
assert.equal(patchedConfig.headless_snapshot_timeout_ms, 15000);
assert.equal(patchedConfig.lark_notify_max_lines, 300);
assert.equal(patchedConfig.lark_notify_fallback_tail_lines, 100);
assert.equal(patchedConfig.lark_notify_merge_wrapped_lines, true);
assert.equal(patchedConfig.lark_app_id, "new-app");
assert.equal(patchedConfig.lark_mention_enabled, false);
assert.equal(patchedConfig.lark_session_chat_prefix, "DEV ·");
assert.equal(patchedConfig.lark_ignore_message_prefix, "/ignore");
assert.equal(patchedConfig.lark_auto_summary_prompt, "请总结上一轮输出");
assert.deepEqual(patchedConfig.lark_notify_drop_line_patterns, [
  { title: "噪声", kind: "line", pattern: "noise", action: "", groups: [] },
  { title: "调试", kind: "line", pattern: "debug", action: "", groups: [] },
]);
assert.deepEqual(patchedConfig.lark_custom_shortcuts, [{ label: "状态", command: "git status" }]);
assert.equal(patchedConfig.agent_kind, "custom");
assert.equal(patchedConfig.agent_name, "Claude 私有助手");
assert.equal(patchedConfig.agent_command, "claude --dangerously-skip-permissions");
assert.deepEqual(patchedConfig.session_start_presets, { "1": { commands: ["codex"] } });

console.log("frontend e2e ok");
