package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

const environmentCheckTimeout = 10 * time.Second

type EnvironmentCheckStep struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Status  string `json:"status"`
	Message string `json:"message"`
}

type EnvironmentCheckResult struct {
	OK      bool                   `json:"ok"`
	Steps   []EnvironmentCheckStep `json:"steps"`
	Checked string                 `json:"checked_at"`
}

type EnvironmentChecker interface {
	Check(context.Context, RuntimeConfig, string) EnvironmentCheckResult
}

type realEnvironmentChecker struct{}

func (realEnvironmentChecker) Check(ctx context.Context, cfg RuntimeConfig, uploadsDir string) EnvironmentCheckResult {
	steps := []EnvironmentCheckStep{{ID: "service", Name: "Iris 服务", Status: "ok", Message: "服务运行正常"}}
	steps = append(steps, checkNodeEnvironment(ctx))
	steps = append(steps, checkHeadlessBrowser(ctx))
	steps = append(steps, checkDataDirectory(uploadsDir))
	steps = append(steps, checkFeishuEnvironment(ctx, cfg)...)
	steps = append(steps, checkAgentCommand(cfg))
	if hook := checkAgentCompletionHook(cfg); hook != nil {
		steps = append(steps, *hook)
	}
	result := EnvironmentCheckResult{OK: true, Steps: steps, Checked: time.Now().UTC().Format(time.RFC3339)}
	for _, step := range steps {
		if step.Status == "error" {
			result.OK = false
			break
		}
	}
	return result
}

func checkNodeEnvironment(ctx context.Context) EnvironmentCheckStep {
	path := findEnvironmentExecutable("node")
	if path == "" {
		return EnvironmentCheckStep{ID: "node", Name: "Node.js", Status: "error", Message: "未找到 Node.js，请先安装 Node.js 18 或更高版本"}
	}
	checkCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	cmd := exec.CommandContext(checkCtx, path, "--version")
	cmd.Env = environmentCommandEnv()
	output, err := cmd.Output()
	if err != nil {
		return EnvironmentCheckStep{ID: "node", Name: "Node.js", Status: "error", Message: "Node.js 无法正常运行"}
	}
	return EnvironmentCheckStep{ID: "node", Name: "Node.js", Status: "ok", Message: "运行正常（" + strings.TrimSpace(string(output)) + "）"}
}

func checkHeadlessBrowser(ctx context.Context) EnvironmentCheckStep {
	chrome := findEnvironmentChrome()
	if chrome == "" {
		return EnvironmentCheckStep{ID: "headless_browser", Name: "Headless 浏览器", Status: "error", Message: "未找到 Chrome、Chromium 或 Edge"}
	}
	profile, err := os.MkdirTemp("", "iris-environment-check-*")
	if err != nil {
		return EnvironmentCheckStep{ID: "headless_browser", Name: "Headless 浏览器", Status: "error", Message: "无法创建浏览器临时目录"}
	}
	defer os.RemoveAll(profile)
	checkCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	args := []string{
		"--headless=new", "--disable-gpu", "--no-sandbox", "--no-first-run", "--no-default-browser-check",
		"--disable-dev-shm-usage", "--user-data-dir=" + profile, "about:blank",
	}
	cmd := exec.CommandContext(checkCtx, chrome, args...)
	cmd.Env = environmentCommandEnv()
	if err := cmd.Start(); err != nil {
		return EnvironmentCheckStep{ID: "headless_browser", Name: "Headless 浏览器", Status: "error", Message: "浏览器已安装，但 Headless 模式启动失败"}
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	timer := time.NewTimer(time.Second)
	defer timer.Stop()
	select {
	case err := <-done:
		if err != nil {
			return EnvironmentCheckStep{ID: "headless_browser", Name: "Headless 浏览器", Status: "error", Message: "浏览器已安装，但 Headless 模式启动失败"}
		}
	case <-timer.C:
		_ = cmd.Process.Kill()
		<-done
	case <-checkCtx.Done():
		_ = cmd.Process.Kill()
		<-done
		return EnvironmentCheckStep{ID: "headless_browser", Name: "Headless 浏览器", Status: "error", Message: "浏览器已安装，但 Headless 模式启动超时"}
	}
	return EnvironmentCheckStep{ID: "headless_browser", Name: "Headless 浏览器", Status: "ok", Message: "浏览器能够正常启动"}
}

func checkDataDirectory(uploadsDir string) EnvironmentCheckStep {
	uploadsDir = strings.TrimSpace(uploadsDir)
	if uploadsDir == "" {
		return EnvironmentCheckStep{ID: "data_directory", Name: "数据目录", Status: "error", Message: "数据目录未配置"}
	}
	if err := os.MkdirAll(uploadsDir, 0o755); err != nil {
		return EnvironmentCheckStep{ID: "data_directory", Name: "数据目录", Status: "error", Message: "数据目录无法创建或写入"}
	}
	probe, err := os.CreateTemp(uploadsDir, ".iris-write-check-*")
	if err != nil {
		return EnvironmentCheckStep{ID: "data_directory", Name: "数据目录", Status: "error", Message: "数据目录不可写"}
	}
	name := probe.Name()
	_ = probe.Close()
	_ = os.Remove(name)
	return EnvironmentCheckStep{ID: "data_directory", Name: "数据目录", Status: "ok", Message: "读写正常"}
}

func checkFeishuEnvironment(ctx context.Context, cfg RuntimeConfig) []EnvironmentCheckStep {
	client := &http.Client{Timeout: environmentCheckTimeout}
	appID := strings.TrimSpace(cfg.LarkAppID)
	appSecret := strings.TrimSpace(cfg.LarkAppSecret)
	if appID == "" || appSecret == "" {
		req, _ := http.NewRequestWithContext(ctx, http.MethodHead, feishuOpenBase, nil)
		resp, err := client.Do(req)
		if resp != nil {
			_ = resp.Body.Close()
		}
		network := EnvironmentCheckStep{ID: "feishu_network", Name: "飞书网络", Status: "ok", Message: "连接正常"}
		if err != nil {
			network.Status = "error"
			network.Message = "无法连接飞书开放平台"
		}
		return []EnvironmentCheckStep{
			network,
			{ID: "feishu_app", Name: "飞书应用", Status: "warning", Message: "尚未完成飞书应用配置"},
		}
	}
	payload, _ := json.Marshal(map[string]string{"app_id": appID, "app_secret": appSecret})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, feishuOpenBase+"/open-apis/auth/v3/tenant_access_token/internal", bytes.NewReader(payload))
	if err != nil {
		return []EnvironmentCheckStep{{ID: "feishu_network", Name: "飞书网络", Status: "error", Message: "无法发起飞书连接检测"}}
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return []EnvironmentCheckStep{
			{ID: "feishu_network", Name: "飞书网络", Status: "error", Message: "无法连接飞书开放平台"},
			{ID: "feishu_app", Name: "飞书应用", Status: "warning", Message: "网络异常，暂时无法验证应用配置"},
		}
	}
	defer resp.Body.Close()
	var tokenResp struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4096)).Decode(&tokenResp); err != nil {
		return []EnvironmentCheckStep{
			{ID: "feishu_network", Name: "飞书网络", Status: "ok", Message: "连接正常"},
			{ID: "feishu_app", Name: "飞书应用", Status: "error", Message: "飞书返回内容无法识别"},
		}
	}
	appStep := EnvironmentCheckStep{ID: "feishu_app", Name: "飞书应用", Status: "ok", Message: "应用凭证有效"}
	if tokenResp.Code != 0 {
		appStep.Status = "error"
		appStep.Message = "应用凭证无效，请重新扫码配置或检查手动填写内容"
	}
	if strings.TrimSpace(cfg.LarkNotifyReceiveID) == "" && appStep.Status == "ok" {
		appStep.Status = "warning"
		appStep.Message = "应用凭证有效，但缺少开发者 Open ID"
	}
	return []EnvironmentCheckStep{
		{ID: "feishu_network", Name: "飞书网络", Status: "ok", Message: "连接正常"},
		appStep,
	}
}

func checkAgentCommand(cfg RuntimeConfig) EnvironmentCheckStep {
	command := strings.TrimSpace(cfg.AgentCommand)
	if command == "" && strings.EqualFold(strings.TrimSpace(cfg.AgentKind), "codex") {
		command = "codex"
	}
	if command == "" {
		return EnvironmentCheckStep{ID: "agent_command", Name: "Agent 启动命令", Status: "error", Message: "尚未配置 Agent 启动命令"}
	}
	executable := firstEnvironmentCommand(command)
	if executable == "" {
		return EnvironmentCheckStep{ID: "agent_command", Name: "Agent 启动命令", Status: "warning", Message: "命令已配置，但组合命令需要启动会话后验证"}
	}
	if filepath.IsAbs(executable) {
		if info, err := os.Stat(executable); err == nil && isEnvironmentExecutableFile(info) {
			return EnvironmentCheckStep{ID: "agent_command", Name: "Agent 启动命令", Status: "ok", Message: "启动命令可用"}
		}
	} else if findEnvironmentExecutable(executable) != "" {
		return EnvironmentCheckStep{ID: "agent_command", Name: "Agent 启动命令", Status: "ok", Message: "启动命令可用"}
	}
	return EnvironmentCheckStep{ID: "agent_command", Name: "Agent 启动命令", Status: "error", Message: "找不到已配置的 Agent 启动命令"}
}

func checkAgentCompletionHook(cfg RuntimeConfig) *EnvironmentCheckStep {
	kind := environmentAgentKind(cfg)
	if kind == "" {
		return nil
	}
	home, _ := os.UserHomeDir()
	path := ""
	marker := ""
	switch kind {
	case "codex":
		path = filepath.Join(home, ".codex", "config.toml")
		marker = "--codex-notify"
	case "claude", "aiden":
		configDir := strings.TrimSpace(os.Getenv("CLAUDE_CONFIG_DIR"))
		if kind == "aiden" {
			configDir = filepath.Join(home, ".aiden")
		} else if configDir == "" {
			configDir = filepath.Join(home, ".claude")
		}
		path = filepath.Join(configDir, "settings.json")
		marker = "--claude-stop"
	}
	content, err := os.ReadFile(path)
	if err != nil || !strings.Contains(string(content), marker) {
		return &EnvironmentCheckStep{ID: "agent_hook", Name: "Agent 完成通知", Status: "error", Message: "完成通知未正确安装，重新启动 Iris 可自动修复"}
	}
	return &EnvironmentCheckStep{ID: "agent_hook", Name: "Agent 完成通知", Status: "ok", Message: "通知配置正常"}
}

func environmentAgentKind(cfg RuntimeConfig) string {
	if strings.EqualFold(strings.TrimSpace(cfg.AgentKind), "codex") {
		return "codex"
	}
	command := strings.ToLower(filepath.Base(firstEnvironmentCommand(cfg.AgentCommand)))
	command = strings.TrimSuffix(command, ".exe")
	switch command {
	case "codex":
		return "codex"
	case "claude", "claude-code":
		return "claude"
	case "aiden":
		fields := strings.Fields(strings.ToLower(cfg.AgentCommand))
		for index := range fields {
			if strings.TrimSuffix(filepath.Base(fields[index]), ".exe") != "aiden" {
				continue
			}
			if index+2 < len(fields) && fields[index+1] == "x" && fields[index+2] == "codex" {
				return "codex"
			}
			return "aiden"
		}
		return "aiden"
	default:
		return ""
	}
}

func firstEnvironmentCommand(command string) string {
	fields := strings.Fields(strings.TrimSpace(strings.Split(command, ";")[0]))
	for len(fields) > 0 && strings.Contains(fields[0], "=") && !strings.HasPrefix(fields[0], "=") {
		fields = fields[1:]
	}
	if len(fields) > 0 && filepath.Base(fields[0]) == "env" {
		fields = fields[1:]
		for len(fields) > 0 && strings.Contains(fields[0], "=") {
			fields = fields[1:]
		}
	}
	if len(fields) == 0 || fields[0] == "source" || fields[0] == "." {
		return ""
	}
	return strings.Trim(fields[0], "'\"")
}

func findEnvironmentExecutable(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return ""
	}
	if path, err := exec.LookPath(name); err == nil {
		return path
	}
	home, _ := os.UserHomeDir()
	for _, dir := range environmentPathEntries(home) {
		candidate := filepath.Join(dir, name)
		if info, err := os.Stat(candidate); err == nil && isEnvironmentExecutableFile(info) {
			return candidate
		}
	}
	return ""
}

func isEnvironmentExecutableFile(info os.FileInfo) bool {
	return info != nil && !info.IsDir() && (runtime.GOOS == "windows" || info.Mode()&0o111 != 0)
}

func findEnvironmentChrome() string {
	candidates := []string{os.Getenv("CHROME_BIN")}
	if runtime.GOOS == "darwin" {
		candidates = append(candidates,
			"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome",
			"/Applications/Chromium.app/Contents/MacOS/Chromium",
			"/Applications/Microsoft Edge.app/Contents/MacOS/Microsoft Edge",
		)
	}
	for _, candidate := range candidates {
		if info, err := os.Stat(candidate); candidate != "" && err == nil && !info.IsDir() {
			return candidate
		}
	}
	for _, name := range []string{"google-chrome", "chromium", "chromium-browser", "chrome", "msedge"} {
		if path := findEnvironmentExecutable(name); path != "" {
			return path
		}
	}
	return ""
}

func environmentCommandEnv() []string {
	home, _ := os.UserHomeDir()
	entries := environmentPathEntries(home)
	if current := strings.TrimSpace(os.Getenv("PATH")); current != "" {
		entries = append(entries, strings.Split(current, string(os.PathListSeparator))...)
	}
	seen := map[string]bool{}
	path := make([]string, 0, len(entries))
	for _, entry := range entries {
		entry = strings.TrimSpace(entry)
		if entry == "" || seen[entry] {
			continue
		}
		seen[entry] = true
		path = append(path, entry)
	}
	environment := make([]string, 0, len(os.Environ())+1)
	for _, value := range os.Environ() {
		if !strings.HasPrefix(value, "PATH=") {
			environment = append(environment, value)
		}
	}
	return append(environment, "PATH="+strings.Join(path, string(os.PathListSeparator)))
}

func environmentPathEntries(home string) []string {
	return []string{
		filepath.Join(home, ".local", "bin"), filepath.Join(home, ".node", "bin"),
		filepath.Join(home, ".npm-global", "bin"), filepath.Join(home, "bin"),
		"/opt/homebrew/bin", "/usr/local/bin", "/usr/bin", "/bin",
	}
}
