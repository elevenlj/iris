package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	lark "github.com/larksuite/oapi-sdk-go/v3"
)

func TestLarkCapabilitiesRequireAllGrantedTenantScopes(t *testing.T) {
	for i, mode := range []string{"complete", "missing", "denied", "user", "api-error"} {
		t.Run(mode, func(t *testing.T) {
			client := &http.Client{Transport: identityTransport(func(r *http.Request) (*http.Response, error) {
				body := `{"code":0,"tenant_access_token":"capabilities-token","expire":7200}`
				if r.URL.Path == "/open-apis/application/v6/scopes" {
					if r.Header.Get("Authorization") != "Bearer capabilities-token" {
						t.Fatal("wrong tenant identity")
					}
					scopes := []map[string]any{}
					for _, name := range larkCapabilities.Scopes {
						if name == "im:message.group_msg" && mode == "missing" {
							continue
						}
						scope := map[string]any{"scope_name": name, "grant_status": 1, "scope_type": "tenant"}
						if name == "im:message.group_msg" && mode == "denied" {
							scope["grant_status"] = 2
						}
						if name == "im:message.group_msg" && mode == "user" {
							scope["scope_type"] = "user"
						}
						scopes = append(scopes, scope)
					}
					data, _ := json.Marshal(map[string]any{"code": 0, "data": map[string]any{"scopes": scopes}})
					body = string(data)
					if mode == "api-error" {
						body = `{"code":212100,"msg":"app not install"}`
					}
				}
				return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(body))}, nil
			})}
			err := checkLarkCapabilities(context.Background(), lark.NewClient(fmt.Sprintf("capabilities-%d", i), "secret", lark.WithHttpClient(client)))
			if mode == "complete" && err != nil {
				t.Fatal(err)
			}
			if mode != "complete" && err == nil {
				t.Fatal("incomplete permissions accepted")
			}
			if mode != "complete" && mode != "api-error" && !strings.Contains(err.Error(), "im:message.group_msg") {
				t.Fatal(err)
			}
		})
	}
}

func TestRegistrationIncludesEveryRequiredCapability(t *testing.T) {
	form := larkRegistrationBeginForm()
	for key, required := range map[string][]string{"scope": larkCapabilities.Scopes, "events": larkCapabilities.Events, "callbacks": larkCapabilities.Callbacks} {
		got := " " + form.Get(key) + " "
		for _, item := range required {
			if !strings.Contains(got, " "+item+" ") {
				t.Fatalf("%s omits %s", key, item)
			}
		}
	}
}

func TestLarkConfigAllowsMissingCapabilitiesWithoutSending(t *testing.T) {
	previous := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = previous })
	http.DefaultTransport = identityTransport(func(r *http.Request) (*http.Response, error) {
		body := `{"code":0,"tenant_access_token":"capability-check-token","expire":7200}`
		switch r.URL.Path {
		case "/open-apis/auth/v3/tenant_access_token/internal":
		case "/open-apis/application/v6/scopes":
			body = `{"code":0,"data":{"scopes":[]}}`
		default:
			t.Fatalf("must not send or configure incomplete apps: %s", r.URL.Path)
		}
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(body))}, nil
	})
	result := TestLarkConfig(RuntimeConfig{LarkAppID: "cli_capability_check", LarkAppSecret: "secret", LarkNotifyReceiveID: "ou_owner"})
	if !result.OK || len(result.Steps) != 3 || !result.Steps[2].Warning || !strings.Contains(result.Steps[2].Message, "contact:user.id:readonly") {
		t.Fatalf("missing capabilities must warn without blocking: %+v", result)
	}
}

func TestLarkConfigInvalidCredentialsStillBlock(t *testing.T) {
	previous := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = previous })
	http.DefaultTransport = identityTransport(func(r *http.Request) (*http.Response, error) {
		if !strings.Contains(r.URL.Path, "tenant_access_token") {
			t.Fatalf("invalid credentials must stop testing: %s", r.URL.Path)
		}
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"code":10014,"msg":"invalid app secret"}`))}, nil
	})
	result := TestLarkConfig(RuntimeConfig{LarkAppID: "cli_bad_secret", LarkAppSecret: "invalid", LarkNotifyReceiveID: "ou_owner"})
	if result.OK || len(result.Steps) != 2 || result.Steps[1].Warning {
		t.Fatalf("invalid credentials accepted: %+v", result)
	}
}
