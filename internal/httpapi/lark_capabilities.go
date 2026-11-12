package httpapi

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"strings"

	lark "github.com/larksuite/oapi-sdk-go/v3"
)

//go:embed lark_capabilities.json
var larkCapabilitiesJSON string

var larkCapabilities struct {
	Scopes    []string `json:"scopes"`
	Events    []string `json:"events"`
	Callbacks []string `json:"callbacks"`
}

func init() {
	if err := json.Unmarshal([]byte(larkCapabilitiesJSON), &larkCapabilities); err != nil {
		panic(err)
	}
}

func LarkCapabilitiesJSON() string { return larkCapabilitiesJSON }

func checkLarkCapabilities(ctx context.Context, client *lark.Client) error {
	resp, err := client.Application.V6.Scope.List(ctx)
	if err != nil {
		return fmt.Errorf("无法核验应用权限：%w", err)
	}
	if !resp.Success() {
		return fmt.Errorf("无法核验应用权限 code %d: %s", resp.Code, resp.Msg)
	}
	granted := map[string]bool{}
	if resp.Data != nil {
		for _, scope := range resp.Data.Scopes {
			if scope != nil && scope.ScopeName != nil && scope.GrantStatus != nil && *scope.GrantStatus == 1 && (scope.ScopeType == nil || *scope.ScopeType == "tenant") {
				granted[*scope.ScopeName] = true
			}
		}
	}
	missing := []string{}
	for _, scope := range larkCapabilities.Scopes {
		if !granted[scope] {
			missing = append(missing, scope)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("尚未开通功能权限：%s。对应功能可能受限，不影响保存；可前往应用后台申请并发布", strings.Join(missing, "、"))
	}
	return nil
}
