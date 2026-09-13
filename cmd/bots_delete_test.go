package main

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/elevenlj/iris/internal/httpapi"
	"github.com/elevenlj/iris/internal/session"
	"github.com/elevenlj/iris/internal/store"
)

func TestBotDeletionBackupIsolationAndLastBot(t *testing.T) {
	ctx := context.Background()
	dir, project := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(project, "keep.txt"), []byte("project"), 0600); err != nil {
		t.Fatal(err)
	}
	cfg := defaultConfig()
	cfg.Bots = []httpapi.BotConfig{{ID: "default", Name: "Primary", AppID: "cli_primary"}, {ID: "bot-other", Name: "Other"}}
	cfg.LarkAppID = "cli_primary"
	root := &appConfigService{cfg: &cfg, path: filepath.Join(dir, "config.json")}
	bots := newBotService(root, nil, dir)
	runtimes := map[string]*botRuntime{}
	for _, bot := range cfg.Bots {
		base := dir
		if bot.ID != "default" {
			base = filepath.Join(dir, "bots", bot.ID)
		}
		if err := os.MkdirAll(base, 0700); err != nil {
			t.Fatal(err)
		}
		st, err := store.Open(dbPathInDataDir(base))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = st.Close() })
		uploads := uploadsDirInDataDir(base)
		mgr := session.NewManager(st, session.ShellLauncher{}, session.WithIsolatedMessageRegistry(), session.WithRecoveryBaseDir(filepath.Join(base, "data", "sessions")),
			session.WithSessionEnded(func(id string) { _ = os.RemoveAll(filepath.Join(uploads, id)) }))
		rt := &botRuntime{store: st, manager: mgr, bridge: session.NewLarkReplyBridge("", "", mgr, uploads)}
		rt.server = httpapi.NewServer(mgr, uploads, root)
		if bot.ID == "default" {
			root.manager, root.bridge, bots.server, bots.defaultStore, bots.defaultUploads = mgr, rt.bridge, rt.server, st, uploads
		} else {
			bots.runtimes[bot.ID] = rt
		}
		runtimes[bot.ID] = rt
		sess, err := mgr.CreateSession(ctx, bot.Name)
		if err != nil {
			t.Fatal(err)
		}
		terminal, _ := mgr.GetRuntime(sess.ID)
		t.Cleanup(terminal.Close)
		if err := st.AppendOutput(ctx, sess.ID, 1, []byte("saved output")); err != nil {
			t.Fatal(err)
		}
		for _, sub := range []string{filepath.Join(base, "data", "sessions", "saved"), filepath.Join(uploads, sess.ID)} {
			if err := os.MkdirAll(sub, 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(sub, "keep.txt"), []byte(bot.Name), 0600); err != nil {
				t.Fatal(err)
			}
		}
		if err := os.Symlink(project, filepath.Join(base, "data", "sessions", "saved", "project-link")); err != nil {
			t.Fatal(err)
		}
		// Include an archived session in the confirmation count and upload backup.
		sess.ID = "sess-archived"
		sess.Live, sess.Status = false, session.StatusExited
		if err := st.CreateSession(ctx, sess); err != nil {
			t.Fatal(err)
		}
	}
	if err := writeConfigFile(root.path, cfg); err != nil {
		t.Fatal(err)
	}
	bots.server.SetBotService(bots)
	// Failed backup and failed config commit must leave the bot and terminal usable.
	backupRoot := filepath.Join(dir, "deleted-bots")
	if err := os.WriteFile(backupRoot, []byte("blocked"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := bots.DeleteBot(ctx, "default"); err == nil {
		t.Fatal("backup failure accepted")
	}
	if len(bots.ListBots()) != 2 {
		t.Fatal("backup failure deleted config")
	}
	if err := os.Remove(backupRoot); err != nil {
		t.Fatal(err)
	}
	oldPath := root.path
	root.path = filepath.Join(oldPath, "impossible.json")
	if _, err := bots.DeleteBot(ctx, "default"); err == nil {
		t.Fatal("config failure accepted")
	}
	root.path = oldPath
	primary, ok := root.manager.GetRuntime("sess-1")
	if !ok || primary.WriteInput("echo still-live\r") != nil {
		t.Fatal("failed delete stopped terminal")
	}
	for _, id := range []string{"default", "bot-other"} {
		preview, err := bots.PreviewBotDeletion(ctx, id)
		if err != nil || preview.Sessions != 2 {
			t.Fatalf("preview: %#v %v", preview, err)
		}
		result, err := bots.DeleteBot(ctx, id)
		if err != nil || result.Warning != "" {
			t.Fatalf("delete %s: %#v %v", id, result, err)
		}
		info, err := os.Stat(result.BackupPath)
		if err != nil || info.Mode().Perm() != 0700 {
			t.Fatal("backup permissions", err)
		}
		backup, err := store.Open(filepath.Join(result.BackupPath, "iris.db"))
		if err != nil {
			t.Fatal(err)
		}
		list, err := backup.ListSessions(ctx)
		if err != nil || len(list) != 2 {
			t.Fatal("missing backup sessions", err)
		}
		output, err := backup.Output(ctx, "sess-1")
		_ = backup.Close()
		if err != nil || !strings.Contains(string(output), "saved output") {
			t.Fatal("WAL output missing", err)
		}
		f, err := os.Open(filepath.Join(result.BackupPath, "files.tar.gz"))
		if err != nil {
			t.Fatal(err)
		}
		gz, err := gzip.NewReader(f)
		if err != nil {
			t.Fatal(err)
		}
		tr := tar.NewReader(gz)
		files := map[string]byte{}
		for {
			h, err := tr.Next()
			if err == io.EOF {
				break
			}
			if err != nil {
				t.Fatal(err)
			}
			files[h.Name] = h.Typeflag
		}
		_ = gz.Close()
		_ = f.Close()
		if _, ok := files["uploads/sess-1/keep.txt"]; !ok {
			t.Fatal("uploads removed before backup")
		}
		if files["sessions/saved/project-link"] != tar.TypeSymlink {
			t.Fatal("symlink missing")
		}
		if _, ok := files["sessions/saved/project-link/keep.txt"]; ok {
			t.Fatal("backup traversed project symlink")
		}
		if _, err := os.Stat(filepath.Join(project, "keep.txt")); err != nil {
			t.Fatal("project removed")
		}
		mgr := runtimes[id].manager
		if _, err := mgr.CreateSession(ctx, "stale card"); err == nil {
			t.Fatal("retired manager creates tasks")
		}
		if _, _, _, err := mgr.RecoverRuntime(ctx, "sess-1"); err == nil {
			t.Fatal("retired manager recovers tasks")
		}
		if bots.BotHandler(id) != nil {
			t.Fatal("deleted route remains")
		}
		if id == "default" {
			if primary.WriteInput("echo old-card\r") == nil {
				t.Fatal("deleted terminal accepts input")
			}
			other, ok := runtimes["bot-other"].manager.GetRuntime("sess-1")
			if !ok || other.WriteInput("echo unaffected\r") != nil {
				t.Fatal("other bot stopped")
			}
		}
	}
	t.Setenv("LARK_APP_ID", "cli_stale")
	t.Setenv("LARK_APP_SECRET", "stale")
	loaded := loadConfig(root.path)
	if loaded.Bots == nil || len(loaded.Bots) != 0 || loaded.LarkAppID != "" {
		t.Fatal("deleted bots resurrected", loaded.Bots)
	}
	if err := bots.Start(); err != nil || len(bots.ListBots()) != 0 {
		t.Fatal("restart resurrected bot", err)
	}
	for _, id := range []string{"default", "../default", "/", ""} {
		if _, err := bots.DeleteBot(ctx, id); err == nil {
			t.Fatal("invalid/repeated deletion accepted", id)
		}
	}
	// Locked settings protect both counts and deletion; no credentials are exposed.
	cfg.SettingsPasswordHash = "configured"
	for _, method := range []string{http.MethodGet, http.MethodDelete} {
		rec := httptest.NewRecorder()
		bots.server.Handler().ServeHTTP(rec, httptest.NewRequest(method, "/api/bots?delete_id=default", strings.NewReader(`{"id":"default"}`)))
		if rec.Code < 400 {
			t.Fatal("unauthorized deletion access", rec.Code)
		}
	}
}

func TestBotSaveCannotResurrectDeletedID(t *testing.T) {
	dir := t.TempDir()
	cfg := defaultConfig()
	cfg.Agents = []session.AgentConfig{{ID: "custom-test", Name: "Test", Kind: "custom", Command: "sh"}}
	bot := httpapi.BotConfig{ID: "default", Name: "Old", AppID: "cli_old", AppSecret: "old", ReceiveID: "ou_old", DefaultAgentID: "custom-test", DefaultWorkspaceDir: dir}
	cfg.Bots = []httpapi.BotConfig{bot}
	root := &appConfigService{cfg: &cfg, path: filepath.Join(dir, "config.json"), manager: session.NewManager(nil, nil)}
	bots := newBotService(root, nil, dir)
	bots.test = func(httpapi.RuntimeConfig) httpapi.LarkConfigTestResult {
		// Simulate deletion committing while SaveBot's external credential check is in flight.
		root.mu.Lock()
		cfg.Bots = []httpapi.BotConfig{}
		root.mu.Unlock()
		return httpapi.LarkConfigTestResult{OK: true}
	}
	bots.appName = func(context.Context, string, string) (string, error) { return "New", nil }
	bot.AppSecret = "new"
	if _, err := bots.SaveBot(context.Background(), bot); err == nil {
		t.Fatal("stale save resurrected bot")
	}
	if len(cfg.Bots) != 0 {
		t.Fatal("deleted bot reinserted")
	}
	bot.ID = ""
	root.path = filepath.Join(dir, "missing-parent-file", "config.json")
	if err := os.WriteFile(filepath.Join(dir, "missing-parent-file"), []byte("blocked"), 0600); err != nil {
		t.Fatal(err)
	}
	// Fail persistence before starting an external bridge, but verify ID allocation.
	newBot, err := bots.SaveBot(context.Background(), bot)
	if err == nil || !strings.HasPrefix(newBot.ID, "bot-") {
		t.Fatal("new bot reused deleted default ID", newBot.ID, err)
	}
	for _, id := range []string{"", ".", "..", "/", "a/b", `a\b`} {
		if safeSessionDataID(id) {
			t.Fatal("unsafe data path", id)
		}
	}
}
