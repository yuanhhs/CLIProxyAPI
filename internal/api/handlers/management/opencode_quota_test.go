package management

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

func TestParseOpenCodeQuota(t *testing.T) {
	html := `<html><script>$R[4] = {mine:!0,useBalance:!1,rollingUsage:{status:"ok",resetInSec:120,usagePercent:22},weeklyUsage:{status:"ok",resetInSec:240,usagePercent:9},monthlyUsage:{status:"ok",resetInSec:360,usagePercent:7}}</script></html>`
	quota, errParse := parseOpenCodeQuota(html)
	if errParse != nil {
		t.Fatalf("parseOpenCodeQuota() error = %v", errParse)
	}
	if quota.Rolling.UsedPercent != 22 || quota.Rolling.RemainingPercent != 78 {
		t.Fatalf("rolling quota = %+v", quota.Rolling)
	}
	if quota.Weekly.UsedPercent != 9 || quota.Monthly.UsedPercent != 7 {
		t.Fatalf("weekly/monthly quota = %+v / %+v", quota.Weekly, quota.Monthly)
	}
}

func TestParseOpenCodeQuotaRejectsMissingData(t *testing.T) {
	_, errParse := parseOpenCodeQuota(`<html><script>$R[0] = {unrelated:!0}</script></html>`)
	if errParse == nil {
		t.Fatal("parseOpenCodeQuota() error = nil, want error")
	}
}

func TestFetchOpenCodeQuota(t *testing.T) {
	var receivedCookie string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedCookie = r.Header.Get("Cookie")
		if r.URL.EscapedPath() != "/workspace/workspace%20one/go" {
			t.Fatalf("escaped path = %q", r.URL.EscapedPath())
		}
		_, _ = w.Write([]byte(`<script>$R[1] = {rollingUsage:{status:"ok",resetInSec:1,usagePercent:10},weeklyUsage:{status:"ok",resetInSec:2,usagePercent:20},monthlyUsage:{status:"ok",resetInSec:3,usagePercent:30}}</script>`))
	}))
	defer server.Close()

	quota, errFetch := fetchOpenCodeQuota(context.Background(), server.Client(), server.URL, "session=test", "workspace one")
	if errFetch != nil {
		t.Fatalf("fetchOpenCodeQuota() error = %v", errFetch)
	}
	if receivedCookie != "session=test" {
		t.Fatalf("cookie = %q", receivedCookie)
	}
	if quota.Monthly.RemainingPercent != 70 {
		t.Fatalf("monthly quota = %+v", quota.Monthly)
	}
}

func TestFetchOpenCodeQuotaRejectsLoginRedirect(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Location", "/login")
		w.WriteHeader(http.StatusFound)
	}))
	defer server.Close()
	client := server.Client()
	client.CheckRedirect = func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }

	_, errFetch := fetchOpenCodeQuota(context.Background(), client, server.URL, "expired", "workspace")
	if errFetch == nil {
		t.Fatal("fetchOpenCodeQuota() error = nil, want error")
	}
	upstreamErr, ok := errFetch.(*openCodeUpstreamError)
	if !ok || upstreamErr.StatusCode != http.StatusFound {
		t.Fatalf("error = %#v", errFetch)
	}
}

func TestGetOpenCodeQuotaRejectsUnknownAuthFile(t *testing.T) {
	gin.SetMode(gin.TestMode)
	h := &Handler{authManager: coreauth.NewManager(nil, nil, nil)}
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/opencode/quota", bytes.NewBufferString(`{"name":"missing.json"}`))
	ctx.Request.Header.Set("Content-Type", "application/json")

	h.GetOpenCodeQuota(ctx)

	if recorder.Code != http.StatusNotFound {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
}

func TestGetOpenCodeQuotaRejectsIncompleteAuthFile(t *testing.T) {
	gin.SetMode(gin.TestMode)
	manager := coreauth.NewManager(nil, nil, nil)
	_, errRegister := manager.Register(context.Background(), &coreauth.Auth{
		ID:       "opencode-test.json",
		Provider: "opencode",
		Metadata: map[string]any{"workspace_id": "workspace-one"},
	})
	if errRegister != nil {
		t.Fatalf("register auth: %v", errRegister)
	}

	h := &Handler{authManager: manager}
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/opencode/quota", bytes.NewBufferString(`{"name":"opencode-test.json"}`))
	ctx.Request.Header.Set("Content-Type", "application/json")

	h.GetOpenCodeQuota(ctx)

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
}
