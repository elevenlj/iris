package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"

	qrcode "github.com/skip2/go-qrcode"
)

const (
	larkRegistrationPath       = "/oauth/v1/app/registration"
	larkRegistrationTemplateID = "default"
	feishuAccountsBase         = "https://accounts.feishu.cn"
	feishuOpenBase             = "https://open.feishu.cn"
	larksuiteAccountsBase      = "https://accounts.larksuite.com"
	larksuiteOpenBase          = "https://open.larksuite.com"
)

var (
	larkRegistrationScopes = []string{
		"im:message",
		"im:message:send_as_bot",
		"im:message.p2p_msg:readonly",
		"im:message.group_msg",
		"im:message.group_msg:readonly",
		"im:message.group_at_msg:readonly",
		"im:message.group_at_msg.include_bot:readonly",
		"im:message:readonly",
		"im:message:update",
		"im:message.reactions:read",
		"im:message.reactions:write_only",
		"im:resource",
		"im:chat:create",
		"im:chat:read",
		"im:chat:update",
		"im:chat.members:read",
		"im:chat.members:write_only",
		"im:chat.members:bot_access",
		"cardkit:card:read",
		"cardkit:card:write",
		"contact:user.base:readonly",
	}
	larkRegistrationEvents = []string{
		"im.chat.member.bot.added_v1",
		"im.message.receive_v1",
		"im.message.message_read_v1",
		"im.message.reaction.created_v1",
		"im.message.reaction.deleted_v1",
	}
	larkRegistrationCallbacks = []string{"card.action.trigger"}
)

type LarkAppRegistrationBegin struct {
	DeviceCode              string `json:"device_code"`
	UserCode                string `json:"user_code"`
	VerificationURI         string `json:"verification_uri"`
	VerificationURIComplete string `json:"verification_uri_complete"`
	ExpiresIn               int    `json:"expires_in"`
	Interval                int    `json:"interval"`
	Brand                   string `json:"brand"`
}

type LarkAppRegistrationResult struct {
	AppID           string `json:"app_id"`
	AppSecret       string `json:"app_secret"`
	Brand           string `json:"brand"`
	OpenID          string `json:"open_id,omitempty"`
	NotifyReceiveID string `json:"lark_notify_receive_id,omitempty"`
	Pending         bool   `json:"pending,omitempty"`
}

type larkAppRegistrationClient struct {
	httpClient *http.Client
}

func newLarkAppRegistrationClient() *larkAppRegistrationClient {
	return &larkAppRegistrationClient{httpClient: &http.Client{Timeout: 15 * time.Second}}
}

func (c *larkAppRegistrationClient) Begin(ctx context.Context, brand string) (LarkAppRegistrationBegin, error) {
	brand = normalizeRegistrationBrand(brand)
	accountsBase, openBase := registrationBases(brand)
	form := larkRegistrationBeginForm()
	log.Printf("lark app registration begin brand=%s scopes=%q events=%q callbacks=%q", brand, form.Get("scope"), form.Get("events"), form.Get("callbacks"))
	data, err := c.postForm(ctx, accountsBase+larkRegistrationPath, form)
	if err != nil {
		return LarkAppRegistrationBegin{}, err
	}
	if errMsg := registrationError(data); errMsg != "" {
		return LarkAppRegistrationBegin{}, fmt.Errorf("应用注册失败: %s", errMsg)
	}
	userCode := stringField(data, "user_code")
	deviceCode := stringField(data, "device_code")
	if userCode == "" || deviceCode == "" {
		return LarkAppRegistrationBegin{}, fmt.Errorf("应用注册响应缺少必要字段")
	}
	return LarkAppRegistrationBegin{
		DeviceCode:              deviceCode,
		UserCode:                userCode,
		VerificationURI:         stringField(data, "verification_uri"),
		VerificationURIComplete: larkRegistrationVerificationURL(openBase, userCode),
		ExpiresIn:               intField(data, "expires_in", 3600),
		Interval:                intField(data, "interval", 5),
		Brand:                   brand,
	}, nil
}

func larkRegistrationVerificationURL(openBase, userCode string) string {
	u, err := url.Parse(openBase + "/page/cli")
	if err != nil {
		return fmt.Sprintf("%s/page/cli?user_code=%s&tp=%s", openBase, url.QueryEscape(userCode), url.QueryEscape(larkRegistrationTemplateID))
	}
	q := u.Query()
	q.Set("user_code", userCode)
	q.Set("tp", larkRegistrationTemplateID)
	u.RawQuery = q.Encode()
	return u.String()
}

func larkRegistrationBeginForm() url.Values {
	form := url.Values{}
	form.Set("action", "begin")
	form.Set("archetype", "PersonalAgent")
	form.Set("auth_method", "client_secret")
	form.Set("request_user_info", "open_id tenant_brand")
	form.Set("scope", strings.Join(larkRegistrationScopes, " "))
	form.Set("events", strings.Join(larkRegistrationEvents, " "))
	form.Set("callbacks", strings.Join(larkRegistrationCallbacks, " "))
	return form
}

func (c *larkAppRegistrationClient) Poll(ctx context.Context, brand, deviceCode string) (LarkAppRegistrationResult, error) {
	brand = normalizeRegistrationBrand(brand)
	deviceCode = strings.TrimSpace(deviceCode)
	if deviceCode == "" {
		return LarkAppRegistrationResult{}, fmt.Errorf("device_code is required")
	}
	accountsBase, _ := registrationBases(brand)
	form := url.Values{}
	form.Set("action", "poll")
	form.Set("device_code", deviceCode)
	data, err := c.postForm(ctx, accountsBase+larkRegistrationPath, form)
	if err != nil {
		return LarkAppRegistrationResult{}, err
	}
	if appID := stringField(data, "client_id"); appID != "" {
		userInfo, _ := data["user_info"].(map[string]any)
		openID := stringField(userInfo, "open_id")
		return LarkAppRegistrationResult{
			AppID:           appID,
			AppSecret:       stringField(data, "client_secret"),
			Brand:           brand,
			OpenID:          openID,
			NotifyReceiveID: openID,
		}, nil
	}
	switch errStr := stringField(data, "error"); errStr {
	case "authorization_pending", "slow_down":
		return LarkAppRegistrationResult{Brand: brand, Pending: true}, nil
	case "access_denied":
		return LarkAppRegistrationResult{}, fmt.Errorf("用户拒绝了应用注册")
	case "expired_token", "invalid_grant":
		return LarkAppRegistrationResult{}, fmt.Errorf("注册码已过期，请重新开始")
	case "":
		return LarkAppRegistrationResult{Brand: brand, Pending: true}, nil
	default:
		return LarkAppRegistrationResult{}, fmt.Errorf("应用注册失败: %s", registrationError(data))
	}
}

func (c *larkAppRegistrationClient) postForm(ctx context.Context, endpoint string, form url.Values) (map[string]any, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	var data map[string]any
	if err := json.Unmarshal(body, &data); err != nil {
		return nil, fmt.Errorf("HTTP %d returned non-JSON response", resp.StatusCode)
	}
	return data, nil
}

func normalizeRegistrationBrand(brand string) string {
	if strings.EqualFold(strings.TrimSpace(brand), "lark") {
		return "lark"
	}
	return "feishu"
}

func registrationBases(brand string) (string, string) {
	if brand == "lark" {
		return larksuiteAccountsBase, larksuiteOpenBase
	}
	return feishuAccountsBase, feishuOpenBase
}

func registrationError(data map[string]any) string {
	if desc := stringField(data, "error_description"); desc != "" {
		return desc
	}
	return stringField(data, "error")
}

func stringField(data map[string]any, key string) string {
	if v, ok := data[key].(string); ok {
		return v
	}
	return ""
}

func intField(data map[string]any, key string, fallback int) int {
	switch v := data[key].(type) {
	case float64:
		return int(v)
	case int:
		return v
	default:
		return fallback
	}
}

func qrPNG(text string) ([]byte, error) {
	return qrcode.Encode(text, qrcode.Medium, 256)
}
