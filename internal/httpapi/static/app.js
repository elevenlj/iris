const state = {
  sessions: [],
  active: null,
  socket: null,
  term: null,
  fit: null,
  quick: [],
  config: null,
  security: null,
  search: "",
  larkRegistrationTimer: null,
  editingPresetCommand: null,
  editingStartPresetCommand: null,
  startupJSONDirty: false,
  pendingTerminalWrite: Promise.resolve(),
  terminalInputQueue: Promise.resolve(),
  snapshotSyncQueue: Promise.resolve(),
  snapshotSyncBatch: null,
  snapshotSyncBatches: [],
  terminalGeneration: 0,
  snapshotRenderEpoch: 0,
  snapshotRenderContinuity: null,
  snapshotAnchorGuard: null,
  lastSentTerminalSize: null,
  terminalResizeObserver: null,
};

const $ = (id) => document.getElementById(id);
const MIN_TERMINAL_COLS = 80;
const MIN_TERMINAL_ROWS = 20;
const STANDARD_TERMINAL_COLS = 120;
const STANDARD_TERMINAL_ROWS = 36;
const STANDARD_TERMINAL_FONT_FAMILY = "Menlo, Consolas, monospace";
const STANDARD_TERMINAL_FONT_SIZE = 13;
const STANDARD_TERMINAL_LINE_HEIGHT = 1.2;
const SNAPSHOT_CONTINUITY_VERSION = 2;
const DEFAULT_SESSION_NAME = "默认会话";
const DEFAULT_AGENT_PRESET_CODE = "999999";
const CONFIG_TAB_IDS = ["config-lark", "config-session", "config-security", "config-workspaces"];
const DROP_RULE_KINDS = [
  ["line", "行过滤"],
  ["block_head", "块首行过滤"],
  ["line_group", "行内分组过滤"],
];
const DROP_RULE_BLOCK_ACTIONS = [
  ["drop_block", "隐藏整个块"],
  ["keep_head", "只保留首行"],
];

async function api(path, options = {}) {
  const res = await fetch(path, {
    headers: options.body instanceof FormData ? {} : { "Content-Type": "application/json" },
    ...options,
  });
  if (!res.ok) {
    let msg = res.statusText;
    try {
      msg = (await res.json()).error || msg;
    } catch {}
    throw new Error(msg);
  }
  if (res.status === 204) return null;
  return res.json();
}

async function loadSessions() {
  state.sessions = await api("/api/sessions");
  renderSessions();
  if (!state.active) {
    const requestedID = new URLSearchParams(location.search).get("session");
    const requested = requestedID ? state.sessions.find((s) => s.id === requestedID && s.live) : null;
    const firstLive = requested || state.sessions.find((s) => s.live);
    if (firstLive) selectSession(firstLive.id);
  }
}

function visibleSessions() {
  const q = state.search.trim().toLowerCase();
  return state.sessions.filter((s) => {
    if (!s.live || s.status === "exited" || s.status === "failed") return false;
    if (!q) return true;
    return `${s.name} ${s.id} ${s.status}`.toLowerCase().includes(q);
  });
}

function renderSessions() {
  $("sessions").innerHTML = "";
  for (const s of visibleSessions()) {
    const el = document.createElement("article");
    el.className = `session session-${s.status} ${state.active === s.id ? "active" : ""}`;
    const updated = new Date(s.updated_at).toLocaleString();
    el.innerHTML = `
      <div class="session-head">
        <div class="session-name"></div>
        <div class="session-actions">
          <button class="link-btn delete-btn" type="button">删除</button>
        </div>
      </div>
      <div class="meta"><span class="status-${s.status}">${s.status}</span> · ${updated}</div>
      <label class="notify-row">
        <input class="notify-input" type="checkbox">
        <span>通知</span>
        <span class="notify-state"></span>
      </label>
    `;
    el.querySelector(".session-name").textContent = s.name;
    el.querySelector(".notify-input").checked = Boolean(s.notify_on_waiting);
    el.querySelector(".notify-state").textContent = s.notifications_available ? (s.notify_on_waiting ? "已启用" : "未启用") : "不可用";
    el.querySelector(".delete-btn").onclick = async (ev) => {
      ev.stopPropagation();
      await deleteSession(s.id);
    };
    el.querySelector(".notify-row").onclick = (ev) => {
      ev.stopPropagation();
    };
    el.querySelector(".notify-input").onclick = (ev) => {
      ev.stopPropagation();
    };
    el.querySelector(".notify-input").onchange = async (ev) => {
      ev.stopPropagation();
      await setNotify(s.id, ev.target.checked);
    };
    el.onclick = () => selectSession(s.id);
    $("sessions").appendChild(el);
  }
  renderActiveTitle();
}

function renderActiveTitle() {
  const sess = currentSession();
  $("active-title").textContent = sess ? `${sess.name}（${sess.status}）` : "请选择会话";
}

function currentSession() {
  return state.sessions.find((s) => s.id === state.active);
}

function initTerminal() {
  disposeSnapshotAnchorGuard();
  state.terminalGeneration++;
  if (state.term) state.term.dispose();
  state.terminalResizeObserver?.disconnect?.();
  window.removeEventListener("resize", resizeTerm);
  state.pendingTerminalWrite = Promise.resolve();
  state.terminalInputQueue = Promise.resolve();
  state.snapshotSyncQueue = Promise.resolve();
  state.snapshotSyncBatch = null;
  state.snapshotSyncBatches = [];
  state.lastSentTerminalSize = null;
  const headless = isHeadlessMode();
  state.term = new Terminal({
    cols: STANDARD_TERMINAL_COLS,
    rows: STANDARD_TERMINAL_ROWS,
    cursorBlink: true,
    convertEol: true,
    fontFamily: STANDARD_TERMINAL_FONT_FAMILY,
    fontSize: STANDARD_TERMINAL_FONT_SIZE,
    lineHeight: STANDARD_TERMINAL_LINE_HEIGHT,
    letterSpacing: 0,
    theme: { background: "#12110f", foreground: "#f4f1e8", cursor: "#f4f1e8" },
  });
  state.fit = headless ? null : createFitAddon();
  if (state.fit) state.term.loadAddon(state.fit);
  state.term.open($("terminal"));
  if (!headless) {
    resizeTerm();
    requestAnimationFrame(() => resizeTerm());
    setTimeout(() => resizeTerm(), 120);
  } else {
    state.term.resize?.(STANDARD_TERMINAL_COLS, STANDARD_TERMINAL_ROWS);
  }
  if (!headless) {
    state.term.onData((data) => {
      queueTerminalInput(data);
    });
    window.addEventListener("resize", resizeTerm);
    observeTerminalSize();
  }
}

function queueTerminalInput(data) {
  const context = {
    term: state.term,
    socket: state.socket,
    generation: state.terminalGeneration,
    sessionID: state.active,
  };
  state.terminalInputQueue = state.terminalInputQueue
    .catch(() => {})
    .then(async () => {
      if (!snapshotContextIsCurrent(context)) return;
      const message = { type: "input", data };
      if (data.includes("\r") || data.includes("\n")) {
        await state.pendingTerminalWrite.catch(() => {});
        await waitForNextPaint();
        if (!snapshotContextIsCurrent(context)) return;
        prepareSnapshotAnchorGuard(context.term, context.generation);
        const baseline = terminalVisibleSnapshotWithSource(context.term, context.generation);
        message.baseline_snapshot = baseline.data || "";
        message.baseline_source = baseline.source || "unknown";
        message.baseline_continuity_version = baseline.continuityVersion || 0;
        message.baseline_render_epoch = baseline.renderEpoch || 0;
        message.baseline_buffer_type = baseline.bufferType || "unknown";
        message.baseline_buffer_at_capacity = Boolean(baseline.bufferAtCapacity);
        message.baseline_anchor_guard_active = Boolean(baseline.anchorGuardActive);
        message.baseline_anchor_guard_line = Number.isInteger(baseline.anchorGuardLine) ? baseline.anchorGuardLine : -1;
        message.baseline_cursor_line = Number.isInteger(baseline.cursorLine) ? baseline.cursorLine : -1;
      }
      sendWSOnSocket(context.socket, message);
    });
  return state.terminalInputQueue;
}

function selectSession(id) {
  const sess = state.sessions.find((s) => s.id === id);
  if (!sess) return;
  state.active = id;
  renderSessions();
  if (sess.live) {
    connectWS(id);
  } else {
    if (state.socket) state.socket.close();
    initTerminal();
    api(`/api/sessions/${id}/output`).then((out) => writeTerminal(out.content || ""));
  }
}

function connectWS(id) {
  if (state.socket) state.socket.close();
  initTerminal();
  const ws = new WebSocket(terminalWebSocketURL(id));
  const term = state.term;
  const generation = state.terminalGeneration;
  state.socket = ws;
  ws.binaryType = "arraybuffer";
  ws.onopen = () => {
    if (!isHeadlessMode()) resizeTerm();
  };
  ws.onmessage = (ev) => {
    if (typeof ev.data === "string") {
      try {
        const msg = JSON.parse(ev.data);
        if (msg.type === "snapshot_request") {
          enqueueSnapshotSync(msg.request_id || "", { socket: ws, term, generation, sessionID: id }, msg.purpose || "");
          return;
        }
        if (msg.type === "terminal_resize") {
          syncHeadlessTerminalSize(msg.cols, msg.rows);
          return;
        }
      } catch {}
    }
    const text = typeof ev.data === "string" ? ev.data : new TextDecoder().decode(ev.data);
    void writeTerminal(text);
  };
  ws.onclose = () => setTimeout(loadSessions, 300);
}

function terminalWebSocketURL(id) {
  const proto = location.protocol === "https:" ? "wss" : "ws";
  const suffix = isHeadlessMode() ? "?headless=1" : "";
  return `${proto}://${location.host}/api/sessions/${id}/ws${suffix}`;
}

function isHeadlessMode() {
  const params = new URLSearchParams(location.search || "");
  return params.get("headless") === "1";
}

function sendWS(obj) {
  return sendWSOnSocket(state.socket, obj);
}

function sendWSOnSocket(socket, obj) {
  if (socket && socket.readyState === WebSocket.OPEN) {
    socket.send(JSON.stringify(obj));
    return true;
  }
  return false;
}

function resizeTerm() {
  if (isHeadlessMode()) return;
  if (!state.term) return;
  const size = fitTerminalToContainer();
  if (!size) return;
  sendTerminalResize(size.cols, size.rows);
}

function observeTerminalSize() {
  state.terminalResizeObserver = null;
  if (isHeadlessMode()) return;
  if (typeof ResizeObserver === "undefined") return;
  try {
    const target = $("terminal")?.parentElement || $("terminal");
    if (!target) return;
    state.terminalResizeObserver = new ResizeObserver(() => resizeTerm());
    state.terminalResizeObserver.observe(target);
  } catch {
    state.terminalResizeObserver = null;
  }
}

function createFitAddon() {
  try {
    if (typeof FitAddon === "undefined" || !FitAddon?.FitAddon) return null;
    return new FitAddon.FitAddon();
  } catch {
    return null;
  }
}

function fitTerminalToContainer() {
  if (!state.term) return null;
  const before = {
    cols: Math.floor(Number(state.term.cols)),
    rows: Math.floor(Number(state.term.rows)),
  };
  try {
    state.fit?.fit?.();
  } catch {}
  if ((!state.term.cols || !state.term.rows) && typeof state.term.resize === "function") {
    state.term.resize(STANDARD_TERMINAL_COLS, STANDARD_TERMINAL_ROWS);
  }
  const cols = Math.floor(Number(state.term.cols));
  const rows = Math.floor(Number(state.term.rows));
  if (cols < MIN_TERMINAL_COLS || rows < MIN_TERMINAL_ROWS) {
    const fallbackCols = before.cols >= MIN_TERMINAL_COLS ? before.cols : STANDARD_TERMINAL_COLS;
    const fallbackRows = before.rows >= MIN_TERMINAL_ROWS ? before.rows : STANDARD_TERMINAL_ROWS;
    state.term.resize?.(fallbackCols, fallbackRows);
    return { cols: fallbackCols, rows: fallbackRows };
  }
  return { cols, rows };
}

function sendTerminalResize(cols, rows) {
  if (isHeadlessMode()) return;
  if (cols < MIN_TERMINAL_COLS || rows < MIN_TERMINAL_ROWS) return;
  const last = state.lastSentTerminalSize;
  if (last && last.cols === cols && last.rows === rows) return;
  if (sendWS({ type: "resize", cols, rows })) {
    state.lastSentTerminalSize = { cols, rows };
  }
}

function syncHeadlessTerminalSize(cols, rows) {
  if (!isHeadlessMode()) return;
  if (!state.term || typeof state.term.resize !== "function") return;
  const nextCols = Math.floor(Number(cols));
  const nextRows = Math.floor(Number(rows));
  if (nextCols < MIN_TERMINAL_COLS || nextRows < MIN_TERMINAL_ROWS) return;
  if (Math.floor(Number(state.term.cols)) === nextCols && Math.floor(Number(state.term.rows)) === nextRows) return;
  state.term.resize(nextCols, nextRows);
}

function writeTerminal(text) {
  const term = state.term;
  if (!term || !text) return Promise.resolve();
  const writeDone = state.pendingTerminalWrite.catch(() => {}).then(() => new Promise((resolve) => {
    let settled = false;
    const finish = () => {
      if (settled) return;
      settled = true;
      resolve();
    };
    try {
      term.write(text, finish);
      if (term.write.length < 2) finish();
      setTimeout(finish, 100);
    } catch {
      finish();
    }
  }));
  state.pendingTerminalWrite = writeDone.catch(() => {});
  return writeDone;
}

function waitForNextPaint() {
  return new Promise((resolve) => {
    let settled = false;
    let timer = null;
    const finish = () => {
      if (settled) return;
      settled = true;
      if (timer !== null) clearTimeout(timer);
      resolve();
    };
    if (typeof requestAnimationFrame === "function") {
      // Background tabs may throttle requestAnimationFrame indefinitely. A
      // bounded fallback keeps correlated snapshot requests responsive while
      // still preferring a real paint in the foreground.
      timer = setTimeout(finish, 50);
      try {
        requestAnimationFrame(finish);
      } catch {
        finish();
      }
      return;
    }
    timer = setTimeout(finish, 16);
  });
}

function wait(ms) {
  return new Promise((resolve) => setTimeout(resolve, ms));
}

function terminalVisibleSnapshot(term = state.term) {
  return terminalVisibleSnapshotWithSource(term).data;
}

function terminalSnapshotMetadata(term = state.term) {
  const snapshot = terminalVisibleSnapshotWithSource(term);
  return {
    source: snapshot.source || "unknown",
    continuity_version: snapshot.continuityVersion || 0,
    render_epoch: snapshot.renderEpoch || 0,
    buffer_type: snapshot.bufferType || "unknown",
    buffer_at_capacity: Boolean(snapshot.bufferAtCapacity),
    anchor_guard_active: Boolean(snapshot.anchorGuardActive),
    anchor_guard_line: Number.isInteger(snapshot.anchorGuardLine) ? snapshot.anchorGuardLine : -1,
    cursor_line: Number.isInteger(snapshot.cursorLine) ? snapshot.cursorLine : -1,
    data_length: (snapshot.data || "").length,
    line_count: snapshot.data ? snapshot.data.split("\n").length : 0,
    terminal_generation: state.terminalGeneration,
    socket_ready_state: state.socket?.readyState ?? -1,
  };
}

function terminalVisibleSnapshotWithSource(term = state.term, generation = state.terminalGeneration) {
  if (!term) {
    const metadata = observeSnapshotRenderContinuity(null, null, generation);
    return { data: "", source: "none", ...metadata };
  }
  const buffer = terminalActiveBuffer(term);
  const metadata = observeSnapshotRenderContinuity(term, buffer, generation);
  const bufferSnapshot = terminalBufferSnapshot(term, buffer);
  if (bufferSnapshot.data) {
    const anchorGuardLine = snapshotAnchorGuardDataLine(
      term,
      buffer,
      generation,
      bufferSnapshot.physicalStarts,
      bufferSnapshot.physicalEnds,
    );
    const cursorLine = terminalCursorDataLine(buffer, bufferSnapshot.physicalStarts, bufferSnapshot.physicalEnds);
    return {
      data: bufferSnapshot.data,
      source: "buffer",
      ...metadata,
      anchorGuardActive: anchorGuardLine >= 0,
      anchorGuardLine,
      cursorLine,
    };
  }
  const domSnapshot = terminalDOMVisibleSnapshot();
  if (domSnapshot) {
    return {
      data: domSnapshot,
      source: "dom",
      ...metadata,
      bufferType: "dom",
      bufferAtCapacity: false,
      anchorGuardActive: false,
      anchorGuardLine: -1,
      cursorLine: -1,
    };
  }
  return {
    data: "",
    source: "empty",
    ...metadata,
    bufferType: "empty",
    bufferAtCapacity: false,
    anchorGuardActive: false,
    anchorGuardLine: -1,
    cursorLine: -1,
  };
}

function terminalActiveBuffer(term = state.term) {
  return term?.buffer?.active || null;
}

function terminalBufferType(term, buffer) {
  const declared = typeof buffer?.type === "string" ? buffer.type.trim().toLowerCase() : "";
  if (declared) return declared;
  if (buffer && term?.buffer?.normal === buffer) return "normal";
  if (buffer && term?.buffer?.alternate === buffer) return "alternate";
  return buffer ? "unknown" : "none";
}

function terminalScrollbackLimit(term) {
  const configured = Number(term?.options?.scrollback ?? term?._core?.optionsService?.rawOptions?.scrollback);
  return Number.isFinite(configured) && configured >= 0 ? Math.floor(configured) : null;
}

function terminalBufferAtCapacity(term, buffer, bufferType) {
  if (!buffer || bufferType !== "normal") return false;
  const length = Number(buffer.length);
  const rows = Number(term?.rows);
  const scrollback = terminalScrollbackLimit(term);
  if (!Number.isFinite(length) || !Number.isFinite(rows) || rows <= 0 || scrollback === null) return false;
  return length >= Math.floor(rows) + scrollback;
}

function observeSnapshotRenderContinuity(term, buffer, generation) {
  const normalizedGeneration = Number.isFinite(Number(generation)) ? Number(generation) : 0;
  const length = Number.isFinite(Number(buffer?.length)) ? Number(buffer.length) : null;
  const previous = state.snapshotRenderContinuity;
  const changed = !previous ||
    previous.term !== term ||
    previous.buffer !== buffer ||
    previous.generation !== normalizedGeneration ||
    (length !== null && previous.length !== null && length < previous.length);
  if (changed) {
    if (previous) disposeSnapshotAnchorGuard();
    state.snapshotRenderEpoch++;
  }
  state.snapshotRenderContinuity = { term, buffer, generation: normalizedGeneration, length };
  const bufferType = terminalBufferType(term, buffer);
  const anchorGuardActive = snapshotAnchorGuardActive(term, buffer, normalizedGeneration);
  return {
    continuityVersion: SNAPSHOT_CONTINUITY_VERSION,
    renderEpoch: state.snapshotRenderEpoch,
    bufferType,
    bufferAtCapacity: terminalBufferAtCapacity(term, buffer, bufferType),
    anchorGuardActive,
  };
}

function prepareSnapshotAnchorGuard(term, generation) {
  const buffer = terminalActiveBuffer(term);
  // Apply a pending terminal/buffer transition before installing the marker;
  // otherwise the baseline capture itself would immediately invalidate it.
  observeSnapshotRenderContinuity(term, buffer, generation);
  disposeSnapshotAnchorGuard();
  // Every Enter establishes a new identity boundary even when marker support
  // is unavailable. Baseline and subsequent snapshots observe this same epoch.
  state.snapshotRenderEpoch++;
  if (!term || !buffer || terminalBufferType(term, buffer) !== "normal" || typeof term.registerMarker !== "function") return false;
  const baseY = Number(buffer.baseY);
  const cursorY = Number(buffer.cursorY);
  const length = Number(buffer.length);
  if (!Number.isFinite(baseY) || !Number.isFinite(cursorY) || !Number.isFinite(length) || length <= 0) return false;
  const cursorLine = Math.max(0, Math.min(Math.floor(length) - 1, Math.floor(baseY + cursorY)));
  const logicalSnapshot = terminalBufferLogicalSnapshot(buffer);
  if (logicalSnapshot.physicalStarts.length === 0) return false;
  const targetLine = logicalSnapshot.physicalStarts[Math.max(0, logicalSnapshot.physicalStarts.length - 64)];
  const offset = targetLine - cursorLine;
  let marker = null;
  try {
    marker = term.registerMarker(offset) || null;
  } catch {
    marker = null;
  }
  if (!marker || marker.isDisposed) return false;
  const markerLine = Number(marker.line);
  if (!Number.isFinite(markerLine) || markerLine !== targetLine) {
    try {
      marker.dispose?.();
    } catch {}
    return false;
  }
  const guard = { marker, term, buffer, generation };
  state.snapshotAnchorGuard = guard;
  if (typeof marker.onDispose === "function") {
    marker.onDispose(() => {
      if (state.snapshotAnchorGuard !== guard) return;
      state.snapshotAnchorGuard = null;
      state.snapshotRenderEpoch++;
    });
  }
  return true;
}

function snapshotAnchorGuardActive(term, buffer, generation) {
  const guard = state.snapshotAnchorGuard;
  if (!guard) return false;
  if (guard.marker?.isDisposed) {
    state.snapshotAnchorGuard = null;
    state.snapshotRenderEpoch++;
    return false;
  }
  return guard.term === term && guard.buffer === buffer && guard.generation === generation;
}

function snapshotAnchorGuardDataLine(term, buffer, generation, physicalStarts, physicalEnds) {
  if (!snapshotAnchorGuardActive(term, buffer, generation)) return -1;
  const markerLine = Number(state.snapshotAnchorGuard?.marker?.line);
  if (!Number.isFinite(markerLine)) return -1;
  return terminalPhysicalDataLine(markerLine, physicalStarts, physicalEnds);
}

function disposeSnapshotAnchorGuard() {
  const guard = state.snapshotAnchorGuard;
  if (!guard) return;
  // Clear first so a synchronous onDispose callback knows this is a deliberate
  // replacement/buffer transition rather than scrollback eviction.
  state.snapshotAnchorGuard = null;
  try {
    guard.marker?.dispose?.();
  } catch {}
}

function terminalBufferSnapshot(term = state.term, activeBuffer = null) {
  const buffer = activeBuffer || terminalActiveBuffer(term);
  return terminalBufferLogicalSnapshot(buffer);
}

function terminalBufferLogicalSnapshot(buffer) {
  if (!buffer || typeof buffer.getLine !== "function" || !Number.isFinite(buffer.length)) {
    return { data: "", physicalStarts: [], physicalEnds: [] };
  }
  const lines = [];
  const physicalStarts = [];
  const physicalEnds = [];
  const end = Math.max(0, buffer.length);
  for (let i = 0; i < end; i++) {
    const bufferLine = buffer.getLine(i);
    const text = terminalBufferPhysicalLineText(bufferLine);
    if (bufferLine?.isWrapped && lines.length > 0) {
      lines[lines.length - 1] += text;
      physicalEnds[physicalEnds.length - 1] = i;
    } else {
      lines.push(text);
      physicalStarts.push(i);
      physicalEnds.push(i);
    }
  }
  while (lines.length && lines[0].trim() === "") {
    lines.shift();
    physicalStarts.shift();
    physicalEnds.shift();
  }
  while (lines.length && lines[lines.length - 1].trim() === "") {
    lines.pop();
    physicalStarts.pop();
    physicalEnds.pop();
  }
  return { data: lines.join("\n"), physicalStarts, physicalEnds };
}

function terminalCursorDataLine(buffer, physicalStarts, physicalEnds) {
  const baseY = Number(buffer?.baseY);
  const cursorY = Number(buffer?.cursorY);
  if (!Number.isFinite(baseY) || !Number.isFinite(cursorY)) return -1;
  const physicalLine = Math.floor(baseY + cursorY);
  return terminalPhysicalDataLine(physicalLine, physicalStarts, physicalEnds);
}

function terminalPhysicalDataLine(physicalLine, physicalStarts, physicalEnds) {
  if (!Number.isInteger(physicalLine)) return -1;
  for (let i = physicalStarts.length - 1; i >= 0; i--) {
    if (physicalStarts[i] <= physicalLine && physicalLine <= physicalEnds[i]) return i;
  }
  return -1;
}

function terminalBufferPhysicalLineText(bufferLine) {
  if (!bufferLine || typeof bufferLine.translateToString !== "function") return "";
  const length = terminalBufferPhysicalLineLength(bufferLine);
  if (length !== null) {
    // The cell-aware end excludes unused padding cells while retaining a real
    // trailing U+0020 cell. translateToString(true) cannot make that distinction.
    return bufferLine.translateToString(false, 0, length) || "";
  }
  return bufferLine.translateToString(true) || "";
}

function terminalBufferPhysicalLineLength(bufferLine) {
  if (typeof bufferLine.getTrimmedLength === "function") {
    const length = Number(bufferLine.getTrimmedLength());
    if (Number.isFinite(length) && length >= 0) return Math.floor(length);
  }
  const width = Number(bufferLine.length);
  if (!Number.isFinite(width) || width < 0 || typeof bufferLine.getCell !== "function") return null;
  let reusableCell;
  for (let column = Math.floor(width) - 1; column >= 0; column--) {
    const cell = bufferLine.getCell(column, reusableCell);
    if (!cell) continue;
    reusableCell = cell;
    const code = typeof cell.getCode === "function" ? Number(cell.getCode()) : 0;
    const chars = typeof cell.getChars === "function" ? cell.getChars() : "";
    if ((Number.isFinite(code) && code !== 0) || chars !== "") return column + 1;
  }
  return 0;
}

function terminalDOMVisibleSnapshot() {
  const rows = Array.from(document.querySelectorAll("#terminal .xterm-rows > div"));
  if (!rows.length) return "";
  const cellWidth = terminalCellWidth();
  const lines = rows.map((row) => terminalDOMRowText(row, cellWidth));
  while (lines.length && lines[lines.length - 1].trim() === "") lines.pop();
  return lines.join("\n").trim();
}

function terminalCellWidth() {
  const width = state.term?._core?._renderService?.dimensions?.css?.cell?.width;
  if (Number.isFinite(width) && width > 0) return width;
  const measure = document.querySelector?.(".xterm-char-measure-element");
  const measured = measure?.getBoundingClientRect?.().width;
  if (Number.isFinite(measured) && measured > 0) return measured;
  return 8;
}

function terminalDOMRowText(row, cellWidth) {
  const rowRect = row.getBoundingClientRect?.() || { left: 0 };
  const rowLeft = Number.isFinite(rowRect.left) ? rowRect.left : 0;
  const spans = Array.from(row.querySelectorAll?.("span") || []);
  if (!spans.length) {
    return normalizeTerminalDOMText(row.textContent || "").trimEnd();
  }
  let line = "";
  let cursorCol = 0;
  let previousRight = rowLeft;
  for (const span of spans) {
    const text = normalizeTerminalDOMText(span.textContent || "");
    if (!text) continue;
    const rect = span.getBoundingClientRect?.();
    if (rect && Number.isFinite(rect.left) && cellWidth > 0) {
      const gapPx = line ? rect.left - previousRight : rect.left - rowLeft;
      if (Number.isFinite(gapPx) && gapPx > cellWidth * 0.6) {
        const gapCols = Math.max(0, Math.round(gapPx / cellWidth));
        if (gapCols > 0) {
          line += " ".repeat(gapCols);
          cursorCol += gapCols;
        }
      }
    }
    line += text;
    cursorCol += terminalTextColumns(text);
    if (rect && Number.isFinite(rect.left)) {
      previousRight = terminalRectRight(rect, rowLeft + cursorCol * cellWidth);
    } else {
      previousRight = rowLeft + cursorCol * cellWidth;
    }
  }
  return line.trimEnd();
}

function normalizeTerminalDOMText(text) {
  return text.replace(/\u00a0/g, " ").replace(/\u200b/g, "");
}

function terminalRectRight(rect, fallback) {
  if (Number.isFinite(rect.right)) return rect.right;
  if (Number.isFinite(rect.left) && Number.isFinite(rect.width)) return rect.left + rect.width;
  return fallback;
}

function terminalTextColumns(text) {
  let cols = 0;
  for (const char of Array.from(text)) {
    cols += terminalCharColumns(char);
  }
  return cols;
}

function terminalCharColumns(char) {
  const code = char.codePointAt(0);
  if (!Number.isFinite(code)) return 0;
  if ((code >= 0x0300 && code <= 0x036f) || (code >= 0xfe00 && code <= 0xfe0f)) return 0;
  if (
    (code >= 0x1100 && code <= 0x115f) ||
    code === 0x2329 ||
    code === 0x232a ||
    (code >= 0x2e80 && code <= 0xa4cf && code !== 0x303f) ||
    (code >= 0xac00 && code <= 0xd7a3) ||
    (code >= 0xf900 && code <= 0xfaff) ||
    (code >= 0xfe10 && code <= 0xfe19) ||
    (code >= 0xfe30 && code <= 0xfe6f) ||
    (code >= 0xff00 && code <= 0xff60) ||
    (code >= 0xffe0 && code <= 0xffe6)
  ) {
    return 2;
  }
  return 1;
}

function enqueueSnapshotSync(serverRequestID, context, purpose = "") {
  purpose = String(purpose || "").trim();
  const activeBatch = state.snapshotSyncBatches[state.snapshotSyncBatches.length - 1];
  if (
    activeBatch &&
    activeBatch.purpose === purpose &&
    activeBatch.context.term === context.term &&
    activeBatch.context.socket === context.socket &&
    activeBatch.context.generation === context.generation &&
    activeBatch.context.sessionID === context.sessionID
  ) {
    activeBatch.requestIDs.push(serverRequestID);
    return activeBatch.promise;
  }
  const batch = { context, purpose, requestIDs: [serverRequestID], promise: null };
  const previous = state.snapshotSyncQueue.catch(() => {});
  batch.promise = previous.then(async () => {
    try {
      if (!snapshotContextIsCurrent(context)) return;
      if (purpose === "input_baseline") {
        prepareSnapshotAnchorGuard(context.term, context.generation);
      }
      const snapshot = await captureTerminalSnapshot(context, true);
      if (!snapshot || !snapshotContextIsCurrent(context)) return;
      for (const requestID of batch.requestIDs) {
        const message = snapshotMessage(snapshot);
        if (requestID) message.request_id = requestID;
        sendWSOnSocket(context.socket, message);
      }
    } finally {
      if (state.snapshotSyncBatch === batch) state.snapshotSyncBatch = null;
      const index = state.snapshotSyncBatches.indexOf(batch);
      if (index >= 0) state.snapshotSyncBatches.splice(index, 1);
    }
  });
  state.snapshotSyncBatch = batch;
  state.snapshotSyncBatches.push(batch);
  state.snapshotSyncQueue = batch.promise.catch(() => {});
  return batch.promise;
}

function snapshotContextIsCurrent(context) {
  return Boolean(
    context?.term &&
      context?.socket &&
      state.term === context.term &&
      state.socket === context.socket &&
      state.terminalGeneration === context.generation &&
      state.active === context.sessionID &&
      context.socket.readyState === WebSocket.OPEN,
  );
}

async function syncSnapshotNow(serverRequestID = "", requestContext = null) {
  const context = requestContext || {
    term: state.term,
    socket: state.socket,
    generation: state.terminalGeneration,
    sessionID: state.active,
  };
  const snapshot = await captureTerminalSnapshot(context, Boolean(requestContext));
  if (!snapshot) return;
  const message = snapshotMessage(snapshot);
  if (serverRequestID) message.request_id = serverRequestID;
  if (requestContext) {
    sendWSOnSocket(context.socket, message);
  } else {
    sendWS(message);
  }
}

function snapshotMessage(snapshot) {
  return {
    type: "snapshot",
    data: snapshot.data || "",
    source: snapshot.source || "unknown",
    render_epoch: snapshot.renderEpoch || 0,
    buffer_type: snapshot.bufferType || "unknown",
    buffer_at_capacity: Boolean(snapshot.bufferAtCapacity),
    anchor_guard_active: Boolean(snapshot.anchorGuardActive),
    continuity_version: snapshot.continuityVersion || 0,
    anchor_guard_line: Number.isInteger(snapshot.anchorGuardLine) ? snapshot.anchorGuardLine : -1,
    cursor_line: Number.isInteger(snapshot.cursorLine) ? snapshot.cursorLine : -1,
  };
}

async function captureTerminalSnapshot(context, requireCurrent) {
  if (!context.term) return null;
  if (requireCurrent && !snapshotContextIsCurrent(context)) return null;
  await state.pendingTerminalWrite.catch(() => {});
  let snapshot = null;
  for (let i = 0; i < 3; i++) {
    if (requireCurrent && !snapshotContextIsCurrent(context)) return null;
    resizeTerm();
    await waitForNextPaint();
    await waitForNextPaint();
    const first = terminalVisibleSnapshotWithSource(context.term, context.generation);
    await wait(40);
    await state.pendingTerminalWrite.catch(() => {});
    await waitForNextPaint();
    const second = terminalVisibleSnapshotWithSource(context.term, context.generation);
    if (requireCurrent && !snapshotContextIsCurrent(context)) return null;
    if (first.data === second.data &&
        first.source === second.source &&
        first.renderEpoch === second.renderEpoch &&
        first.bufferType === second.bufferType &&
        first.bufferAtCapacity === second.bufferAtCapacity &&
        first.anchorGuardActive === second.anchorGuardActive &&
        first.continuityVersion === second.continuityVersion &&
        first.anchorGuardLine === second.anchorGuardLine &&
        first.cursorLine === second.cursorLine) {
      snapshot = second;
      break;
    }
  }
  // A screen that kept changing throughout every sample may be halfway
  // through a TUI redraw. Returning no snapshot makes the server retry instead
  // of treating an incoherent frame as a round boundary.
  return snapshot;
}

async function loadQuick() {
  state.quick = await api("/api/quick-commands");
  renderQuick();
}

async function loadConfig() {
  state.config = await api("/api/config");
  renderConfig();
}

async function loadSecurityStatus() {
  state.security = await api("/api/settings/security/status");
  return state.security;
}

function renderConfig() {
  const cfg = state.config;
  if (!cfg) return;
  $("lark-register-panel").hidden = true;
  $("lark-register-panel").classList.add("hidden");
  $("lark-register-qr").src = "";
  $("lark-register-code").textContent = "";
  $("lark-register-link").href = "";
  $("lark-register-status").textContent = "等待扫码确认...";
  $("lark-register-start").disabled = false;
  $("lark-test-start").disabled = false;
  $("lark-test-result").innerHTML = "";
  $("environment-check-start").disabled = false;
  $("environment-check-start").textContent = "开始检测";
  $("environment-check-result").innerHTML = "";
  $("settings-current-password").value = "";
  $("settings-new-password").value = "";
  $("settings-confirm-password").value = "";
  $("settings-password-status").textContent = "";
  renderSettingsPasswordMatch();
  const agentKind = cfg.agent_kind || "codex";
  $("cfg-agent-preset").value = agentKind;
  $("cfg-agent-custom-name").value = agentKind === "custom" ? (cfg.agent_name || "自定义 Agent") : "";
  $("cfg-agent-custom-command").value = agentKind === "custom" ? (cfg.agent_command || "") : "";
  renderAgentPresetControls();
  setAgentPresetStatus("所有新会话都会自动启动当前 Agent。");
  $("preset-session-name").value = "";
  $("start-preset-code").value = "";
  state.editingPresetCommand = null;
  state.editingStartPresetCommand = null;
  setPresetStatus("");
  setStartPresetStatus("");
  stopLarkRegistrationPolling();
  $("cfg-fast-waiting").value = cfg.fast_waiting_transition_ms;
  $("cfg-conservative-waiting").value = cfg.conservative_waiting_transition_ms;
  $("cfg-auto-refresh-interval").value = cfg.lark_auto_refresh_interval_ms || 5000;
  $("cfg-headless-snapshot-timeout").value = cfg.headless_snapshot_timeout_ms || 10000;
  $("cfg-lark-max-lines").value = cfg.lark_notify_max_lines;
  $("cfg-lark-fallback-tail-lines").value = cfg.lark_notify_fallback_tail_lines || 100;
  $("cfg-lark-merge-wrapped-lines").checked = Boolean(cfg.lark_notify_merge_wrapped_lines);
  $("cfg-lark-app-id").value = cfg.lark_app_id || "";
  $("cfg-lark-app-secret").value = cfg.lark_app_secret || "";
  $("cfg-lark-receive-id").value = cfg.lark_notify_receive_id || "";
  $("cfg-lark-default-session-name").value = cfg.lark_default_session_name || "";
  $("cfg-lark-session-chat-prefix").value = cfg.lark_session_chat_prefix || "Iris · ";
  $("cfg-lark-ignore-prefix").value = cfg.lark_ignore_message_prefix || "/i";
  $("cfg-lark-auto-summary-prompt").value = cfg.lark_auto_summary_prompt || "";
  $("cfg-lark-mention-enabled").checked = Boolean(cfg.lark_mention_enabled);
  $("cfg-prestart-command").value = cfg.session_pre_start_command || "";
  $("cfg-drop-patterns").value = JSON.stringify(normalizeDropRules(cfg.lark_notify_drop_line_patterns || []), null, 2);
  $("cfg-lark-custom-shortcuts").value = JSON.stringify(cfg.lark_custom_shortcuts || [], null, 2);
  $("cfg-session-name-presets").value = JSON.stringify(cfg.session_name_presets || {}, null, 2);
  $("cfg-session-start-presets").value = JSON.stringify(cfg.session_start_presets || {}, null, 2);
  $("cfg-default-workspace-dir").value = cfg.default_workspace_dir || "";
  $("cfg-workspace-options").value = JSON.stringify(cfg.workspace_options || [], null, 2);
  renderDropRules();
  renderCustomShortcuts();
  renderWorkspaceOptions();
  renderSecurityStatus();
  renderLarkPermissionGuide();
  renderPreStartCommandRows();
  renderNamePresets();
  renderStartPresets();
  state.startupJSONDirty = false;
  updateStartupJSONPreview();
  $("config-error").textContent = "";
}

function larkAppConsoleURL(appID) {
  const id = String(appID || "").trim();
  if (!id) return "https://open.feishu.cn/app";
  return `https://open.feishu.cn/app/${encodeURIComponent(id)}/auth`;
}

function renderLarkPermissionGuide() {
  const appID = $("cfg-lark-app-id").value.trim();
  $("lark-app-console-link").href = larkAppConsoleURL(appID);
  $("lark-permission-status").textContent = appID
    ? "打开后台后进入权限管理，开通以上两个应用身份权限，然后发布应用版本。"
    : "先扫码或填写 App ID，再打开应用后台。";
}

function setConfigTab(targetID) {
  document.querySelectorAll(".config-tab").forEach((item) => item.classList.toggle("active", item.dataset.configTarget === targetID));
  document.querySelectorAll(".config-panel").forEach((panel) => panel.classList.toggle("active", panel.id === targetID));
  updateConfigStepButtons(targetID);
}

function activeConfigTabID() {
  const active = Array.from(document.querySelectorAll(".config-tab")).find((tab) => /\bactive\b/.test(tab.className));
  return active?.dataset.configTarget || CONFIG_TAB_IDS[0];
}

function updateConfigStepButtons(targetID = activeConfigTabID()) {
  const index = CONFIG_TAB_IDS.indexOf(targetID);
  const prev = $("config-prev");
  const next = $("config-next");
  if (!prev || !next) return;
  prev.disabled = index <= 0;
  next.disabled = index < 0 || index >= CONFIG_TAB_IDS.length - 1;
}

function moveConfigStep(delta) {
  const index = CONFIG_TAB_IDS.indexOf(activeConfigTabID());
  if (index < 0) return;
  const nextIndex = Math.min(CONFIG_TAB_IDS.length - 1, Math.max(0, index + delta));
  if (nextIndex !== index) setConfigTab(CONFIG_TAB_IDS[nextIndex]);
}

async function openConfigDialog(targetID = "config-lark") {
  if (!state.config) await loadConfig();
  renderConfig();
  setConfigTab(targetID);
  $("config-dialog").showModal();
}

function settingsAuthURL(path) {
  const next = `${location.pathname}${location.search}`;
  return `${path}?next=${encodeURIComponent(next)}`;
}

async function ensureSettingsAccess(openSettings = true) {
  const status = await loadSecurityStatus();
	if (!status.configured) {
		location.assign(settingsAuthURL("/setup-password"));
    return false;
  }
  if (!status.authenticated) {
		location.assign(settingsAuthURL("/login"));
    return false;
  }
  await loadConfig();
  if (openSettings) await openConfigDialog();
  return true;
}

async function maybeShowOnboarding() {
  if (!state.config || state.config.onboarding_completed) return;
  if ($("config-dialog").open || $("help-dialog").open) return;
  $("onboarding-default-session-name").value = state.config.lark_default_session_name || DEFAULT_SESSION_NAME;
  $("onboarding-agent-preset").value = state.config.agent_kind || "codex";
  $("onboarding-agent-custom-name").value = state.config.agent_kind === "custom" ? (state.config.agent_name || "自定义 Agent") : "";
  $("onboarding-agent-custom-command").value = state.config.agent_kind === "custom" ? (state.config.agent_command || "") : "";
  renderOnboardingAgentControls();
  $("onboarding-dialog").showModal();
}

function readNumber(id, fallback) {
  const raw = String($(id).value || "").trim();
  const n = raw === "" ? Number(fallback) : Number(raw);
  if (!Number.isFinite(n) || n <= 0) throw new Error("配置里存在无效数字");
  return Math.trunc(n);
}

function readConfigForm() {
  if (state.startupJSONDirty) syncStartupJSONPreview({ throwOnError: true });
  flushPendingPresetCommandEdits();
  const namePresets = parseJSONObject($("cfg-session-name-presets").value || "{}", "会话名预设 JSON");
  const startPresets = parseJSONObject($("cfg-session-start-presets").value || "{}", "开始命令后缀预设 JSON");
  const dropRules = parseJSONArray($("cfg-drop-patterns").value || "[]", "通知过滤规则 JSON")
    .map(normalizeDropRule)
    .filter((item) => item.title || item.pattern);
  const invalidGroupRule = dropRules.find((item) => item.kind === "line_group" && item.pattern && item.groups.length === 0);
  if (invalidGroupRule) {
    throw new Error("行内分组过滤需要填写分组编号");
  }
  const customShortcuts = parseJSONArray($("cfg-lark-custom-shortcuts").value || "[]", "飞书自定义快捷键 JSON")
    .map((item) => ({
      label: String(item?.label || "").trim(),
      command: String(item?.command || "").trim(),
    }))
    .filter((item) => item.label && item.command);
  const workspaces = parseJSONArray($("cfg-workspace-options").value || "[]", "工作目录配置")
    .map((item) => ({ label: String(item?.label || "").trim(), value: String(item?.value || "").trim(), default: Boolean(item?.default) }))
    .filter((item) => item.label || item.value);
  const agentKind = $("cfg-agent-preset").value;
  const agentName = agentKind === "custom" ? $("cfg-agent-custom-name").value.trim() : ({ codex: "Codex", claude: "Claude Code" }[agentKind] || "");
  const agentCommand = agentCommandForPreset(agentKind, $("cfg-agent-custom-command").value);
  if (!agentKind || !agentCommand) throw new Error("必须配置一个 Agent");
  if (!agentName) throw new Error("自定义 Agent 名称不能为空");
  return {
    lark_app_id: $("cfg-lark-app-id").value.trim(),
    lark_app_secret: $("cfg-lark-app-secret").value,
    lark_notify_receive_id: $("cfg-lark-receive-id").value.trim(),
    lark_mention_enabled: $("cfg-lark-mention-enabled").checked,
    lark_default_session_name: $("cfg-lark-default-session-name").value.trim(),
    lark_session_chat_prefix: $("cfg-lark-session-chat-prefix").value.trim(),
    lark_ignore_message_prefix: $("cfg-lark-ignore-prefix").value.trim(),
    lark_auto_summary_prompt: $("cfg-lark-auto-summary-prompt").value.trim(),
    fast_waiting_transition_ms: readNumber("cfg-fast-waiting", state.config?.fast_waiting_transition_ms || 1000),
    conservative_waiting_transition_ms: readNumber("cfg-conservative-waiting", state.config?.conservative_waiting_transition_ms || 3000),
    lark_auto_refresh_interval_ms: readNumber("cfg-auto-refresh-interval", state.config?.lark_auto_refresh_interval_ms || 5000),
    headless_snapshot_timeout_ms: readNumber("cfg-headless-snapshot-timeout", state.config?.headless_snapshot_timeout_ms || 10000),
    lark_notify_max_lines: readNumber("cfg-lark-max-lines", state.config?.lark_notify_max_lines || 100),
    lark_notify_fallback_tail_lines: readNumber("cfg-lark-fallback-tail-lines", state.config?.lark_notify_fallback_tail_lines || 100),
    lark_notify_merge_wrapped_lines: $("cfg-lark-merge-wrapped-lines").checked,
    session_pre_start_command: $("cfg-prestart-command").value,
    lark_notify_drop_line_patterns: dropRules,
    lark_custom_shortcuts: customShortcuts,
    onboarding_completed: Boolean(state.config?.onboarding_completed),
    session_name_presets: namePresets,
    session_start_presets: startPresets,
    agent_kind: agentKind,
    agent_name: agentName,
    agent_command: agentCommand,
    default_workspace_dir: $("cfg-default-workspace-dir").value.trim(),
    workspace_options: workspaces,
  };
}

function parseJSONArray(text, label) {
  let value;
  try {
    value = JSON.parse(text || "[]");
  } catch (err) {
    throw new Error(`${label} 格式不正确：${err.message}`);
  }
  if (!Array.isArray(value)) {
    throw new Error(`${label} 必须是 JSON 数组`);
  }
  return value;
}

function normalizeDropRules(value) {
  if (!Array.isArray(value)) return [];
  return value.map(normalizeDropRule);
}

function normalizeDropRule(item) {
  if (typeof item === "string") {
    return { title: "", kind: "line", pattern: item.trim(), action: "", groups: [] };
  }
  const kind = normalizeDropRuleKind(item?.kind);
  return {
    title: String(item?.title || "").trim(),
    kind,
    pattern: String(item?.pattern || "").trim(),
    action: kind === "block_head" ? normalizeDropRuleBlockAction(item?.action) : "",
    groups: kind === "line_group" ? normalizeDropRuleGroups(item?.groups) : [],
  };
}

function normalizeDropRuleKind(kind) {
  const value = String(kind || "").trim();
  return DROP_RULE_KINDS.some(([key]) => key === value) ? value : "line";
}

function normalizeDropRuleBlockAction(action) {
  const value = String(action || "").trim();
  return DROP_RULE_BLOCK_ACTIONS.some(([key]) => key === value) ? value : "drop_block";
}

function normalizeDropRuleGroups(value) {
  const raw = Array.isArray(value) ? value : String(value || "").split(/[,\s，、]+/);
  const groups = [];
  raw.forEach((item) => {
    const n = Number(item);
    if (Number.isInteger(n) && n > 0 && !groups.includes(n)) groups.push(n);
  });
  return groups;
}

function readDropRulesForUI() {
  try {
    const rules = JSON.parse($("cfg-drop-patterns").value || "[]");
    return normalizeDropRules(rules);
  } catch {
    return null;
  }
}

function writeDropRulesFromUI(rules) {
  $("cfg-drop-patterns").value = JSON.stringify(normalizeDropRules(rules || []), null, 2);
  renderDropRules();
}

function renderDropRules() {
  const list = $("drop-rule-list");
  list.innerHTML = "";
  const rules = readDropRulesForUI();
  if (!rules) {
    const err = document.createElement("div");
    err.className = "preset-empty";
    err.textContent = "通知过滤规则 JSON 格式不正确。";
    list.appendChild(err);
    return;
  }
  if (rules.length === 0) {
    const empty = document.createElement("div");
    empty.className = "preset-empty";
    empty.textContent = "还没有过滤规则。";
    list.appendChild(empty);
    return;
  }
  rules.forEach((rule, index) => {
    const row = document.createElement("div");
    row.className = "drop-rule-row";
    const kind = document.createElement("select");
    kind.className = "drop-rule-kind";
    kind.title = "过滤类型";
    DROP_RULE_KINDS.forEach(([value, label]) => {
      const option = document.createElement("option");
      option.value = value;
      option.textContent = label;
      kind.appendChild(option);
    });
    kind.value = rule.kind || "line";
    const title = document.createElement("input");
    title.className = "drop-rule-title";
    title.placeholder = "标题";
    title.title = "规则标题";
    title.value = rule.title || "";
    const pattern = document.createElement("input");
    pattern.className = "drop-rule-pattern";
    pattern.placeholder = "正则表达式";
    pattern.title = "正则表达式";
    pattern.value = rule.pattern || "";
    const extra = document.createElement("div");
    extra.className = "drop-rule-extra";
    if (rule.kind === "block_head") {
      const action = document.createElement("select");
      action.className = "drop-rule-action";
      action.title = "块匹配后的展示方式";
      DROP_RULE_BLOCK_ACTIONS.forEach(([value, label]) => {
        const option = document.createElement("option");
        option.value = value;
        option.textContent = label;
        action.appendChild(option);
      });
      action.value = rule.action || "drop_block";
      action.onchange = () => updateDropRule(index, { action: action.value }, false);
      extra.appendChild(action);
    } else if (rule.kind === "line_group") {
      const groups = document.createElement("input");
      groups.className = "drop-rule-groups";
      groups.placeholder = "分组 1,2";
      groups.title = "要隐藏的捕获分组编号";
      groups.value = (rule.groups || []).join(",");
      groups.oninput = () => updateDropRule(index, { groups: normalizeDropRuleGroups(groups.value) }, false);
      extra.appendChild(groups);
    } else {
      const hint = document.createElement("span");
      hint.className = "drop-rule-extra-hint";
      hint.textContent = "整行隐藏";
      extra.appendChild(hint);
    }
    const remove = document.createElement("button");
    remove.className = "drop-rule-remove";
    remove.type = "button";
    remove.textContent = "删除";
    kind.onchange = () => updateDropRule(index, { kind: kind.value }, true);
    title.oninput = () => updateDropRule(index, { title: title.value }, false);
    pattern.oninput = () => updateDropRule(index, { pattern: pattern.value }, false);
    remove.onclick = () => deleteDropRule(index);
    row.appendChild(kind);
    row.appendChild(title);
    row.appendChild(pattern);
    row.appendChild(extra);
    row.appendChild(remove);
    list.appendChild(row);
  });
}

function addDropRule() {
  const rules = readDropRulesForUI();
  if (!rules) return;
  rules.push({ title: "", kind: "line", pattern: "", action: "", groups: [] });
  writeDropRulesFromUI(rules);
}

function updateDropRule(index, patch, rerender) {
  const rules = readDropRulesForUI();
  if (!rules || !rules[index]) return;
  rules[index] = normalizeDropRule({ ...rules[index], ...patch });
  $("cfg-drop-patterns").value = JSON.stringify(rules, null, 2);
  if (rerender) renderDropRules();
}

function deleteDropRule(index) {
  const rules = readDropRulesForUI();
  if (!rules) return;
  rules.splice(index, 1);
  writeDropRulesFromUI(rules);
}

function readCustomShortcutsForUI() {
  try {
    const shortcuts = JSON.parse($("cfg-lark-custom-shortcuts").value || "[]");
    if (!Array.isArray(shortcuts)) return null;
    return shortcuts;
  } catch {
    return null;
  }
}

function writeCustomShortcutsFromUI(shortcuts) {
  $("cfg-lark-custom-shortcuts").value = JSON.stringify(shortcuts || [], null, 2);
  renderCustomShortcuts();
}

function renderCustomShortcuts() {
  const list = $("custom-shortcut-list");
  list.innerHTML = "";
  const shortcuts = readCustomShortcutsForUI();
  if (!shortcuts) {
    const err = document.createElement("div");
    err.className = "preset-empty";
    err.textContent = "自定义快捷键 JSON 格式不正确。";
    list.appendChild(err);
    return;
  }
  if (shortcuts.length === 0) {
    const empty = document.createElement("div");
    empty.className = "preset-empty";
    empty.textContent = "还没有自定义快捷键。";
    list.appendChild(empty);
    return;
  }
  shortcuts.forEach((shortcut, index) => {
    const row = document.createElement("div");
    row.className = "custom-shortcut-row";
    const label = document.createElement("input");
    label.className = "custom-shortcut-label";
    label.placeholder = "按钮名称";
    const command = document.createElement("input");
    command.className = "custom-shortcut-command";
    command.placeholder = "要提交的指令";
    const remove = document.createElement("button");
    remove.className = "custom-shortcut-remove";
    remove.type = "button";
    remove.textContent = "删除";
    label.value = shortcut?.label || "";
    command.value = shortcut?.command || "";
    label.oninput = () => updateCustomShortcut(index, label.value, command.value);
    command.oninput = () => updateCustomShortcut(index, label.value, command.value);
    remove.onclick = () => deleteCustomShortcut(index);
    row.appendChild(label);
    row.appendChild(command);
    row.appendChild(remove);
    list.appendChild(row);
  });
}

function addCustomShortcut() {
  const shortcuts = readCustomShortcutsForUI();
  if (!shortcuts) return;
  shortcuts.push({ label: "", command: "" });
  writeCustomShortcutsFromUI(shortcuts);
}

function updateCustomShortcut(index, label, command) {
  const shortcuts = readCustomShortcutsForUI();
  if (!shortcuts || !shortcuts[index]) return;
  shortcuts[index] = { label, command };
  $("cfg-lark-custom-shortcuts").value = JSON.stringify(shortcuts, null, 2);
}

function deleteCustomShortcut(index) {
  const shortcuts = readCustomShortcutsForUI();
  if (!shortcuts) return;
  shortcuts.splice(index, 1);
  writeCustomShortcutsFromUI(shortcuts);
}

function parseJSONObject(text, label) {
  let value;
  try {
    value = JSON.parse(text || "{}");
  } catch (err) {
    throw new Error(`${label} 格式不正确：${err.message}`);
  }
  if (!value || typeof value !== "object" || Array.isArray(value)) {
    throw new Error(`${label} 必须是 JSON 对象`);
  }
  return value;
}

async function saveConfig() {
  const cfg = readConfigForm();
  state.config = await api("/api/config", { method: "PATCH", body: JSON.stringify(cfg) });
  renderConfig();
}

async function testLarkConfig() {
  $("lark-test-start").disabled = true;
  renderLarkTestResult({ steps: [{ name: "测试中", ok: true, message: "正在发送飞书测试通知..." }] });
  try {
    const result = await api("/api/config/lark-test", { method: "POST", body: JSON.stringify(readConfigForm()) });
    renderLarkTestResult(result);
  } finally {
    $("lark-test-start").disabled = false;
  }
}

function renderLarkTestResult(result) {
  const box = $("lark-test-result");
  box.innerHTML = "";
  for (const step of result.steps || []) {
    const row = document.createElement("div");
    row.className = `lark-test-step ${step.ok ? "ok" : "fail"}`;
    row.innerHTML = `<div><strong></strong><span></span></div>`;
    row.querySelector("strong").textContent = step.name || "";
    row.querySelector("span").textContent = step.message || "";
    box.appendChild(row);
  }
}

async function checkEnvironment() {
  const button = $("environment-check-start");
  button.disabled = true;
  button.textContent = "检测中…";
  renderEnvironmentCheckResult({ steps: [{ name: "环境检测", status: "checking", message: "正在检查当前环境…" }] });
  try {
    const result = await api("/api/environment-check", { method: "POST", body: JSON.stringify(readConfigForm()) });
    renderEnvironmentCheckResult(result);
    return result;
  } finally {
    button.disabled = false;
    button.textContent = "重新检测";
  }
}

function renderEnvironmentCheckResult(result) {
  const box = $("environment-check-result");
  box.innerHTML = "";
  for (const step of result.steps || []) {
    const row = document.createElement("div");
    const status = ["ok", "warning", "error", "checking"].includes(step.status) ? step.status : "warning";
    row.className = `environment-check-step ${status}`;
    row.innerHTML = `<div><strong></strong><span></span></div>`;
    row.querySelector("strong").textContent = step.name || "";
    row.querySelector("span").textContent = step.message || "";
    box.appendChild(row);
  }
}

function copyTextWithSelection(text) {
  const textarea = document.createElement("textarea");
  textarea.value = text;
  textarea.readOnly = true;
  textarea.style.cssText = "position:fixed;left:-9999px;top:0";
  document.body.appendChild(textarea);
  textarea.focus();
  textarea.select();
  textarea.setSelectionRange?.(0, text.length);
  let copied = false;
  try {
    copied = document.execCommand("copy");
  } catch {
  } finally {
    textarea.remove();
  }
  return copied;
}

async function copyText(text, okMessage) {
  if (copyTextWithSelection(text)) {
    $("lark-permission-status").textContent = okMessage;
    return;
  }
  try {
    if (navigator.clipboard?.writeText) {
      await navigator.clipboard.writeText(text);
      $("lark-permission-status").textContent = okMessage;
      return;
    }
  } catch {}
  $("lark-permission-status").textContent = `复制失败，请手动复制：${text}`;
}

function agentPresetCommand() {
  return agentCommandForPreset($("cfg-agent-preset").value, $("cfg-agent-custom-command").value);
}

function agentCommandForPreset(preset, customCommand = "") {
  if (preset === "custom") return customCommand.trim();
  return {
    codex: "codex --dangerously-bypass-approvals-and-sandbox",
    claude: "claude --dangerously-skip-permissions",
  }[preset] || "";
}

function presetForAgentCommand(command = "") {
  const normalized = String(command || "").trim();
  for (const preset of ["codex", "claude"]) {
    if (normalized === agentCommandForPreset(preset)) return { preset, customCommand: "" };
  }
  if (normalized) return { preset: "custom", customCommand: normalized };
  return { preset: "", customCommand: "" };
}

function renderDefaultAgentPresetFromStartPreset(startPresets = {}) {
  if (state.config?.agent_kind) return;
  const commands = Array.isArray(startPresets?.[DEFAULT_AGENT_PRESET_CODE]?.commands) ? startPresets[DEFAULT_AGENT_PRESET_CODE].commands : [];
  const firstCommand = commands.find((command) => String(command || "").trim());
  const matched = presetForAgentCommand(firstCommand || "");
  $("cfg-agent-preset").value = matched.preset || "codex";
  $("cfg-agent-custom-name").value = matched.preset === "custom" ? "自定义 Agent" : "";
  $("cfg-agent-custom-command").value = matched.customCommand;
}

function renderAgentPresetControls() {
  const custom = $("cfg-agent-preset").value === "custom";
  $("cfg-custom-agent-fields").hidden = !custom;
  $("cfg-custom-agent-fields").classList.toggle("hidden", !custom);
}

function setAgentPresetStatus(message, ok = null) {
  const el = $("agent-preset-status");
  el.textContent = message || "";
  el.classList.toggle("ok", ok === true);
  el.classList.toggle("fail", ok === false);
}

function ensureDefaultAgentPreset() {
  const custom = $("cfg-agent-preset").value === "custom";
  if (custom && !$("cfg-agent-custom-name").value.trim()) {
    setAgentPresetStatus("请填写自定义 Agent 名称。", false);
    return;
  }
  const command = agentPresetCommand();
  if (!command) {
    setAgentPresetStatus("请填写自定义 Agent 启动命令。", false);
    return;
  }
  setAgentPresetStatus(`新会话将自动启动：${command}`, true);
}

function renderOnboardingAgentControls() {
  const custom = $("onboarding-agent-preset").value === "custom";
  $("onboarding-custom-agent-fields").hidden = !custom;
  $("onboarding-custom-agent-fields").classList.toggle("hidden", !custom);
}

function setOnboardingAgentStatus(message, ok = null) {
  const el = $("onboarding-agent-status");
  el.textContent = message || "";
  el.classList.toggle("ok", ok === true);
  el.classList.toggle("fail", ok === false);
}

function setDefaultAgentPresetInConfig(cfg, preset, customCommand, defaultSessionName = "") {
  const command = agentCommandForPreset(preset, customCommand);
  const sessionName = (defaultSessionName || cfg.lark_default_session_name || DEFAULT_SESSION_NAME).trim() || DEFAULT_SESSION_NAME;
  const presets = cfg.session_start_presets && typeof cfg.session_start_presets === "object" && !Array.isArray(cfg.session_start_presets)
    ? { ...cfg.session_start_presets }
    : {};
  if (command) {
    presets[DEFAULT_AGENT_PRESET_CODE] = { commands: [command] };
  }
  return { ...cfg, lark_default_session_name: sessionName, session_start_presets: presets, onboarding_completed: true };
}

async function completeOnboarding(options = {}) {
  if (!state.config) await loadConfig();
  const preset = $("onboarding-agent-preset").value;
  const defaultSessionName = $("onboarding-default-session-name").value.trim() || DEFAULT_SESSION_NAME;
  const customName = $("onboarding-agent-custom-name").value.trim();
  const customCommand = $("onboarding-agent-custom-command").value.trim();
  if (preset === "custom" && (!customName || !customCommand)) {
    setOnboardingAgentStatus("请填写自定义 Agent 名称和启动命令。", false);
    return;
  }
  const command = agentCommandForPreset(preset, customCommand);
  const agentName = preset === "custom" ? customName : ({ codex: "Codex", claude: "Claude Code" }[preset] || "");
  state.config = {
    ...state.config,
    lark_default_session_name: defaultSessionName,
    agent_kind: preset,
    agent_name: agentName,
    agent_command: command,
    onboarding_completed: true,
  };
  state.config = await api("/api/config", { method: "PATCH", body: JSON.stringify(state.config) });
  $("onboarding-dialog").close();
  renderConfig();
}

function renderWorkspaceOptions() {
  const box = $("workspace-option-list");
  if (!box) return;
  box.innerHTML = "";
  let options = [];
  try { options = JSON.parse($("cfg-workspace-options").value || "[]"); } catch {}
  for (const [index, option] of options.entries()) {
    const row = document.createElement("div");
    row.className = "workspace-option-row";
    row.innerHTML = `
      <label><span>名称</span><input class="workspace-label"></label>
      <label><span>绝对路径</span><input class="workspace-value"></label>
      <button class="workspace-remove" type="button" aria-label="删除目录">删除</button>`;
    row.querySelector(".workspace-label").value = option.label || "";
    row.querySelector(".workspace-value").value = option.value || "";
    row.querySelectorAll("input").forEach((input) => input.oninput = syncWorkspaceOptions);
    row.querySelector(".workspace-remove").onclick = () => {
      options.splice(index, 1);
      $("cfg-workspace-options").value = JSON.stringify(options);
      renderWorkspaceOptions();
    };
    box.appendChild(row);
  }
}

function syncWorkspaceOptions() {
  const options = [...$("workspace-option-list").querySelectorAll(".workspace-option-row")].map((row) => ({
    label: row.querySelector(".workspace-label").value.trim(),
    value: row.querySelector(".workspace-value").value.trim(),
  }));
  $("cfg-workspace-options").value = JSON.stringify(options);
}

function addWorkspaceOption() {
  syncWorkspaceOptions();
  let options = [];
  try { options = JSON.parse($("cfg-workspace-options").value || "[]"); } catch {}
  options.push({ label: "", value: "" });
  $("cfg-workspace-options").value = JSON.stringify(options);
  renderWorkspaceOptions();
  $("workspace-option-list").lastElementChild?.querySelector(".workspace-label")?.focus();
}

function renderSecurityStatus() {
  const warning = $("settings-security-warning");
  if (!warning) return;
  warning.hidden = !state.security?.risk_warning;
  warning.classList.toggle("hidden", !state.security?.risk_warning);
}

function readNamePresetsForUI() {
  try {
    const presets = JSON.parse($("cfg-session-name-presets").value || "{}");
    if (!presets || typeof presets !== "object" || Array.isArray(presets)) return null;
    return presets;
  } catch {
    return null;
  }
}

function readStartPresetsForUI() {
  try {
    const presets = JSON.parse($("cfg-session-start-presets").value || "{}");
    if (!presets || typeof presets !== "object" || Array.isArray(presets)) return null;
    return presets;
  } catch {
    return null;
  }
}

function writeNamePresetsFromUI(presets) {
  $("cfg-session-name-presets").value = JSON.stringify(presets || {}, null, 2);
  renderNamePresets();
  state.startupJSONDirty = false;
  updateStartupJSONPreview();
}

function writeStartPresetsFromUI(presets) {
  $("cfg-session-start-presets").value = JSON.stringify(presets || {}, null, 2);
  renderStartPresets();
  renderDefaultAgentPresetFromStartPreset(presets || {});
  renderAgentPresetControls();
  state.startupJSONDirty = false;
  updateStartupJSONPreview();
}

function setPresetStatus(message, ok = null) {
  const el = $("preset-status");
  el.textContent = message || "";
  el.classList.toggle("ok", ok === true);
  el.classList.toggle("fail", ok === false);
}

function setStartPresetStatus(message, ok = null) {
  const el = $("start-preset-status");
  el.textContent = message || "";
  el.classList.toggle("ok", ok === true);
  el.classList.toggle("fail", ok === false);
}

function saveNamePresetFromForm() {
  const presets = readNamePresetsForUI();
  if (!presets) {
    setPresetStatus("会话名预设 JSON 格式不正确，先修正后再添加会话名。", false);
    return;
  }
  const name = $("preset-session-name").value.trim();
  if (!name) {
    setPresetStatus("请填写会话名。", false);
    return;
  }
  if (presets[name]) {
    setPresetStatus(`“${name}”已存在，可以直接在下方添加命令。`, null);
    return;
  }
  presets[name] = { commands: [] };
  writeNamePresetsFromUI(presets);
  $("preset-session-name").value = "";
  setPresetStatus(`已添加会话名“${name}”。`, true);
}

function clearNamePresetForm() {
  $("preset-session-name").value = "";
  state.editingPresetCommand = null;
  renderNamePresets();
  setPresetStatus("");
}

function addPresetCommand(name, command) {
  const presets = readNamePresetsForUI();
  if (!presets?.[name]) return;
  const value = command.trim();
  if (!value) {
    setPresetStatus(`请填写“${name}”要添加的命令。`, false);
    return;
  }
  if (!Array.isArray(presets[name].commands)) presets[name].commands = [];
  presets[name].commands.push(value);
  writeNamePresetsFromUI(presets);
  setPresetStatus(`已为“${name}”添加命令。`, true);
}

function editPresetCommand(name, index) {
  state.editingPresetCommand = { name, index };
  renderNamePresets();
  setPresetStatus(`正在编辑“${name}”第 ${index + 1} 条命令。`, null);
}

function updatePresetCommand(name, index, command) {
  const presets = readNamePresetsForUI();
  const commands = presets?.[name]?.commands;
  if (!Array.isArray(commands)) return false;
  const value = command.trim();
  if (!value) {
    setPresetStatus("命令不能为空。", false);
    return false;
  }
  commands[index] = value;
  state.editingPresetCommand = null;
  writeNamePresetsFromUI(presets);
  setPresetStatus(`已更新“${name}”第 ${index + 1} 条命令。`, true);
  return true;
}

function cancelEditPresetCommand() {
  state.editingPresetCommand = null;
  renderNamePresets();
  setPresetStatus("");
}

function addCommandRow(container, value = "", onChange) {
  const row = document.createElement("div");
  row.className = "preset-command-row";
  row.innerHTML = `
    <input class="preset-command-input" placeholder="codex">
    <button class="preset-command-remove" type="button">删除</button>
  `;
  const input = row.querySelector(".preset-command-input");
  input.value = value;
  input.oninput = () => {
    if (typeof onChange === "function") onChange();
  };
  row.querySelector(".preset-command-remove").onclick = () => {
    row.remove();
    if (typeof onChange === "function") onChange();
    if (container.querySelectorAll(".preset-command-input").length === 0) {
      addCommandRow(container, "", onChange);
    }
  };
  container.appendChild(row);
}

function renderPreStartCommandRows() {
  const commands = $("cfg-prestart-command").value.split("\n");
  const list = $("prestart-command-list");
  list.innerHTML = "";
  const rows = commands.some((line) => line.trim()) ? commands : [""];
  for (const command of rows) addCommandRow(list, command, syncPreStartCommandsFromRows);
}

function addPreStartCommandRow(value = "") {
  addCommandRow($("prestart-command-list"), value, syncPreStartCommandsFromRows);
  syncPreStartCommandsFromRows();
}

function syncPreStartCommandsFromRows() {
  $("cfg-prestart-command").value = [...$("prestart-command-list").querySelectorAll(".preset-command-input")]
    .map((input) => input.value.trim())
    .filter(Boolean)
    .join("\n");
  state.startupJSONDirty = false;
  updateStartupJSONPreview();
}

function deletePresetCommand(name, index) {
  const presets = readNamePresetsForUI();
  const commands = presets?.[name]?.commands;
  if (!Array.isArray(commands)) return;
  commands.splice(index, 1);
  writeNamePresetsFromUI(presets);
  setPresetStatus(`已删除“${name}”的第 ${index + 1} 条命令。`, true);
}

function deletePresetSession(name) {
  const presets = readNamePresetsForUI();
  if (!presets?.[name]) return;
  delete presets[name];
  if (state.editingPresetCommand?.name === name) state.editingPresetCommand = null;
  writeNamePresetsFromUI(presets);
  setPresetStatus(`已删除会话名“${name}”。`, true);
}

function renderNamePresets() {
  const list = $("preset-list");
  list.innerHTML = "";
  const presets = readNamePresetsForUI();
  if (!presets) {
    setPresetStatus("会话名预设 JSON 格式不正确，列表无法同步。", false);
    return;
  }
  const names = Object.keys(presets).sort((a, b) => a.localeCompare(b));
  if (names.length === 0) {
    const empty = document.createElement("div");
    empty.className = "preset-empty";
    empty.textContent = "还没有会话名预设。";
    list.appendChild(empty);
    return;
  }
  for (const name of names) {
    const commands = Array.isArray(presets[name]?.commands) ? presets[name].commands : [];
    const item = document.createElement("article");
    item.className = "preset-item";
    const head = document.createElement("div");
    head.className = "preset-item-head";
    const title = document.createElement("strong");
    title.textContent = name;
    const deleteSession = document.createElement("button");
    deleteSession.type = "button";
    deleteSession.className = "preset-session-delete";
    deleteSession.textContent = "删除会话名";
    deleteSession.onclick = () => deletePresetSession(name);
    head.appendChild(title);
    head.appendChild(deleteSession);
    item.appendChild(head);
    const display = document.createElement("div");
    display.className = "preset-command-display";
    item.appendChild(display);
    if (commands.length === 0) {
      const empty = document.createElement("div");
      empty.className = "preset-command-display-row";
      empty.textContent = "未配置命令";
      display.appendChild(empty);
    }
    commands.forEach((command, index) => {
      const row = document.createElement("div");
      row.className = "preset-command-display-row";
      const editing = state.editingPresetCommand?.name === name && state.editingPresetCommand.index === index;
      const commandEl = editing ? document.createElement("input") : document.createElement("code");
      if (editing) {
        commandEl.className = "preset-inline-command-input";
        commandEl.value = command;
      } else {
        commandEl.textContent = command;
      }
      const actions = document.createElement("div");
      actions.className = "preset-item-actions";
      if (editing) {
        const save = document.createElement("button");
        save.type = "button";
        save.textContent = "保存";
        save.onclick = () => updatePresetCommand(name, index, commandEl.value);
        const cancel = document.createElement("button");
        cancel.type = "button";
        cancel.textContent = "取消";
        cancel.onclick = cancelEditPresetCommand;
        actions.appendChild(save);
        actions.appendChild(cancel);
      } else {
        const edit = document.createElement("button");
        edit.className = "preset-edit";
        edit.type = "button";
        edit.textContent = "编辑";
        edit.onclick = () => editPresetCommand(name, index);
        const remove = document.createElement("button");
        remove.className = "preset-delete";
        remove.type = "button";
        remove.textContent = "删除";
        remove.onclick = () => deletePresetCommand(name, index);
        actions.appendChild(edit);
        actions.appendChild(remove);
      }
      row.appendChild(commandEl);
      row.appendChild(actions);
      display.appendChild(row);
    });
    const addRow = document.createElement("div");
    addRow.className = "preset-command-add-row";
    const input = document.createElement("input");
    input.className = "preset-new-command-input";
    input.placeholder = `给“${name}”添加命令`;
    const add = document.createElement("button");
    add.type = "button";
    add.textContent = "添加命令";
    add.onclick = () => addPresetCommand(name, input.value);
    addRow.appendChild(input);
    addRow.appendChild(add);
    item.appendChild(addRow);
    list.appendChild(item);
  }
}

function normalizeStartPresetCodeForUI(code) {
  return String(code || "").trim();
}

function validateStartPresetCodeForUI(code) {
  if (/\s/.test(code)) return "标识不能包含空格。";
  if (/[,+，＋]/.test(code)) return "标识不能包含逗号或加号；这些符号用于组合多个预设。";
  if (code === "0") return "0 保留为仅进入目录，不需要配置命令。";
  return "";
}

function compareStartPresetCodes(a, b) {
  const an = Number(a);
  const bn = Number(b);
  if (Number.isSafeInteger(an) && Number.isSafeInteger(bn) && an !== bn) return an - bn;
  if (a.length !== b.length) return a.length - b.length;
  return a.localeCompare(b);
}

function startPresetTitle(code) {
  if (code === DEFAULT_AGENT_PRESET_CODE) return `${code}（默认 Agent）`;
  return code;
}

function saveStartPresetFromForm() {
  const presets = readStartPresetsForUI();
  if (!presets) {
    setStartPresetStatus("启动指令预设 JSON 格式不正确，先修正后再添加标识。", false);
    return;
  }
  const code = normalizeStartPresetCodeForUI($("start-preset-code").value);
  if (!code) {
    setStartPresetStatus("请填写标识。", false);
    return;
  }
  const invalid = validateStartPresetCodeForUI(code);
  if (invalid) {
    setStartPresetStatus(invalid, false);
    return;
  }
  if (presets[code]) {
    setStartPresetStatus(`“${code}”已存在，可以直接在下方添加命令。`, null);
    return;
  }
  presets[code] = { commands: [] };
  writeStartPresetsFromUI(presets);
  $("start-preset-code").value = "";
  setStartPresetStatus(`已添加“${code}”启动指令预设。`, true);
}

function clearStartPresetForm() {
  $("start-preset-code").value = "";
  state.editingStartPresetCommand = null;
  renderStartPresets();
  setStartPresetStatus("");
}

function addStartPresetCommand(code, command) {
  const presets = readStartPresetsForUI();
  if (!presets?.[code]) return;
  const value = command.trim();
  if (!value) {
    setStartPresetStatus(`请填写“${code}”要添加的命令。`, false);
    return;
  }
  if (!Array.isArray(presets[code].commands)) presets[code].commands = [];
  presets[code].commands.push(value);
  writeStartPresetsFromUI(presets);
  setStartPresetStatus(`已为“${code}”添加命令。`, true);
}

function editStartPresetCommand(code, index) {
  state.editingStartPresetCommand = { code, index };
  renderStartPresets();
  setStartPresetStatus(`正在编辑“${code}”第 ${index + 1} 条命令。`, null);
}

function updateStartPresetCommand(code, index, command) {
  const presets = readStartPresetsForUI();
  const commands = presets?.[code]?.commands;
  if (!Array.isArray(commands)) return false;
  const value = command.trim();
  if (!value) {
    setStartPresetStatus("命令不能为空。", false);
    return false;
  }
  commands[index] = value;
  state.editingStartPresetCommand = null;
  writeStartPresetsFromUI(presets);
  setStartPresetStatus(`已更新“${code}”第 ${index + 1} 条命令。`, true);
  return true;
}

function flushPendingPresetCommandEdits() {
  const pendingNameEdit = state.editingPresetCommand;
  if (pendingNameEdit) {
    const input = $("preset-list").querySelector(".preset-inline-command-input");
    if (input && !updatePresetCommand(pendingNameEdit.name, pendingNameEdit.index, input.value)) {
      throw new Error("会话名预设命令不能为空");
    }
  }
  const pendingStartEdit = state.editingStartPresetCommand;
  if (pendingStartEdit) {
    const input = $("start-preset-list").querySelector(".preset-inline-command-input");
    if (input && !updateStartPresetCommand(pendingStartEdit.code, pendingStartEdit.index, input.value)) {
      throw new Error("启动指令预设命令不能为空");
    }
  }
}

function cancelEditStartPresetCommand() {
  state.editingStartPresetCommand = null;
  renderStartPresets();
  setStartPresetStatus("");
}

function deleteStartPresetCommand(code, index) {
  const presets = readStartPresetsForUI();
  const commands = presets?.[code]?.commands;
  if (!Array.isArray(commands)) return;
  commands.splice(index, 1);
  writeStartPresetsFromUI(presets);
  setStartPresetStatus(`已删除“${code}”的第 ${index + 1} 条命令。`, true);
}

function deleteStartPresetCode(code) {
  const presets = readStartPresetsForUI();
  if (!presets?.[code]) return;
  delete presets[code];
  if (state.editingStartPresetCommand?.code === code) state.editingStartPresetCommand = null;
  writeStartPresetsFromUI(presets);
  setStartPresetStatus(`已删除“${code}”启动指令预设。`, true);
}

function renderStartPresets() {
  const list = $("start-preset-list");
  list.innerHTML = "";
  const presets = readStartPresetsForUI();
  if (!presets) {
    setStartPresetStatus("启动指令预设 JSON 格式不正确，列表无法同步。", false);
    return;
  }
  const codes = Object.keys(presets).sort(compareStartPresetCodes);
  if (codes.length === 0) {
    const empty = document.createElement("div");
    empty.className = "preset-empty";
    empty.textContent = "还没有启动指令预设。";
    list.appendChild(empty);
    return;
  }
  for (const code of codes) {
    const commands = Array.isArray(presets[code]?.commands) ? presets[code].commands : [];
    const item = document.createElement("article");
    item.className = "preset-item";
    const head = document.createElement("div");
    head.className = "preset-item-head";
    const title = document.createElement("strong");
    title.textContent = startPresetTitle(code);
    const deleteCode = document.createElement("button");
    deleteCode.type = "button";
    deleteCode.className = "preset-session-delete";
    deleteCode.textContent = "删除编号";
    deleteCode.onclick = () => deleteStartPresetCode(code);
    head.appendChild(title);
    head.appendChild(deleteCode);
    item.appendChild(head);
    const display = document.createElement("div");
    display.className = "preset-command-display";
    item.appendChild(display);
    if (commands.length === 0) {
      const empty = document.createElement("div");
      empty.className = "preset-command-display-row";
      empty.textContent = "未配置命令";
      display.appendChild(empty);
    }
    commands.forEach((command, index) => {
      const row = document.createElement("div");
      row.className = "preset-command-display-row";
      const editing = state.editingStartPresetCommand?.code === code && state.editingStartPresetCommand.index === index;
      const commandEl = editing ? document.createElement("input") : document.createElement("code");
      if (editing) {
        commandEl.className = "preset-inline-command-input";
        commandEl.value = command;
      } else {
        commandEl.textContent = command;
      }
      const actions = document.createElement("div");
      actions.className = "preset-item-actions";
      if (editing) {
        const save = document.createElement("button");
        save.type = "button";
        save.textContent = "保存";
        save.onclick = () => updateStartPresetCommand(code, index, commandEl.value);
        const cancel = document.createElement("button");
        cancel.type = "button";
        cancel.textContent = "取消";
        cancel.onclick = cancelEditStartPresetCommand;
        actions.appendChild(save);
        actions.appendChild(cancel);
      } else {
        const edit = document.createElement("button");
        edit.className = "preset-edit";
        edit.type = "button";
        edit.textContent = "编辑";
        edit.onclick = () => editStartPresetCommand(code, index);
        const remove = document.createElement("button");
        remove.className = "preset-delete";
        remove.type = "button";
        remove.textContent = "删除";
        remove.onclick = () => deleteStartPresetCommand(code, index);
        actions.appendChild(edit);
        actions.appendChild(remove);
      }
      row.appendChild(commandEl);
      row.appendChild(actions);
      display.appendChild(row);
    });
    const addRow = document.createElement("div");
    addRow.className = "preset-command-add-row";
    const input = document.createElement("input");
    input.className = "preset-new-command-input";
    input.placeholder = `给“${code}”添加命令`;
    const add = document.createElement("button");
    add.type = "button";
    add.textContent = "添加命令";
    add.onclick = () => addStartPresetCommand(code, input.value);
    addRow.appendChild(input);
    addRow.appendChild(add);
    item.appendChild(addRow);
    list.appendChild(item);
  }
}

function startupJSONValue() {
  return {
    session_pre_start_command: $("cfg-prestart-command").value,
    session_name_presets: parseJSONObject($("cfg-session-name-presets").value || "{}", "会话名预设 JSON"),
    session_start_presets: parseJSONObject($("cfg-session-start-presets").value || "{}", "开始命令后缀预设 JSON"),
  };
}

function updateStartupJSONPreview() {
  if (state.startupJSONDirty) return;
  $("startup-json-preview").value = JSON.stringify(startupJSONValue(), null, 2);
}

function syncStartupJSONPreview(options = {}) {
  const preview = $("startup-json-preview");
  let value;
  try {
    value = JSON.parse(preview.value || "{}");
  } catch (err) {
    const msg = `启动配置 JSON 格式不正确：${err.message}`;
    if (options.throwOnError) throw new Error(msg);
    $("config-error").textContent = msg;
    return false;
  }
  if (!value || typeof value !== "object" || Array.isArray(value)) {
    const msg = "启动配置 JSON 必须是对象";
    if (options.throwOnError) throw new Error(msg);
    $("config-error").textContent = msg;
    return false;
  }
  const preStart = value.session_pre_start_command ?? "";
  if (typeof preStart !== "string") {
    const msg = "session_pre_start_command 必须是字符串";
    if (options.throwOnError) throw new Error(msg);
    $("config-error").textContent = msg;
    return false;
  }
  const namePresets = value.session_name_presets ?? {};
  const startPresets = value.session_start_presets ?? {};
  if (!namePresets || typeof namePresets !== "object" || Array.isArray(namePresets)) {
    const msg = "session_name_presets 必须是对象";
    if (options.throwOnError) throw new Error(msg);
    $("config-error").textContent = msg;
    return false;
  }
  if (!startPresets || typeof startPresets !== "object" || Array.isArray(startPresets)) {
    const msg = "session_start_presets 必须是对象";
    if (options.throwOnError) throw new Error(msg);
    $("config-error").textContent = msg;
    return false;
  }
  $("cfg-prestart-command").value = preStart;
  $("cfg-session-name-presets").value = JSON.stringify(namePresets, null, 2);
  $("cfg-session-start-presets").value = JSON.stringify(startPresets, null, 2);
  state.startupJSONDirty = false;
  renderPreStartCommandRows();
  renderNamePresets();
  renderStartPresets();
  renderDefaultAgentPresetFromStartPreset(startPresets);
  renderAgentPresetControls();
  $("config-error").textContent = "";
  if (!options.keepText) updateStartupJSONPreview();
  return true;
}

function toggleStartupJSONPreview() {
  const preview = $("startup-json-preview");
  const hidden = !preview.hidden;
  preview.hidden = hidden;
  preview.classList.toggle("hidden", hidden);
  $("startup-json-toggle").textContent = hidden ? "查看启动配置 JSON" : "隐藏启动配置 JSON";
  if (!hidden) updateStartupJSONPreview();
}

function renderQuick() {
  $("quick-list").innerHTML = "";
  for (const q of state.quick) {
    const chip = document.createElement("div");
    chip.className = "quick-chip";
    chip.title = q.text;
    chip.innerHTML = `<span></span><button class="chip-close" type="button" title="删除">×</button>`;
    chip.querySelector("span").textContent = q.text;
    chip.onclick = () => {
      $("composer-input").value = q.text;
      $("composer-input").focus();
    };
    chip.querySelector(".chip-close").onclick = async (ev) => {
      ev.stopPropagation();
      await api(`/api/quick-commands/${q.id}`, { method: "DELETE" });
      await loadQuick();
    };
    $("quick-list").appendChild(chip);
  }
  const add = document.createElement("button");
  add.className = "add-quick";
  add.type = "button";
  add.title = "添加快捷命令";
  add.textContent = "+";
  add.onclick = () => $("quick-dialog").showModal();
  $("quick-list").appendChild(add);
}

async function setNotify(id, enabled) {
  await api(`/api/sessions/${id}`, { method: "PATCH", body: JSON.stringify({ notify_on_waiting: enabled }) });
  await loadSessions();
}

async function deleteSession(id) {
  await api(`/api/sessions/${id}`, { method: "DELETE" });
  if (state.active === id) {
    state.active = null;
    if (state.socket) state.socket.close();
    if (state.term) state.term.clear();
    $("active-title").textContent = "请选择会话";
  }
  await loadSessions();
}

$("new-session").onsubmit = async (ev) => {
  ev.preventDefault();
  const name = $("session-name").value.trim();
  if (!name) return;
  const s = await api("/api/sessions", { method: "POST", body: JSON.stringify({ name }) });
  $("session-name").value = "";
  await loadSessions();
  selectSession(s.id);
};

$("session-search").oninput = (ev) => {
  state.search = ev.target.value;
  renderSessions();
};

$("composer").onsubmit = (ev) => {
  ev.preventDefault();
  sendComposer();
};

$("composer-input").onkeydown = (ev) => {
  if (ev.key === "Enter" && (ev.metaKey || ev.ctrlKey)) {
    ev.preventDefault();
    sendComposer();
  }
};

function sendComposer() {
  const input = $("composer-input");
  const text = input.value;
  if (!text || !state.active) return;
  sendWS({ type: "submit", data: text });
  input.value = "";
  input.focus();
}

$("quick-form").onsubmit = async (ev) => {
  ev.preventDefault();
  const text = $("quick-text").value.trim();
  if (!text) return;
  const name = text.length > 40 ? `${text.slice(0, 37)}...` : text;
  await api("/api/quick-commands", { method: "POST", body: JSON.stringify({ name, text }) });
  $("quick-text").value = "";
  $("quick-dialog").close();
  await loadQuick();
};

$("quick-cancel").onclick = () => $("quick-dialog").close();

$("config-open").onclick = async () => {
  try {
    await ensureSettingsAccess(true);
  } catch (err) {
    console.error(err);
  }
};

$("settings-password-save").onclick = async () => {
  const status = $("settings-password-status");
  const newPassword = $("settings-new-password").value;
  const confirmPassword = $("settings-confirm-password").value;
  if (newPassword.length < 8) {
    status.textContent = "新密码至少需要 8 个字符。";
    status.className = "agent-preset-status fail";
    $("settings-new-password").focus();
    return;
  }
  if (newPassword !== confirmPassword) {
    status.textContent = "两次输入的新密码不一致。";
    status.className = "agent-preset-status fail";
    $("settings-confirm-password").focus();
    return;
  }
  try {
    await api("/api/settings/security/password", { method: "POST", body: JSON.stringify({
      current_password: $("settings-current-password").value,
      new_password: newPassword,
      confirm_password: confirmPassword,
    }) });
    $("settings-current-password").value = "";
    $("settings-new-password").value = "";
    $("settings-confirm-password").value = "";
    renderSettingsPasswordMatch();
    await loadSecurityStatus();
    renderSecurityStatus();
    status.textContent = "密码已更新，其他设置登录已失效。";
    status.className = "agent-preset-status ok";
  } catch (err) {
    status.textContent = err.message || String(err);
    status.className = "agent-preset-status fail";
  }
};

function renderSettingsPasswordMatch() {
  const hint = $("settings-password-match");
  const password = $("settings-new-password").value;
  const confirmation = $("settings-confirm-password").value;
  hint.className = "password-match-hint";
  if (!confirmation) {
    hint.textContent = "";
    return;
  }
  const matches = password === confirmation;
  hint.textContent = matches ? "两次输入一致。" : "两次输入的新密码不一致。";
  hint.classList.add(matches ? "ok" : "fail");
}

$("settings-new-password").oninput = renderSettingsPasswordMatch;
$("settings-confirm-password").oninput = renderSettingsPasswordMatch;

$("config-cancel").onclick = () => {
  stopLarkRegistrationPolling();
  $("config-dialog").close();
};

$("config-prev").onclick = () => moveConfigStep(-1);
$("config-next").onclick = () => moveConfigStep(1);

$("config-form").onsubmit = async (ev) => {
  ev.preventDefault();
  try {
    await saveConfig();
    $("config-dialog").close();
  } catch (err) {
    $("config-error").textContent = err.message || String(err);
  }
};

$("lark-register-start").onclick = () => startLarkRegistration().catch((err) => {
  $("lark-register-status").textContent = err.message || String(err);
});

$("lark-test-start").onclick = () => testLarkConfig().catch((err) => {
  renderLarkTestResult({ steps: [{ name: "测试失败", ok: false, message: err.message || String(err) }] });
  $("lark-test-start").disabled = false;
});

$("environment-check-start").onclick = () => checkEnvironment().catch((err) => {
  renderEnvironmentCheckResult({ steps: [{ name: "环境检测", status: "error", message: err.message || String(err) }] });
  $("environment-check-start").disabled = false;
  $("environment-check-start").textContent = "重新检测";
});

$("cfg-lark-app-id").oninput = renderLarkPermissionGuide;

$("lark-copy-contact-scope").onclick = () => copyText("contact:user.base:readonly", "已复制 Scope：contact:user.base:readonly");

$("lark-copy-group-scope").onclick = () => copyText("im:message.group_msg", "已复制 Scope：im:message.group_msg");

$("cfg-agent-preset").onchange = () => {
  renderAgentPresetControls();
  setAgentPresetStatus("");
  if ($("cfg-agent-preset").value === "custom" && !$("cfg-agent-custom-name").value.trim()) {
    $("cfg-agent-custom-name").focus();
    return;
  }
  ensureDefaultAgentPreset();
};

$("cfg-agent-custom-name").oninput = ensureDefaultAgentPreset;
$("cfg-agent-custom-command").onchange = ensureDefaultAgentPreset;

$("preset-save").onclick = saveNamePresetFromForm;
$("preset-clear").onclick = clearNamePresetForm;
$("start-preset-save").onclick = saveStartPresetFromForm;
$("start-preset-clear").onclick = clearStartPresetForm;
$("prestart-command-add").onclick = () => addPreStartCommandRow("");
$("drop-rule-add").onclick = addDropRule;
$("custom-shortcut-add").onclick = addCustomShortcut;
$("workspace-option-add").onclick = addWorkspaceOption;
$("startup-json-toggle").onclick = toggleStartupJSONPreview;
$("startup-json-preview").oninput = () => {
  state.startupJSONDirty = true;
  syncStartupJSONPreview({ keepText: true });
};
$("cfg-session-name-presets").oninput = () => {
  state.startupJSONDirty = false;
  renderNamePresets();
  updateStartupJSONPreview();
};
$("cfg-session-start-presets").oninput = () => {
  state.startupJSONDirty = false;
  renderStartPresets();
  renderDefaultAgentPresetFromStartPreset(readStartPresetsForUI() || {});
  renderAgentPresetControls();
  updateStartupJSONPreview();
};
$("cfg-prestart-command").oninput = () => {
  state.startupJSONDirty = false;
  renderPreStartCommandRows();
  updateStartupJSONPreview();
};

document.querySelectorAll(".config-tab").forEach((tab) => {
  tab.onclick = () => {
    setConfigTab(tab.dataset.configTarget);
  };
});

$("onboarding-agent-preset").onchange = () => {
  renderOnboardingAgentControls();
  setOnboardingAgentStatus("");
  if ($("onboarding-agent-preset").value === "custom" && !$("onboarding-agent-custom-name").value.trim()) {
    $("onboarding-agent-custom-name").focus();
  }
};

$("onboarding-agent-custom-name").oninput = () => setOnboardingAgentStatus("");
$("onboarding-agent-custom-command").oninput = () => setOnboardingAgentStatus("");

$("onboarding-config").onclick = async () => {
  try {
    await completeOnboarding();
  } catch (err) {
    setOnboardingAgentStatus(err.message || String(err), false);
  }
};

$("onboarding-later").onclick = async () => {
  try {
    await completeOnboarding({ skip: true });
  } catch (err) {
    setOnboardingAgentStatus(err.message || String(err), false);
  }
};

async function startLarkRegistration() {
  stopLarkRegistrationPolling();
  $("lark-register-start").disabled = true;
  $("lark-register-panel").hidden = false;
  $("lark-register-panel").classList.remove("hidden");
  $("lark-register-status").textContent = "正在创建扫码任务...";
  const reg = await api("/api/lark-app-registration", { method: "POST", body: JSON.stringify({ brand: "feishu" }) });
  $("lark-register-code").textContent = reg.user_code;
  $("lark-register-link").href = reg.verification_uri_complete;
  $("lark-register-qr").src = `/api/lark-app-registration/qr?text=${encodeURIComponent(reg.verification_uri_complete)}`;
  $("lark-register-status").textContent = "请用飞书扫码确认创建应用...";
  $("lark-register-panel").hidden = false;
  $("lark-register-panel").classList.remove("hidden");
  pollLarkRegistration(reg);
}

function pollLarkRegistration(reg) {
  const intervalMs = Math.max(Number(reg.interval || 5), 2) * 1000;
  const startedAt = Date.now();
  const expiresAt = startedAt + Math.max(Number(reg.expires_in || 3600), 60) * 1000;
  const tick = async () => {
    if (Date.now() >= expiresAt) {
      $("lark-register-status").textContent = "二维码已过期，请重新开始。";
      $("lark-register-start").disabled = false;
      return;
    }
    try {
      const result = await api("/api/lark-app-registration/poll", {
        method: "POST",
        body: JSON.stringify({ brand: reg.brand || "feishu", device_code: reg.device_code }),
      });
      if (result.pending) {
        const left = Math.max(0, Math.ceil((expiresAt - Date.now()) / 1000));
        $("lark-register-status").textContent = `等待扫码确认，剩余 ${left} 秒...`;
        state.larkRegistrationTimer = setTimeout(tick, intervalMs);
        return;
      }
      applyLarkRegistrationResult(result);
    } catch (err) {
      $("lark-register-status").textContent = err.message || String(err);
      $("lark-register-start").disabled = false;
    }
  };
  state.larkRegistrationTimer = setTimeout(tick, intervalMs);
}

function applyLarkRegistrationResult(result) {
  $("cfg-lark-app-id").value = result.app_id || "";
  $("cfg-lark-app-secret").value = result.app_secret || "";
  if (result.lark_notify_receive_id) $("cfg-lark-receive-id").value = result.lark_notify_receive_id;
  renderLarkPermissionGuide();
  $("lark-register-status").textContent = "扫码成功，已填入飞书应用配置。保存后请按下方企业级权限引导补开 im:message.group_msg。";
  $("lark-register-start").disabled = false;
  stopLarkRegistrationPolling();
}

function stopLarkRegistrationPolling() {
  if (state.larkRegistrationTimer) {
    clearTimeout(state.larkRegistrationTimer);
    state.larkRegistrationTimer = null;
  }
}

$("help-open").onclick = () => $("help-dialog").showModal();
$("help-close").onclick = () => $("help-dialog").close();

document.querySelectorAll(".help-tab").forEach((tab) => {
  tab.onclick = () => {
    const targetID = tab.dataset.helpTarget;
    document.querySelectorAll(".help-tab").forEach((item) => item.classList.toggle("active", item === tab));
    document.querySelectorAll(".help-panel").forEach((panel) => panel.classList.toggle("active", panel.id === targetID));
  };
});

function clipboardImageFile(ev) {
  const files = [...(ev.clipboardData?.files || [])];
  const fromFiles = files.find((f) => f.type.startsWith("image/"));
  if (fromFiles) return fromFiles;
  const items = [...(ev.clipboardData?.items || [])];
  const imageItem = items.find((item) => item.kind === "file" && item.type.startsWith("image/"));
  return imageItem?.getAsFile?.() || null;
}

async function handleImagePaste(ev) {
  if (ev.irisImagePasteHandled) return;
  if (!state.active) return;
  const file = clipboardImageFile(ev);
  if (!file) return;
  ev.irisImagePasteHandled = true;
  ev.preventDefault();
  const form = new FormData();
  form.append("file", file, file.name || "paste.png");
  form.append("mime_type", file.type);
  const res = await api(`/api/sessions/${state.active}/uploads`, { method: "POST", body: form });
  const uploadedPathInput = ` ${res.path}`;
  const target = ev.target;
  if (target === $("composer-input")) {
    $("composer-input").value += `${uploadedPathInput}\n`;
  } else {
    sendWS({ type: "input", data: `${uploadedPathInput} ` });
    state.term?.focus?.();
  }
}

$("terminal").addEventListener("paste", handleImagePaste, true);
document.addEventListener("paste", handleImagePaste);

async function initializeIris() {
  const security = await loadSecurityStatus();
	const settingsRequested = new URLSearchParams(location.search).get("settings") === "1";
	if (settingsRequested && !security.configured) {
		location.replace(settingsAuthURL("/setup-password"));
    return;
  }
	if (settingsRequested && !security.authenticated) {
		location.replace(settingsAuthURL("/login"));
		return;
	}
	if (settingsRequested) {
		await loadConfig();
		await openConfigDialog();
		return;
	}
  if (security.onboarding_required && security.authenticated) {
		if (!state.config) {
			await loadConfig();
		}
		if (!state.config) {
      return;
    }
    await maybeShowOnboarding();
  }
}

setInterval(loadSessions, 3000);
loadSessions().catch(console.error);
loadQuick().catch(console.error);
initializeIris().catch(console.error);

if (typeof window !== "undefined") {
  window.irisApp = {
    state,
    sendComposer,
    renderSessions,
    setNotify,
    loadConfig,
    saveConfig,
    testLarkConfig,
    checkEnvironment,
    maybeShowOnboarding,
    openConfigDialog,
    ensureDefaultAgentPreset,
    saveNamePresetFromForm,
    renderNamePresets,
    saveStartPresetFromForm,
    renderStartPresets,
    addPreStartCommandRow,
    visibleSessions,
    syncSnapshotNow,
    enqueueSnapshotSync,
    queueTerminalInput,
    terminalVisibleSnapshot,
    terminalSnapshotMetadata,
    terminalWebSocketURL,
    resizeTerm,
    syncHeadlessTerminalSize,
    standardTerminal: {
      cols: STANDARD_TERMINAL_COLS,
      rows: STANDARD_TERMINAL_ROWS,
      fontFamily: STANDARD_TERMINAL_FONT_FAMILY,
      fontSize: STANDARD_TERMINAL_FONT_SIZE,
      lineHeight: STANDARD_TERMINAL_LINE_HEIGHT,
    },
  };
}
