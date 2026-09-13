package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"text/tabwriter"
	"time"
)

const runtimeControlHeader = "X-Iris-Control-Token"

type runtimeRecord struct {
	InstanceID string    `json:"instance_id"`
	Token      string    `json:"token"`
	Port       string    `json:"port"`
	PID        int       `json:"pid"`
	Version    string    `json:"version"`
	StartedAt  time.Time `json:"started_at"`
	ConfigDir  string    `json:"config_dir"`
	Executable string    `json:"executable"`
}

func newRuntimeRecord(port, configDir string) (runtimeRecord, error) {
	executable, err := os.Executable()
	if err != nil {
		return runtimeRecord{}, err
	}
	instanceID, err := randomHex(16)
	if err != nil {
		return runtimeRecord{}, err
	}
	token, err := randomHex(32)
	if err != nil {
		return runtimeRecord{}, err
	}
	return runtimeRecord{
		InstanceID: instanceID,
		Token:      token,
		Port:       port,
		PID:        os.Getpid(),
		Version:    version,
		StartedAt:  time.Now().UTC(),
		ConfigDir:  configDir,
		Executable: executable,
	}, nil
}

func randomHex(size int) (string, error) {
	b := make([]byte, size)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func runtimeRecordPath(dataDir, port string) string {
	return filepath.Join(dataDir, "run", "service-"+port+".json")
}

func registerRuntimeRecord(dataDir string, record runtimeRecord) error {
	path := runtimeRecordPath(dataDir, record.Port)
	b, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return err
	}
	if err := writeFileAtomic(path, append(b, '\n'), 0o600); err != nil {
		return err
	}
	return nil
}

func unregisterRuntimeRecord(dataDir string, record runtimeRecord) {
	path := runtimeRecordPath(dataDir, record.Port)
	b, err := os.ReadFile(path)
	if err != nil {
		return
	}
	var current runtimeRecord
	if json.Unmarshal(b, &current) == nil && current.InstanceID == record.InstanceID {
		_ = os.Remove(path)
	}
}

func writeFileAtomic(path string, data []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".iris-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if err := tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, path); err == nil {
		return nil
	} else if runtime.GOOS != "windows" {
		return err
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return os.Rename(tmpPath, path)
}

func listActiveRuntimeRecords(dataDir string) ([]runtimeRecord, error) {
	paths, err := filepath.Glob(filepath.Join(dataDir, "run", "service-*.json"))
	if err != nil {
		return nil, err
	}
	client := &http.Client{Timeout: time.Second}
	records := make([]runtimeRecord, 0, len(paths))
	for _, path := range paths {
		b, readErr := os.ReadFile(path)
		var record runtimeRecord
		if readErr != nil || json.Unmarshal(b, &record) != nil || record.Port == "" || record.Token == "" || record.InstanceID == "" {
			_ = os.Remove(path)
			continue
		}
		if probeRuntime(client, record) != nil {
			_ = os.Remove(path)
			continue
		}
		records = append(records, record)
	}
	sort.Slice(records, func(i, j int) bool {
		left, _ := strconv.Atoi(records[i].Port)
		right, _ := strconv.Atoi(records[j].Port)
		return left < right
	})
	return records, nil
}

func probeRuntime(client *http.Client, record runtimeRecord) error {
	req, err := http.NewRequest(http.MethodGet, "http://127.0.0.1:"+record.Port+"/api/runtime", nil)
	if err != nil {
		return err
	}
	req.Header.Set(runtimeControlHeader, record.Token)
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("runtime status %s", resp.Status)
	}
	var status struct {
		InstanceID string `json:"instance_id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&status); err != nil {
		return err
	}
	if status.InstanceID != record.InstanceID {
		return errors.New("runtime instance changed")
	}
	return nil
}

func stopRuntime(record runtimeRecord) error {
	req, err := http.NewRequest(http.MethodDelete, "http://127.0.0.1:"+record.Port+"/api/runtime", nil)
	if err != nil {
		return err
	}
	req.Header.Set(runtimeControlHeader, record.Token)
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("停止端口 %s 失败：%s", record.Port, resp.Status)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		time.Sleep(100 * time.Millisecond)
		if probeRuntime(&http.Client{Timeout: 250 * time.Millisecond}, record) != nil {
			return nil
		}
	}
	return fmt.Errorf("端口 %s 的 Iris 未在 5 秒内退出", record.Port)
}

var launchRuntimeProcess = func(record runtimeRecord) error {
	cmd := exec.Command(record.Executable, "--port", record.Port, "--config-dir", record.ConfigDir)
	configureDetachedCommand(cmd)
	if err := cmd.Start(); err != nil {
		return err
	}
	return cmd.Process.Release()
}

func restartRuntime(dataDir string, record runtimeRecord) error {
	if err := launchRuntimeProcess(record); err != nil {
		return err
	}
	deadline := time.Now().Add(30 * time.Second)
	client := &http.Client{Timeout: 250 * time.Millisecond}
	for time.Now().Before(deadline) {
		time.Sleep(100 * time.Millisecond)
		b, err := os.ReadFile(runtimeRecordPath(dataDir, record.Port))
		if err != nil {
			continue
		}
		var current runtimeRecord
		if json.Unmarshal(b, &current) == nil && current.InstanceID != record.InstanceID && probeRuntime(client, current) == nil {
			return nil
		}
	}
	return fmt.Errorf("端口 %s 的 Iris 未在 30 秒内重新启动", record.Port)
}

func handleServiceCommand(args []string, _ io.Reader, out io.Writer, dataDir string, _ bool) (bool, error) {
	if len(args) == 0 || (args[0] != "status" && args[0] != "stop" && args[0] != "restart") {
		return false, nil
	}
	command := args[0]
	if len(args) != 1 {
		return true, fmt.Errorf("用法：iris %s（操作 Iris 服务，不再接受端口或 all）", command)
	}
	records, err := listActiveRuntimeRecords(dataDir)
	if err != nil {
		return true, err
	}
	if command == "status" {
		printRuntimeStatus(out, records, autoStartPort(dataDir))
		return true, nil
	}
	if len(records) == 0 {
		fmt.Fprintln(out, "当前没有运行中的 Iris 服务。")
		return true, nil
	}
	targets := records
	if len(records) > 1 {
		// Never stop a legacy instance's sessions without migrating its data.
		primary := loadConfig(configPathForDataDir("", dataDir)).Port
		targets, err = selectRuntimeRecords(records, primary)
		if err != nil {
			return true, errors.New("检测到旧版多服务，请先确认主服务配置并迁移数据")
		}
		fmt.Fprintf(out, "检测到旧版多服务，本次仅操作主服务（端口 %s）。\n", primary)
	}
	record := targets[0]
	if err := stopRuntime(record); err != nil {
		return true, err
	}
	if command == "restart" {
		if err := restartRuntime(dataDir, record); err != nil {
			return true, err
		}
		fmt.Fprintln(out, "Iris 服务已重启。")
	} else {
		fmt.Fprintln(out, "Iris 服务已停止。")
	}
	return true, nil
}

func selectRuntimeRecords(records []runtimeRecord, selector string) ([]runtimeRecord, error) {
	port, err := strconv.Atoi(selector)
	if err != nil || port < 1 || port > 65535 {
		return nil, fmt.Errorf("无效端口：%s", selector)
	}
	for _, record := range records {
		if record.Port == selector {
			return []runtimeRecord{record}, nil
		}
	}
	return nil, fmt.Errorf("端口 %s 没有运行中的 Iris 服务", selector)
}

func printRuntimeStatus(out io.Writer, records []runtimeRecord, autoPort string) {
	if len(records) == 0 {
		fmt.Fprintln(out, "当前没有运行中的 Iris 服务。")
		return
	}
	tw := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "PORT\tPID\tVERSION\tUPTIME\tAUTO START")
	for _, record := range records {
		auto := "no"
		if record.Port == autoPort {
			auto = "yes"
		}
		fmt.Fprintf(tw, "%s\t%d\t%s\t%s\t%s\n", record.Port, record.PID, record.Version, formatUptime(time.Since(record.StartedAt)), auto)
	}
	_ = tw.Flush()
}

func formatUptime(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%ds", max(0, int(d.Seconds())))
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm", int(d.Minutes()))
	}
	return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
}
