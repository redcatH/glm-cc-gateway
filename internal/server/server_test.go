package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"glm-cc-gateway/internal/config"
	"glm-cc-gateway/internal/mimic"
)

func TestRouteAliases(t *testing.T) {
	// 假上游:两种路径风格的请求都应成功转发。
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"ok":true}`)) //nolint:errcheck
	}))
	t.Cleanup(upstream.Close)

	cfg := &config.Config{
		UpstreamBaseURL:    upstream.URL,
		UpstreamAuthScheme: "bearer",
		UpstreamPath:       "/v1/messages?beta=true",
		CountTokensPath:    "/v1/messages/count_tokens?beta=true",
		MaxBodyBytes:       1 << 20,
	}
	ident, _ := mimic.NewIdentityStore("")
	handler := New(cfg, ident)

	body := `{"model":"m","messages":[{"role":"user","content":"hi"}],"stream":false}`
	for _, path := range []string{"/v1/messages", "/messages", "/v1/messages/count_tokens", "/messages/count_tokens"} {
		req, _ := http.NewRequest(http.MethodPost, "http://gw"+path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer k")
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Errorf("POST %s = %d, want 200", path, rec.Code)
		}
	}

	// 未知路径 → 404(有诊断日志)。
	req, _ := http.NewRequest(http.MethodGet, "http://gw/v1/models", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("GET /v1/models = %d, want 404", rec.Code)
	}
}
