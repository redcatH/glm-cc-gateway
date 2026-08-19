package mimic

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func TestParseMetadataUserIDJSON(t *testing.T) {
	raw := `{"device_id":"0afd57113bc896505af27e60910d5f72395e0f11fb352ecab1162cc752e015cb","account_uuid":"","session_id":"c0a7919d-cdc3-4c3f-a098-3223f408871a"}`
	parsed := ParseMetadataUserID(raw)
	if parsed == nil {
		t.Fatal("expected parse success")
	}
	if parsed.DeviceID != "0afd57113bc896505af27e60910d5f72395e0f11fb352ecab1162cc752e015cb" {
		t.Fatalf("device_id mismatch: %s", parsed.DeviceID)
	}
	if parsed.SessionID != "c0a7919d-cdc3-4c3f-a098-3223f408871a" {
		t.Fatalf("session_id mismatch: %s", parsed.SessionID)
	}
	if !parsed.IsNewFormat {
		t.Fatal("expected new format")
	}
}

func TestFormatMetadataUserIDJSONShape(t *testing.T) {
	got := FormatMetadataUserID("abcd", "", "sess-1", "2.1.220")
	var m map[string]string
	if err := json.Unmarshal([]byte(got), &m); err != nil {
		t.Fatalf("not JSON: %v", err)
	}
	if m["device_id"] != "abcd" || m["account_uuid"] != "" || m["session_id"] != "sess-1" {
		t.Fatalf("fields mismatch: %v", m)
	}
}

func TestFormatMetadataUserIDLegacy(t *testing.T) {
	got := FormatMetadataUserID("abcd", "uuid-1", "sess-1", "2.1.50")
	want := "user_abcd_account_uuid-1_session_sess-1"
	if got != want {
		t.Fatalf("legacy format mismatch: %s", got)
	}
}

func TestAPIKeyMimicryBetasExcludeOAuth(t *testing.T) {
	betas := APIKeyMimicryBetas()
	joined := strings.Join(betas, ",")
	if strings.Contains(joined, BetaOAuth) {
		t.Fatalf("API-key mimicry betas must not contain %s: %s", BetaOAuth, joined)
	}
	full := FullClaudeCodeMimicryBetas()
	if len(betas) != len(full)-1 {
		t.Fatalf("expected full-minus-oauth (%d items), got %d", len(full)-1, len(betas))
	}
}

func TestBuildUpstreamHeaders(t *testing.T) {
	h := BuildUpstreamHeaders(HeaderInput{
		APIKey:     "sk-test",
		AuthScheme: "bearer",
		IsStream:   true,
		SessionID:  "sess-uuid",
		RetryCount: 0,
	})

	checks := []struct{ key, want string }{
		{"User-Agent", "claude-cli/2.1.220 (external, cli)"},
		{"x-app", "cli"},
		{"x-stainless-os", "Linux"},
		{"x-stainless-arch", "arm64"},
		{"x-stainless-runtime-version", "v24.3.0"},
		{"anthropic-version", "2023-06-01"},
		{"anthropic-dangerous-direct-browser-access", "true"},
		{"accept", "application/json"},
		{"content-type", "application/json"},
		{"x-stainless-helper-method", "stream"},
		{"X-Claude-Code-Session-Id", "sess-uuid"},
	}
	for _, c := range checks {
		if got := getHeaderRaw(h, c.key); got != c.want {
			t.Errorf("header %s = %q, want %q", c.key, got, c.want)
		}
	}

	// wire casing:实际 key 必须保留真实 CLI 大小写形态(map 中的 key 即 wire 形态)。
	for _, key := range []string{
		"X-Stainless-OS",
		"x-stainless-helper-method",
		"anthropic-beta",
		"X-Claude-Code-Session-Id",
		"anthropic-dangerous-direct-browser-access",
		"anthropic-version",
		"x-app",
		"x-client-request-id",
	} {
		if _, ok := h[key]; !ok {
			t.Errorf("header key %q not present in wire form (keys: %v)", key, headerKeys(h))
		}
	}

	if got := getHeaderRaw(h, "authorization"); got != "Bearer sk-test" {
		t.Errorf("authorization = %q", got)
	}
	beta := getHeaderRaw(h, "anthropic-beta")
	if !strings.HasPrefix(beta, "claude-code-20250219,interleaved-thinking-2025-05-14") {
		t.Errorf("anthropic-beta = %q", beta)
	}
	if strings.Contains(beta, BetaOAuth) {
		t.Errorf("anthropic-beta must not contain oauth beta: %q", beta)
	}
	if getHeaderRaw(h, "x-client-request-id") == "" {
		t.Error("x-client-request-id must be set")
	}
}

func TestBuildUpstreamHeadersXAPIKeyAndRetry(t *testing.T) {
	h := BuildUpstreamHeaders(HeaderInput{
		APIKey:     "sk-test",
		AuthScheme: "x-api-key",
		RetryCount: 2,
	})
	if got := getHeaderRaw(h, "x-api-key"); got != "sk-test" {
		t.Fatalf("x-api-key = %q", got)
	}
	if got := getHeaderRaw(h, "x-stainless-retry-count"); got != "2" {
		t.Fatalf("retry-count = %q, want 2", got)
	}
	if getHeaderRaw(h, "x-stainless-helper-method") != "" {
		t.Fatal("helper-method should be absent for non-stream")
	}
}

func TestRewriteMetadataUserIDInjectsAndRewrites(t *testing.T) {
	id := Identity{ClientID: strings.Repeat("a", 64)}
	body, _ := json.Marshal(map[string]any{
		"model": "m",
		"messages": []any{
			map[string]any{"role": "user", "content": []any{map[string]any{"type": "text", "text": "hi"}}},
		},
	})

	// 1. 无 metadata → 注入。
	out, sess1 := RewriteMetadataUserID(body, id, RewriteInput{IdentityKey: "keyhash", ClientIP: "1.2.3.4", UserAgent: "opencode/1.0"})
	var top map[string]any
	if err := json.Unmarshal(out, &top); err != nil {
		t.Fatal(err)
	}
	meta, _ := top["metadata"].(map[string]any)
	if meta == nil || meta["user_id"] == nil {
		t.Fatalf("metadata.user_id not injected: %v", top["metadata"])
	}
	var uid map[string]string
	if err := json.Unmarshal([]byte(meta["user_id"].(string)), &uid); err != nil {
		t.Fatalf("user_id not JSON: %v", err)
	}
	if uid["device_id"] != id.ClientID || uid["account_uuid"] != "" || uid["session_id"] != sess1 {
		t.Fatalf("user_id fields mismatch: %v", uid)
	}
	// 原始顶层字段保留。
	if top["model"] != "m" {
		t.Fatalf("model lost: %v", top["model"])
	}

	// 2. 同输入 → session 稳定。
	_, sess2 := RewriteMetadataUserID(body, id, RewriteInput{IdentityKey: "keyhash", ClientIP: "1.2.3.4", UserAgent: "opencode/1.0"})
	if sess1 != sess2 {
		t.Fatalf("session not stable: %s vs %s", sess1, sess2)
	}

	// 3. 下游带了 metadata.user_id → 重写 device_id/session,account_uuid 清空。
	uidJSON, _ := json.Marshal(map[string]string{
		"device_id":    strings.Repeat("f", 64),
		"account_uuid": "leak-uuid",
		"session_id":   "11111111-2222-3333-4444-555555555555",
	})
	bodyWithMeta, _ := json.Marshal(map[string]any{
		"metadata": map[string]any{"user_id": string(uidJSON)},
		"messages": []any{
			map[string]any{"role": "user", "content": []any{map[string]any{"type": "text", "text": "hi"}}},
		},
	})
	out3, sess3 := RewriteMetadataUserID(bodyWithMeta, id, RewriteInput{
		IdentityKey:   "keyhash",
		SessionAnchor: "11111111-2222-3333-4444-555555555555",
	})
	var top3 struct {
		Metadata struct {
			UserID string `json:"user_id"`
		} `json:"metadata"`
	}
	if err := json.Unmarshal(out3, &top3); err != nil {
		t.Fatal(err)
	}
	parsed := ParseMetadataUserID(top3.Metadata.UserID)
	if parsed == nil {
		t.Fatal("rewritten user_id unparseable")
	}
	if parsed.DeviceID != id.ClientID {
		t.Fatalf("device_id not rewritten: %s", parsed.DeviceID)
	}
	if parsed.AccountUUID != "" {
		t.Fatalf("account_uuid must be cleared, got %q", parsed.AccountUUID)
	}
	if parsed.SessionID != sess3 {
		t.Fatalf("session header sync mismatch: %s vs %s", parsed.SessionID, sess3)
	}

	// 4. 锚点变化 → session 变化(不同下游会话不同 session)。
	_, sess4 := RewriteMetadataUserID(bodyWithMeta, id, RewriteInput{
		IdentityKey:   "keyhash",
		SessionAnchor: "99999999-2222-3333-4444-555555555555",
	})
	if sess3 == sess4 {
		t.Fatal("different anchors must derive different sessions")
	}
}

func headerKeys(h http.Header) []string {
	keys := make([]string, 0, len(h))
	for k := range h {
		keys = append(keys, k)
	}
	return keys
}
