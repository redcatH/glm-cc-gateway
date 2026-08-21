package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"glm-cc-gateway/internal/config"
	"glm-cc-gateway/internal/mimic"
)

// startStack 启动 假上游 + 网关,返回网关地址与收到的上游请求记录。
type upstreamCapture struct {
	Headers http.Header
	Body    []byte
	URL     string
}

func startStack(t *testing.T, authScheme string) (*upstreamCapture, string) {
	t.Helper()
	cap := &upstreamCapture{}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cap.Headers = r.Header.Clone()
		cap.URL = r.URL.String()
		cap.Body, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"text","text":"pong"}],"model":"m","stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`) //nolint:errcheck
	}))
	t.Cleanup(upstream.Close)

	cfg := &config.Config{
		Listen:             "unused",
		UpstreamBaseURL:    upstream.URL,
		UpstreamAuthScheme: authScheme,
		UpstreamPath:       "/v1/messages?beta=true",
		CountTokensPath:    "/v1/messages/count_tokens?beta=true",
		IdentityFile:       "", // 内存态
		MaxBodyBytes:       1 << 20,
		MaxUpstreamRetries: 0,
	}
	ident, err := mimic.NewIdentityStore("")
	if err != nil {
		t.Fatal(err)
	}
	f := New(cfg, ident)
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/messages", f.HandleMessages(cfg.UpstreamPath, nil))
	gw := httptest.NewServer(mux)
	t.Cleanup(gw.Close)
	return cap, gw.URL
}

func postMessages(t *testing.T, base string, headers map[string]string, body string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, base+"/v1/messages", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func TestForwardRewritesHeadersAndMetadata(t *testing.T) {
	cap, gw := startStack(t, "bearer")

	downstreamBody := `{
		"model": "glm-4.6",
		"system": "You are a helpful assistant.",
		"messages": [
			{"role": "user", "content": [{"type": "text", "text": "hi"}]}
		],
		"stream": false,
		"temperature": 0.7,
		"metadata": {"user_id": "{\"device_id\":\"ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff\",\"account_uuid\":\"leak\",\"session_id\":\"aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee\"}"}
	}`
	resp := postMessages(t, gw, map[string]string{
		"Authorization":  "Bearer sk-downstream-1",
		"User-Agent":     "opencode/1.2.3",
		"X-Some-Client":  "leak-header",
		"anthropic-beta": "leak-beta",
		"x-api-key":      "leak-key",
	}, downstreamBody)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}

	// 上游 URL 带 beta=true。
	if !strings.Contains(cap.URL, "/v1/messages?beta=true") {
		t.Fatalf("upstream url = %s", cap.URL)
	}

	// 关键伪装头。
	if ua := cap.Headers.Get("User-Agent"); ua != "claude-cli/2.1.220 (external, cli)" {
		t.Errorf("User-Agent = %q", ua)
	}
	if v := cap.Headers.Get("X-App"); v != "cli" {
		t.Errorf("X-App = %q", v)
	}
	if v := cap.Headers.Get("X-Stainless-Lang"); v != "js" {
		t.Errorf("X-Stainless-Lang = %q", v)
	}
	// 认证:仅 Bearer 下游 key,无 x-api-key 残留。
	if v := cap.Headers.Get("Authorization"); v != "Bearer sk-downstream-1" {
		t.Errorf("Authorization = %q", v)
	}
	if v := cap.Headers.Get("x-api-key"); v == "leak-key" {
		t.Errorf("downstream x-api-key leaked: %q", v)
	}
	// 下游 UA / 自定义头 / beta 不得透传。
	if v := cap.Headers.Get("X-Some-Client"); v != "" {
		t.Errorf("downstream header leaked: %q", v)
	}
	if v := cap.Headers.Get("anthropic-beta"); strings.Contains(v, "leak-beta") {
		t.Errorf("downstream beta leaked: %q", v)
	}
	// beta 为 API-key 伪装集(无 oauth)。
	beta := cap.Headers.Get("anthropic-beta")
	if !strings.HasPrefix(beta, "claude-code-20250219,") || strings.Contains(beta, "oauth-2025-04-20") {
		t.Errorf("anthropic-beta = %q", beta)
	}
	// Session 头存在。
	sessHdr := cap.Headers.Get("X-Claude-Code-Session-Id")
	if sessHdr == "" {
		t.Fatal("X-Claude-Code-Session-Id missing")
	}

	// body:metadata.user_id 被重写为伪装身份,device_id 与下游不同。
	var top map[string]json.RawMessage
	if err := json.Unmarshal(cap.Body, &top); err != nil {
		t.Fatal(err)
	}
	var meta struct {
		UserID string `json:"user_id"`
	}
	if err := json.Unmarshal(top["metadata"], &meta); err != nil {
		t.Fatalf("metadata: %v", err)
	}
	parsed := mimic.ParseMetadataUserID(meta.UserID)
	if parsed == nil {
		t.Fatalf("user_id unparseable: %s", meta.UserID)
	}
	if parsed.DeviceID == strings.Repeat("f", 64) {
		t.Fatal("downstream device_id leaked")
	}
	if parsed.AccountUUID == "leak" {
		t.Fatal("downstream account_uuid leaked")
	}
	if parsed.SessionID != sessHdr {
		t.Fatalf("session header (%s) != body session (%s)", sessHdr, parsed.SessionID)
	}
	if parsed.SessionID == "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee" {
		t.Fatal("downstream session_id leaked")
	}

	// 下游原始字段(model/system/messages)原样保留。
	if !bytes.Contains(cap.Body, []byte(`"glm-4.6"`)) {
		t.Error("model lost")
	}
	if !bytes.Contains(cap.Body, []byte("You are a helpful assistant.")) {
		t.Error("system lost")
	}

	// 响应透传。
	b, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(b), "pong") {
		t.Fatalf("response body = %s", b)
	}
}

func TestForwardDeviceIDStablePerKey(t *testing.T) {
	cap, gw := startStack(t, "x-api-key")
	body := `{"model":"m","messages":[{"role":"user","content":"hi"}],"stream":false}`

	first := captureUserID(t, cap, gw, body, "sk-key-A")
	second := captureUserID(t, cap, gw, body, "sk-key-A")
	other := captureUserID(t, cap, gw, body, "sk-key-B")

	if first.DeviceID != second.DeviceID {
		t.Fatal("same key must have stable device_id")
	}
	if first.DeviceID == other.DeviceID {
		t.Fatal("different keys must have different device_id")
	}
	// x-api-key 方案。
	if v := cap.Headers.Get("x-api-key"); v != "sk-key-B" {
		t.Errorf("x-api-key = %q", v)
	}
}

func captureUserID(t *testing.T, cap *upstreamCapture, gw, body, key string) *mimic.ParsedUserID {
	t.Helper()
	postMessages(t, gw, map[string]string{"x-api-key": key}, body)
	var top struct {
		Metadata struct {
			UserID string `json:"user_id"`
		} `json:"metadata"`
	}
	if err := json.Unmarshal(cap.Body, &top); err != nil {
		t.Fatal(err)
	}
	parsed := mimic.ParseMetadataUserID(top.Metadata.UserID)
	if parsed == nil {
		t.Fatalf("user_id unparseable: %s", top.Metadata.UserID)
	}
	return parsed
}

func TestForwardStreamingPassthrough(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)
		for _, chunk := range []string{"data: {\"a\":1}\n\n", "data: {\"b\":2}\n\n", "data: [DONE]\n\n"} {
			io.WriteString(w, chunk) //nolint:errcheck
			flusher.Flush()
		}
	}))
	t.Cleanup(upstream.Close)

	cfg := &config.Config{
		UpstreamBaseURL:    upstream.URL,
		UpstreamAuthScheme: "bearer",
		UpstreamPath:       "/v1/messages?beta=true",
		MaxBodyBytes:       1 << 20,
	}
	ident, _ := mimic.NewIdentityStore("")
	f := New(cfg, ident)
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/messages", f.HandleMessages(cfg.UpstreamPath, nil))
	gw := httptest.NewServer(mux)
	t.Cleanup(gw.Close)

	resp := postMessages(t, gw.URL, map[string]string{"Authorization": "Bearer k"},
		`{"model":"m","messages":[{"role":"user","content":"hi"}],"stream":true}`)
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
		t.Fatalf("content-type = %q", ct)
	}
	b, _ := io.ReadAll(resp.Body)
	for _, want := range []string{`{"a":1}`, `{"b":2}`, "[DONE]"} {
		if !strings.Contains(string(b), want) {
			t.Fatalf("stream chunk %q missing in %s", want, b)
		}
	}
}

func TestForwardMissingKey(t *testing.T) {
	_, gw := startStack(t, "bearer")
	resp := postMessages(t, gw, nil, `{"model":"m","messages":[]}`)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
}

func TestOrderedHeadersPreservesWireValues(t *testing.T) {
	// 回归:dump 曾用 http.Header.Get(canonical 查找)读 wire 形态 key,导致值全为空。
	h := mimic.BuildUpstreamHeaders(mimic.HeaderInput{
		APIKey: "sk-x", AuthScheme: "bearer", SessionID: "sess", IsStream: true,
	})
	got := orderedHeaders(h)
	m := map[string]string{}
	for _, e := range got {
		m[e["key"]] = e["value"]
	}
	for _, k := range []string{
		"anthropic-version", "anthropic-beta", "authorization", "x-app",
		"content-type", "x-client-request-id", "accept-encoding",
		"X-Stainless-OS", "anthropic-dangerous-direct-browser-access",
	} {
		if m[k] == "" {
			t.Errorf("dump lost value for %q", k)
		}
	}
	if m["authorization"] != "Bearer sk-x" || m["anthropic-version"] != "2023-06-01" {
		t.Errorf("values wrong: auth=%q ver=%q", m["authorization"], m["anthropic-version"])
	}
}

func TestForwardThirdPartyClientFullRewrite(t *testing.T) {
	cap, gw := startStack(t, "bearer")
	body := `{
		"model": "claude-sonnet-4-5",
		"system": "You are a helpful assistant.",
		"messages": [{"role": "user", "content": [{"type": "text", "text": "hi"}]}],
		"stream": false
	}`
	resp := postMessages(t, gw, map[string]string{"Authorization": "Bearer k"}, body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}

	var top struct {
		System []struct {
			Text string `json:"text"`
		} `json:"system"`
		Messages []struct {
			Role    string `json:"role"`
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"messages"`
		Tools       []any  `json:"tools"`
		Temperature *int   `json:"temperature"`
		MaxTokens   *int64 `json:"max_tokens"`
	}
	if err := json.Unmarshal(cap.Body, &top); err != nil {
		t.Fatal(err)
	}
	// system 已重写为 CC 3-block。
	if len(top.System) != 3 || !strings.Contains(top.System[0].Text, "x-anthropic-billing-header: cc_version=") {
		t.Fatalf("system not rewritten: %+v", top.System)
	}
	if top.System[1].Text != mimic.CCSystemPrompt {
		t.Fatalf("banner missing: %q", top.System[1].Text)
	}
	// 原 system 降级注入。
	if len(top.Messages) != 3 || !strings.HasPrefix(top.Messages[0].Content[0].Text, "[System Instructions]") {
		t.Fatalf("degraded system not injected: %d messages", len(top.Messages))
	}
	// 参数补齐。
	if top.Tools == nil || top.Temperature == nil || *top.Temperature != 1 || top.MaxTokens == nil || *top.MaxTokens != 128000 {
		t.Fatalf("params not normalized: tools=%v temp=%v max=%v", top.Tools, top.Temperature, top.MaxTokens)
	}
}

func TestForwardRealCCBodyKeepsSystem(t *testing.T) {
	cap, gw := startStack(t, "bearer")
	// 真 CC 直连:system 自带 billing block → 不重写,只同步版本。
	body := `{
		"model": "glm-5.2",
		"system": [
			{"type": "text", "text": "x-anthropic-billing-header: cc_version=2.1.235.4e7; cc_entrypoint=cli;"},
			{"type": "text", "text": "You are Claude Code, Anthropic's official CLI for Claude.", "cache_control": {"type": "ephemeral"}}
		],
		"messages": [{"role": "user", "content": [{"type": "text", "text": "hi"}]}],
		"temperature": 0.3,
		"stream": false
	}`
	postMessages(t, gw, map[string]string{"Authorization": "Bearer k"}, body)

	var top struct {
		System []struct {
			Text string `json:"text"`
		} `json:"system"`
		Temperature *float64 `json:"temperature"`
	}
	json.Unmarshal(cap.Body, &top)
	if len(top.System) != 2 {
		t.Fatalf("real CC system must not be rewritten to 3 blocks, got %d", len(top.System))
	}
	if !strings.Contains(top.System[0].Text, "cc_version="+mimic.CLICurrentVersion+".") {
		t.Fatalf("billing version not synced: %q", top.System[0].Text)
	}
	if top.Temperature == nil || *top.Temperature != 0.3 {
		t.Fatal("real CC params must pass through")
	}
}

func TestForwardModelMapAndRestore(t *testing.T) {
	cap := &upstreamCapture{}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cap.Headers = r.Header.Clone()
		cap.Body, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)
		io.WriteString(w, "data: {\"model\":\"glm-5.2\",\"text\":\"x\"}\n\n") //nolint:errcheck
		flusher.Flush()
		io.WriteString(w, "data: [DONE]\n\n") //nolint:errcheck
		flusher.Flush()
	}))
	t.Cleanup(upstream.Close)

	cfg := &config.Config{
		UpstreamBaseURL:    upstream.URL,
		UpstreamAuthScheme: "bearer",
		UpstreamPath:       "/v1/messages?beta=true",
		MaxBodyBytes:       1 << 20,
		ModelMap:           map[string]string{"*": "glm-5.2"},
	}
	ident, _ := mimic.NewIdentityStore("")
	f := New(cfg, ident)
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/messages", f.HandleMessages(cfg.UpstreamPath, nil))
	gw := httptest.NewServer(mux)
	t.Cleanup(gw.Close)

	resp := postMessages(t, gw.URL, map[string]string{"Authorization": "Bearer k"},
		`{"model":"claude-sonnet-4-5","messages":[{"role":"user","content":"hi"}],"stream":true}`)

	// 上游收到映射后的模型名。
	var top struct {
		Model string `json:"model"`
	}
	json.Unmarshal(cap.Body, &top)
	if top.Model != "glm-5.2" {
		t.Fatalf("upstream model = %s, want glm-5.2", top.Model)
	}
	// 响应还原为请求名。
	b, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(b), `"model":"claude-sonnet-4-5"`) || strings.Contains(string(b), `"model":"glm-5.2"`) {
		t.Fatalf("model not restored in response: %s", b)
	}
}

func TestForwardSessionPoolAndBudget(t *testing.T) {
	cap := &upstreamCapture{}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cap.Body, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"m","usage":{"input_tokens":10,"output_tokens":5}}`) //nolint:errcheck
	}))
	t.Cleanup(upstream.Close)

	tmp := t.TempDir()
	cfg := &config.Config{
		IdentityFile:       filepath.Join(tmp, "identity.json"),
		UpstreamBaseURL:    upstream.URL,
		UpstreamAuthScheme: "bearer",
		UpstreamPath:       "/v1/messages?beta=true",
		MaxBodyBytes:       1 << 20,
		Behavior: config.Behavior{
			MaxConcurrency:      2,
			QueueTimeoutSeconds: 5,
			SessionPoolSize:     2,
			DailyTokenBudget:    20,
		},
	}
	ident, _ := mimic.NewIdentityStore("")
	f := New(cfg, ident)
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/messages", f.HandleMessages(cfg.UpstreamPath, nil))
	gw := httptest.NewServer(mux)
	t.Cleanup(gw.Close)

	body := `{"model":"m","metadata":{"user_id":"{\"device_id\":\"x\",\"account_uuid\":\"\",\"session_id\":\"11111111-2222-3333-4444-555555555555\"}"},"messages":[{"role":"user","content":"hi"}],"stream":false}`

	// 同一下游会话(锚点)→ 池分配的 session 跨请求稳定。
	sess := make([]string, 0, 2)
	for i := 0; i < 2; i++ {
		resp := postMessages(t, gw.URL, map[string]string{"Authorization": "Bearer k"}, body)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d", resp.StatusCode)
		}
		var top struct {
			Metadata struct {
				UserID string `json:"user_id"`
			} `json:"metadata"`
		}
		json.Unmarshal(cap.Body, &top)
		parsed := mimic.ParseMetadataUserID(top.Metadata.UserID)
		if parsed == nil {
			t.Fatal("user_id missing")
		}
		sess = append(sess, parsed.SessionID)
	}
	if sess[0] == "11111111-2222-3333-4444-555555555555" {
		t.Fatal("downstream session leaked (pool not applied)")
	}
	if sess[0] != sess[1] {
		t.Fatalf("pool session not stable: %s vs %s", sess[0], sess[1])
	}

	// 用量累计 2 次 × 15 token = 30 >= 预算 20 → 第 3 次被拒。
	resp := postMessages(t, gw.URL, map[string]string{"Authorization": "Bearer k"}, body)
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("budget-exceeded status = %d, want 429", resp.StatusCode)
	}

	stats := f.Stats()
	if len(stats) != 1 || stats[0].TokensToday != 30 || stats[0].RequestsToday != 2 || stats[0].SessionsActive == 0 {
		t.Fatalf("stats wrong: %+v", stats)
	}
}

func TestPruneDumps(t *testing.T) {
	dir := t.TempDir()
	old := filepath.Join(dir, "20260101-000000.000000000_downstream.json")
	newer := filepath.Join(dir, "29990101-000000.000000000_downstream.json")
	os.WriteFile(old, []byte("x"), 0o600)   //nolint:errcheck
	os.WriteFile(newer, []byte("y"), 0o600) //nolint:errcheck
	// 把 old 的修改时间拨到 100 小时前。
	past := time.Now().Add(-100 * time.Hour)
	if err := os.Chtimes(old, past, past); err != nil {
		t.Fatal(err)
	}

	f := &Forwarder{cfg: &config.Config{DumpDir: dir, DumpRetentionHours: 72}}
	f.pruneDumps()

	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Fatal("expired dump not pruned")
	}
	if _, err := os.Stat(newer); err != nil {
		t.Fatal("fresh dump must survive")
	}
}

// 用户提供的真实 1308 错误体(窗口上限,消息自带重置时间)。
const body429WindowLimit = `{"error":{"code":"1308","message":"[1308][已达到 5 小时的使用上限。您的限额将在 2999-12-31 23:59:59 重置。][x]","type":"rate_limit_error"},"request_id":"x","type":"error"}`

func TestForwardWindowLimitFreeze(t *testing.T) {
	var upstreamHits int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			// 推理请求:返回 1308 窗口上限。
			atomic.AddInt32(&upstreamHits, 1)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusTooManyRequests)
			w.Write([]byte(body429WindowLimit)) //nolint:errcheck
			return
		}
		// 额度探测(GET):返回带 5h 档的 quota。
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"success":true,"data":{"level":"PRO","limits":[{"type":"TOKENS_LIMIT","unit":3,"percentage":30}]}}`)) //nolint:errcheck
	}))
	t.Cleanup(upstream.Close)

	cfg := &config.Config{
		IdentityFile:       filepath.Join(t.TempDir(), "identity.json"),
		UpstreamBaseURL:    upstream.URL,
		UpstreamAuthScheme: "bearer",
		UpstreamPath:       "/v1/messages?beta=true",
		MaxBodyBytes:       1 << 20,
		MaxUpstreamRetries: 2,
	}
	ident, _ := mimic.NewIdentityStore("")
	f := New(cfg, ident)
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/messages", f.HandleMessages(cfg.UpstreamPath, nil))
	mux.HandleFunc("GET /quota", func(w http.ResponseWriter, r *http.Request) {
		apiKey := ExtractAPIKey(r)
		if apiKey == "" {
			http.Error(w, "missing key", http.StatusUnauthorized)
			return
		}
		refresh := r.URL.Query().Get("refresh") == "true" || r.URL.Query().Get("refresh") == "1"
		result, err := f.Quota(r.Context(), mimic.KeyIdentityHash(apiKey), apiKey, refresh)
		if err != nil {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(result) //nolint:errcheck
	})
	gw := httptest.NewServer(mux)
	t.Cleanup(gw.Close)

	body := `{"model":"m","messages":[{"role":"user","content":"hi"}],"stream":false}`

	// 第一击:上游返回 1308 → 冻结设置 + 原样透传 + Retry-After,不重试。
	resp := postMessages(t, gw.URL, map[string]string{"Authorization": "Bearer k"}, body)
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	b, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(b), "1308") {
		t.Fatalf("original error body must pass through: %s", b)
	}
	if ra := resp.Header.Get("Retry-After"); ra == "" {
		t.Fatal("Retry-After missing")
	}
	if n := atomic.LoadInt32(&upstreamHits); n != 1 {
		t.Fatalf("window-limit 429 must not retry, upstream hits = %d", n)
	}

	// 第二击:冻结期内直接 429,不再发上游。
	resp2 := postMessages(t, gw.URL, map[string]string{"Authorization": "Bearer k"}, body)
	if resp2.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("frozen status = %d", resp2.StatusCode)
	}
	b2, _ := io.ReadAll(resp2.Body)
	if !strings.Contains(string(b2), "cooldown active") || resp2.Header.Get("Retry-After") == "" {
		t.Fatalf("frozen response wrong: %s", b2)
	}
	if n := atomic.LoadInt32(&upstreamHits); n != 1 {
		t.Fatalf("frozen request must not reach upstream, hits = %d", n)
	}

	// /quota?refresh=true 解冻。
	req, _ := http.NewRequest(http.MethodGet, gw.URL+"/quota?refresh=true", nil)
	req.Header.Set("Authorization", "Bearer k")
	qresp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	qb, _ := io.ReadAll(qresp.Body)
	qresp.Body.Close()
	if qresp.StatusCode != http.StatusOK || !strings.Contains(string(qb), "PRO") || !strings.Contains(string(qb), "5h") {
		t.Fatalf("quota response = %d %s", qresp.StatusCode, qb)
	}

	// 解冻后第三击:恢复转发(重新打到上游)。
	resp3 := postMessages(t, gw.URL, map[string]string{"Authorization": "Bearer k"}, body)
	if resp3.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("post-refresh status = %d", resp3.StatusCode)
	}
	if n := atomic.LoadInt32(&upstreamHits); n != 2 {
		t.Fatalf("refresh must clear freeze, hits = %d", n)
	}
}

func TestQuotaAllWithoutKey(t *testing.T) {
	var quotaHits int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			atomic.AddInt32(&quotaHits, 1)
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"success":true,"data":{"level":"PRO","limits":[{"type":"TOKENS_LIMIT","unit":3,"percentage":30}]}}`)) //nolint:errcheck
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(upstream.Close)

	cfg := &config.Config{
		IdentityFile:       filepath.Join(t.TempDir(), "identity.json"),
		UpstreamBaseURL:    upstream.URL,
		UpstreamAuthScheme: "bearer",
		UpstreamPath:       "/v1/messages?beta=true",
		MaxBodyBytes:       1 << 20,
	}
	ident, _ := mimic.NewIdentityStore("")
	f := New(cfg, ident)
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/messages", f.HandleMessages(cfg.UpstreamPath, nil))
	gw := httptest.NewServer(mux)
	t.Cleanup(gw.Close)

	// 无 key 列表模式:KeyRing 为空 → 空列表。
	entries := f.QuotaAll(context.Background(), false)
	if len(entries) != 0 {
		t.Fatalf("empty ring must return empty list, got %d", len(entries))
	}

	// 经过一次请求后,KeyRing 记录该 key → 列表可反查(不回显原文)。
	postMessages(t, gw.URL, map[string]string{"Authorization": "Bearer sk-abc"},
		`{"model":"m","messages":[{"role":"user","content":"hi"}],"stream":false}`)
	entries = f.QuotaAll(context.Background(), false)
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entries))
	}
	wantPrefix := mimic.KeyIdentityHash("sk-abc")[:8]
	if entries[0].Key != wantPrefix || len(entries[0].Key) != 8 || strings.Contains(fmt.Sprint(entries[0]), "sk-abc") {
		t.Fatalf("entry must expose 8-char hash prefix only: %+v", entries[0])
	}
	if entries[0].Quota == nil || entries[0].Quota.PlanLevel != "PRO" {
		t.Fatalf("quota not probed: %+v", entries[0])
	}
	if atomic.LoadInt32(&quotaHits) != 1 {
		t.Fatalf("quota hits = %d(缓存/列表探测异常)", quotaHits)
	}
	// 再次列表(30s 缓存):不打上游。
	f.QuotaAll(context.Background(), false)
	if atomic.LoadInt32(&quotaHits) != 1 {
		t.Fatalf("cache miss in list mode, hits = %d", quotaHits)
	}
}
