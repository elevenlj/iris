package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

func larkJSON(ctx context.Context, method, path, token string, body any) (map[string]json.RawMessage, error) {
	data, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, method, feishuOpenBase+path, bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	response, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	var result map[string]json.RawMessage
	if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&result); err != nil {
		return nil, errors.New("飞书返回无效响应")
	}
	var code int
	if err := json.Unmarshal(result["code"], &code); err != nil || code != 0 || response.StatusCode >= 300 {
		return nil, fmt.Errorf("飞书接口验证失败（HTTP %d, code %d）", response.StatusCode, code)
	}
	return result, nil
}

func larkIdentityToken(ctx context.Context, appID, secret string) (string, error) {
	result, err := larkJSON(ctx, http.MethodPost, "/open-apis/auth/v3/tenant_access_token/internal", "", map[string]string{"app_id": appID, "app_secret": secret})
	if err != nil {
		return "", err
	}
	var token string
	_ = json.Unmarshal(result["tenant_access_token"], &token)
	if token == "" {
		return "", errors.New("未返回访问令牌")
	}
	return token, nil
}

func ResolveLarkOwner(ctx context.Context, appID, secret, email string) (string, error) {
	if strings.TrimSpace(email) == "" {
		return "", errors.New("开放平台未返回登录人的邮箱")
	}
	token, err := larkIdentityToken(ctx, appID, secret)
	if err != nil {
		return "", err
	}
	result, err := larkJSON(ctx, http.MethodPost, "/open-apis/contact/v3/users/batch_get_id?user_id_type=open_id", token, map[string]any{"emails": []string{email}})
	if err != nil {
		return "", err
	}
	var data struct {
		Users []struct {
			ID string `json:"user_id"`
		} `json:"user_list"`
	}
	if err := json.Unmarshal(result["data"], &data); err != nil || len(data.Users) != 1 || !strings.HasPrefix(data.Users[0].ID, "ou_") {
		return "", errors.New("无法解析此应用下的开发者 open_id")
	}
	if _, err := larkJSON(ctx, http.MethodGet, "/open-apis/contact/v3/users/"+url.PathEscape(data.Users[0].ID)+"?user_id_type=open_id", token, nil); err != nil {
		return "", fmt.Errorf("用户基本信息权限验证失败：%w", err)
	}
	return data.Users[0].ID, nil
}

func LarkAppName(ctx context.Context, appID, secret string) (string, error) {
	token, err := larkIdentityToken(ctx, appID, secret)
	if err != nil {
		return "", err
	}
	result, err := larkJSON(ctx, http.MethodGet, "/open-apis/bot/v3/info", token, nil)
	if err != nil {
		return "", err
	}
	var bot struct {
		Name string `json:"app_name"`
	}
	if err := json.Unmarshal(result["bot"], &bot); err != nil {
		return "", errors.New("飞书未返回应用信息")
	}
	return bot.Name, nil
}
