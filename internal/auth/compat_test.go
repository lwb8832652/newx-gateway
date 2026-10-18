package auth

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/cloudwego/hertz/pkg/app"
	"newx-gateway/internal/contracts"
	"newx-gateway/internal/middleware"
)

type failingTokenTransport struct{}

func (failingTokenTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, errors.New("private upstream business message")
}

func TestAuthFailureLogDoesNotLeakToken(t *testing.T) {
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	client := NewTokenClient(func(context.Context) (string, error) { return "https://auth.example", nil }, "", time.Second)
	client.HTTPClient.Transport = failingTokenTransport{}
	svc := New(client, Options{Enabled: true}, logger)
	c := app.NewContext(0)
	const token = "secret-token-do-not-log"
	c.Request.Header.Set(contracts.HeaderAuthorization, "Bearer "+token)
	svc.Middleware()(context.Background(), c)
	output := logs.String()
	for _, secret := range []string{token, "accessToken", "auth.example", "private upstream business message"} {
		if strings.Contains(output, secret) {
			t.Fatalf("authentication log leaked %q: %s", secret, output)
		}
	}
	if !strings.Contains(output, "error_type=*url.Error") {
		t.Fatalf("missing safe error classification: %s", output)
	}
}

func TestYudaoRawExpiresTimeContract(t *testing.T) {
	user := &LoginUser{ID: 42, UserType: 1, TenantID: 7, ExpiresTime: time.Date(2026, 1, 1, 8, 0, 0, 123000000, time.FixedZone("UTC+8", 8*3600))}
	header, err := user.HeaderValue()
	if err != nil {
		t.Fatal(err)
	}
	raw, err := url.QueryUnescape(header)
	if err != nil {
		t.Fatal(err)
	}
	const want = `{"id":42,"userType":1,"info":{},"tenantId":7,"scopes":[],"expiresTime":1767225600123}`
	if raw != want {
		t.Fatalf("wire JSON = %s, want %s", raw, want)
	}
	parsed, err := ParseLoginUser(url.QueryEscape(want))
	if err != nil || !parsed.ExpiresTime.Equal(user.ExpiresTime) {
		t.Fatalf("Java JSON parse: %+v, %v", parsed, err)
	}
}

func TestExpiresTimeFormatsAndTimezone(t *testing.T) {
	oldLocal := time.Local
	time.Local = time.FixedZone("UTC+8", 8*3600)
	defer func() { time.Local = oldLocal }()
	for _, raw := range []string{`1767225600123`, `"2026-01-01T08:00:00.123"`, `"2026-01-01T08:00:00.123+08:00"`, `"2026-01-01T00:00:00.123Z"`, `"2026-01-01 08:00:00.123"`} {
		got, err := parseJSONExpiresTime([]byte(raw))
		if err != nil || got.UnixMilli() != 1767225600123 {
			t.Errorf("%s: %v, %v", raw, got, err)
		}
	}
	for _, raw := range []string{`true`, `{}`, `1767225600.123`, `"not-a-date"`} {
		if _, err := parseJSONExpiresTime([]byte(raw)); err == nil {
			t.Errorf("accepted invalid time %s", raw)
		}
	}
}

func TestHeaderGuardAndAuthTenantBoundary(t *testing.T) {
	var tenants []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tenants = append(tenants, r.Header.Get("tenant-id"))
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Query().Get("accessToken") == "invalid" {
			_, _ = w.Write([]byte(`{"code":401,"data":null}`))
			return
		}
		_, _ = w.Write([]byte(`{"code":0,"data":{"userId":42,"userType":1,"userInfo":{},"tenantId":9,"scopes":[],"expiresTime":4070908800000}}`))
	}))
	defer server.Close()
	svc := New(NewTokenClient(func(context.Context) (string, error) { return server.URL, nil }, "", time.Second), Options{Enabled: true}, nil)
	cases := []struct {
		tenant, token, wantTenant string
		loggedIn                  bool
	}{
		{"7", "", "7", false},
		{"7", "invalid", "7", false},
		{"8", "invalid", "8", false},
		{"7", "valid", "9", true},
		{"8", "valid", "9", true},
		{"7", "valid", "9", true},
		{"", "valid", "9", true},
		{"0", "valid", "9", true},
		{"", "valid", "9", true},
		{"0", "valid", "9", true},
		{"", "invalid", "", false},
		{"0", "invalid", "0", false},
		{"", "invalid", "", false},
		{"0", "invalid", "0", false},
		{"-1", "", "", false},
		{"abc", "", "", false},
		{"9223372036854775808", "", "", false},
	}
	for _, tc := range cases {
		c := app.NewContext(0)
		c.Request.Header.Set(contracts.HeaderTenantID, tc.tenant)
		c.Request.Header.Set(contracts.HeaderLoginUser, "forged-admin")
		if tc.token != "" {
			c.Request.Header.Set(contracts.HeaderAuthorization, "Bearer "+tc.token)
		}
		middleware.HeaderGuard(false)(context.Background(), c)
		svc.Middleware()(context.Background(), c)
		if got := c.Request.Header.Get(contracts.HeaderTenantID); got != tc.wantTenant {
			t.Errorf("tenant %s token %s: got %q", tc.tenant, tc.token, got)
		}
		login := c.Request.Header.Get(contracts.HeaderLoginUser)
		principal, exists := c.Get(contracts.CtxPrincipalID)
		if tc.loggedIn {
			user, err := ParseLoginUser(login)
			if err != nil || user.ID != 42 || user.TenantID != 9 || principal != "42" {
				t.Fatalf("trusted identity: %+v, %v, principal %v", user, err, principal)
			}
		} else if login != "" || exists {
			t.Fatalf("anonymous request retained identity: %q, %v", login, principal)
		}
	}
	wantTenants := []string{"7", "8", "7", "8", "", "0", "", "0"}
	if len(tenants) != len(wantTenants) {
		t.Fatalf("cache must isolate positive/negative entries and missing/zero tenants: %v", tenants)
	}
	for i, want := range wantTenants {
		if tenants[i] != want {
			t.Fatalf("check-token tenant at call %d = %q, want %q", i, tenants[i], want)
		}
	}
}
