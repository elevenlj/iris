const mode = document.body.dataset.authMode;
const form = document.getElementById("auth-form");
const passwordInput = document.getElementById("auth-password");
const confirmInput = document.getElementById("auth-confirm-password");
const errorBox = document.getElementById("auth-error");
const submitButton = document.getElementById("auth-submit");

function safeNextPath() {
  const next = new URLSearchParams(location.search).get("next") || "/?settings=1";
  return next.startsWith("/") && !next.startsWith("//") ? next : "/?settings=1";
}

function authPage(path) {
  return `${path}?next=${encodeURIComponent(safeNextPath())}`;
}

async function request(path, options = {}) {
  const response = await fetch(path, {
    headers: { "Content-Type": "application/json" },
    ...options,
  });
  if (!response.ok) {
    let message = response.statusText;
    try {
      message = (await response.json()).error || message;
    } catch {}
    throw new Error(message);
  }
  return response.status === 204 ? null : response.json();
}

async function ensureCorrectPage() {
  const status = await request("/api/settings/security/status");
  if (mode === "setup" && status.configured) {
    location.replace(status.authenticated ? safeNextPath() : authPage("/login"));
    return;
  }
  if (mode === "login" && !status.configured) {
    location.replace(authPage("/setup-password"));
    return;
  }
  if (mode === "login" && status.authenticated) {
    location.replace(safeNextPath());
  }
}

form.addEventListener("submit", async (event) => {
  event.preventDefault();
  errorBox.textContent = "";
  const password = passwordInput.value;
  if (mode === "setup") {
    if (password.length < 8) {
      errorBox.textContent = "密码至少需要 8 个字符";
      passwordInput.focus();
      return;
    }
    if (password !== confirmInput.value) {
      errorBox.textContent = "两次输入的密码不一致";
      confirmInput.focus();
      return;
    }
  }
  submitButton.disabled = true;
  submitButton.textContent = mode === "setup" ? "正在保护 Iris…" : "正在验证…";
  try {
    await request(mode === "setup" ? "/api/settings/security/setup" : "/api/settings/security/login", {
      method: "POST",
      body: JSON.stringify(mode === "setup"
        ? { password, confirm_password: confirmInput.value }
        : { password }),
    });
    location.replace(safeNextPath());
  } catch (error) {
    errorBox.textContent = error.message || String(error);
    passwordInput.select();
    submitButton.disabled = false;
    submitButton.textContent = mode === "setup" ? "设置密码并继续" : "进入配置台";
  }
});

ensureCorrectPage().catch((error) => {
  errorBox.textContent = error.message || "暂时无法连接 Iris，请稍后重试";
});
