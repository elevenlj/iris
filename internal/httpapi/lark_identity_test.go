package httpapi

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
)

type identityTransport func(*http.Request) (*http.Response, error)

func (f identityTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestResolveLarkOwnerValidatesAppScopedIdentityAndBasicPermission(t *testing.T) {
	old := http.DefaultTransport
	defer func() { http.DefaultTransport = old }()
	for _, denied := range []bool{false, true} {
		checkedBasic := false
		http.DefaultTransport = identityTransport(func(r *http.Request) (*http.Response, error) {
			body := `{"code":0,"tenant_access_token":"private-token"}`
			if strings.Contains(r.URL.Path, "batch_get_id") {
				if r.URL.Query().Get("user_id_type") != "open_id" {
					t.Error("must resolve application-scoped open_id")
				}
				body = `{"code":0,"data":{"user_list":[{"user_id":"ou_owner"}]}}`
			} else if strings.Contains(r.URL.Path, "users/ou_owner") {
				checkedBasic = true
				if r.Header.Get("Authorization") != "Bearer private-token" {
					t.Error("wrong token")
				}
				body = `{"code":0,"data":{"user":{"name":"Owner"}}}`
				if denied {
					body = `{"code":99991672,"msg":"permission denied"}`
				}
			}
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
		})
		id, err := ResolveLarkOwner(context.Background(), "cli_test", "secret", "owner@example.com")
		if !checkedBasic || (denied && err == nil) || (!denied && (err != nil || id != "ou_owner")) {
			t.Fatalf("permission validation failed: denied=%v, err=%v", denied, err)
		}
	}
}
