package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type progressTestBots struct {
	fail  bool
	calls int
}

func (b *progressTestBots) DeleteBot(context.Context, string) (BotDeletion, error) {
	b.calls++
	return BotDeletion{}, nil
}

func (b *progressTestBots) PreviewBotDeletion(context.Context, string) (BotDeletionPreview, error) {
	return BotDeletionPreview{}, nil
}

func (b *progressTestBots) CreateBot(ctx context.Context, _ BotConfig) (BotConfig, error) {
	b.calls++
	ReportBotCreationProgress(ctx, "login", "等待登录", "")
	ReportBotCreationProgress(ctx, "configuring", "配置中", "cli_created")
	if b.fail {
		return BotConfig{}, errors.New("发布已提交，等待企业审批")
	}
	return BotConfig{ID: "bot-created", AppSecret: "must-not-stream-secret"}, nil
}
func (b *progressTestBots) ListFeishuApps(ctx context.Context) ([]FeishuApp, error) {
	ReportBotCreationProgress(ctx, "listing", "读取已有应用", "")
	return []FeishuApp{{AppID: "cli_existing", Name: "已有机器人"}}, nil
}
func (*progressTestBots) ListBots() []BotConfig { return nil }
func (*progressTestBots) SaveBot(context.Context, BotConfig) (BotConfig, error) {
	return BotConfig{}, nil
}
func (*progressTestBots) BotHandler(string) http.Handler { return nil }

func TestBotSetupProgressStreamsAndRequiresAuth(t *testing.T) {
	for _, mode := range []string{"create", "error", "list"} {
		t.Run(mode, func(t *testing.T) {
			bots := &progressTestBots{fail: mode == "error"}
			server := NewServer(nil, "")
			server.SetBotService(bots)
			body := "{}"
			if mode == "list" {
				body = `{"list_apps":true}`
			}
			req := httptest.NewRequest(http.MethodPost, "/api/bots/create", strings.NewReader(body))
			req.Header.Set("Accept", "application/x-ndjson")
			rec := httptest.NewRecorder()
			server.handleBotCreate(rec, req)
			if rec.Code != 200 || !rec.Flushed || rec.Header().Get("Content-Type") != "application/x-ndjson" {
				t.Fatalf("not streamed: %d, %v", rec.Code, rec.Header())
			}
			if strings.Contains(rec.Body.String(), "must-not-stream-secret") {
				t.Fatal("credentials leaked")
			}
			var events []BotCreationProgress
			for _, line := range strings.Split(strings.TrimSpace(rec.Body.String()), "\n") {
				var e BotCreationProgress
				if err := json.Unmarshal([]byte(line), &e); err != nil {
					t.Fatal(err)
				}
				events = append(events, e)
			}
			last := events[len(events)-1]
			switch mode {
			case "list":
				if bots.calls != 0 || last.Stage != "apps" || len(last.Apps) != 1 {
					t.Fatal(events)
				}
			case "create":
				if events[0].Stage != "login" || last.Stage != "done" || last.BotID != "bot-created" {
					t.Fatal(events)
				}
			case "error":
				if events[1].AppID != "cli_created" || last.Stage != "error" || last.Error == "" {
					t.Fatal(events)
				}
			}
			locked := NewServer(nil, "", &secureTestConfig{security: SettingsSecurity{PasswordHash: "configured"}})
			locked.SetBotService(bots)
			req = httptest.NewRequest(http.MethodPost, "/api/bots/create", strings.NewReader(body))
			req.Header.Set("Accept", "application/x-ndjson")
			rec = httptest.NewRecorder()
			locked.handleBotCreate(rec, req)
			if rec.Code != http.StatusUnauthorized || rec.Flushed {
				t.Fatal("unauthorized setup stream accepted")
			}
		})
	}
}
