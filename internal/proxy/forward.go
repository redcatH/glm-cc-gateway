// Package proxy 实现下游请求 → 伪装改写 → 上游转发的核心链路。
package proxy

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"glm-cc-gateway/internal/config"
	"glm-cc-gateway/internal/guard"
	"glm-cc-gateway/internal/mimic"
)

// Forwarder 持有转发所需的依赖。
type Forwarder struct {
	cfg      *config.Config
	ident    *mimic.IdentityStore
	limiter  *guard.Limiter
	pool     *guard.SessionPool
	usage    *guard.UsageTracker
	client   *http.Client
	allowSet map[string]struct{}
}

// New 创建 Forwarder(含行为收敛组件)。
func New(cfg *config.Config, ident *mimic.IdentityStore) *Forwarder {
	allow := make(map[string]struct{}, len(cfg.APIKeysAllow))
	for _, k := range cfg.APIKeysAllow {
		allow[k] = struct{}{}
	}
	b := cfg.Behavior
	dir := filepath.Dir(cfg.IdentityFile)
	return &Forwarder{
		cfg:      cfg,
		ident:    ident,
		limiter:  guard.NewLimiter(b.MaxConcurrency, b.RPMLimit),
		pool:     guard.NewSessionPool(filepath.Join(dir, "sessions.json"), b.SessionPoolSize, time.Duration(b.SessionRotateMinMinutes)*time.Minute, time.Duration(b.SessionRotateMaxMinutes)*time.Minute),
		usage:    guard.NewUsageTracker(filepath.Join(dir, "usage.json"), b.DailyTokenBudget),
		client:   &http.Client{Timeout: 0}, // 流式长连接不设整体超时,由 X-Stainless-Timeout 语义兜底
		allowSet: allow,
	}
}

// KeyStatEntry 是 /stats 暴露的单 key 画像。
type KeyStatEntry struct {
	Key            string `json:"key"`
	ConcurrencyNow int    `json:"concurrency_now"`
	RPMLastMinute  int    `json:"rpm_last_minute"`
	SessionsActive int    `json:"sessions_active"`
	TokensToday    int64  `json:"tokens_today"`
	RequestsToday  int64  `json:"requests_today"`
}

// Stats 汇总各 key 当日行为画像(供 /stats)。
func (f *Forwarder) Stats() []KeyStatEntry {
	seen := map[string]struct{}{}
	var keys []string
	add := func(k string) {
		if _, ok := seen[k]; !ok && k != "" {
			seen[k] = struct{}{}
			keys = append(keys, k)
		}
	}
	for _, s := range f.usage.SnapshotToday() {
		add(s.Key)
	}
	f.pool.EachKey(add)
	f.limiter.EachKey(add)

	out := make([]KeyStatEntry, 0, len(keys))
	for _, k := range keys {
		entry := KeyStatEntry{Key: k}
		entry.ConcurrencyNow = f.limiter.InFlight(k)
		entry.RPMLastMinute = f.limiter.RPMCount(k)
		entry.SessionsActive = f.pool.ActiveSessions(k)
		for _, s := range f.usage.SnapshotToday() {
			if s.Key == k {
				entry.TokensToday = s.TokensToday
				entry.RequestsToday = s.RequestsMade
			}
		}
		out = append(out, entry)
	}
	return out
}

// extractAPIKey 从下游请求头提取上游 key:优先 Authorization: Bearer,其次 x-api-key。
func extractAPIKey(r *http.Request) string {
	auth := r.Header.Get("Authorization")
	if auth != "" {
		if strings.HasPrefix(auth, "Bearer ") {
			return strings.TrimSpace(strings.TrimPrefix(auth, "Bearer "))
		}
		return strings.TrimSpace(auth)
	}
	return strings.TrimSpace(r.Header.Get("x-api-key"))
}

// clientIP 提取下游客户端 IP(仅用于 session 区分因子)。
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// hop-by-hop 头,不透传给下游。
var hopByHopHeaders = []string{
	"Connection", "Keep-Alive", "Proxy-Authenticate", "Proxy-Authorization",
	"TE", "Trailer", "Transfer-Encoding", "Upgrade",
}

// HandleMessages 处理 /v1/messages(及 count_tokens)请求。
func (f *Forwarder) HandleMessages(upstreamPath string, extraBetas []string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		apiKey := extractAPIKey(r)
		if apiKey == "" {
			http.Error(w, `{"type":"error","message":"missing API key: set Authorization: Bearer <key> or x-api-key"}`, http.StatusUnauthorized)
			return
		}
		if len(f.allowSet) > 0 {
			if _, ok := f.allowSet[apiKey]; !ok {
				http.Error(w, `{"type":"error","message":"invalid API key"}`, http.StatusUnauthorized)
				return
			}
		}

		body, err := io.ReadAll(io.LimitReader(r.Body, f.cfg.MaxBodyBytes))
		if err != nil {
			http.Error(w, `{"type":"error","message":"read body failed"}`, http.StatusBadRequest)
			return
		}

		// === 行为收敛:预算 → 排队(并发+RPM)→ 会话池 ===
		identityKey := mimic.KeyIdentityHash(apiKey)

		if f.usage.Exceeded(identityKey) {
			slog.Warn("daily token budget exceeded", "key", identityKey[:8])
			http.Error(w, `{"type":"error","message":"daily token budget exceeded"}`, http.StatusTooManyRequests)
			return
		}
		if err := f.limiter.Acquire(r.Context(), identityKey,
			time.Duration(f.cfg.Behavior.QueueTimeoutSeconds)*time.Second); err != nil {
			slog.Warn("queue gate", "key", identityKey[:8], "err", err)
			http.Error(w, `{"type":"error","message":"gateway busy (queue timeout)"}`, http.StatusTooManyRequests)
			return
		}
		defer f.limiter.Release(identityKey)
		f.usage.CountRequest(identityKey)

		// === 伪装改写:身份 + session + body 形态 ===
		identity, idErr := f.ident.GetOrCreate(identityKey)
		if idErr != nil {
			slog.Warn("persist identity failed", "err", idErr)
		}

		// 会话锚点:下游 metadata.user_id 的 session_id 优先,其次 session 头。
		sessionAnchor := downstreamSessionAnchor(body, r)

		// 会话池启用时:下游会话稳定映射到池内槽位,上游仅见固定数量 session。
		slotSeed := sessionAnchor
		if slotSeed == "" {
			slotSeed = clientIP(r) + ":" + r.UserAgent()
		}
		sessionOverride := f.pool.Assign(identityKey, slotSeed)

		newBody, sessionID := mimic.RewriteMetadataUserID(body, identity, mimic.RewriteInput{
			IdentityKey:     identityKey,
			ClientIP:        clientIP(r),
			UserAgent:       r.UserAgent(),
			SessionAnchor:   sessionAnchor,
			SessionOverride: sessionOverride,
		})

		// 真 CC 直连流量(自带 billing block):只同步版本,不重写 system
		// (对齐 sub2api 的 isClaudeCode 跳过逻辑,保护其缓存前缀);
		// 第三方客户端流量:完整 3-block 重写 + 参数归一化。
		if mimic.IsRealClaudeCodeBody(newBody) {
			newBody = mimic.SyncBillingVersion(newBody)
		} else {
			newBody = mimic.RewriteSystemForNonCC(newBody)
			newBody = mimic.NormalizeParams(newBody)
		}
		newBody = mimic.EnforceCacheControlLimit(newBody)
		newBody, requestedModel, upstreamModel := f.applyModelMap(newBody)

		isStream := bodyFlagStream(newBody)
		downstreamDump := f.dumpRequest("downstream", r, body)
		defer downstreamDump()

		// === 上游转发(带 429/5xx 重试,Retry-Count 递增对齐真实 SDK)===
		targetURL := strings.TrimRight(f.cfg.UpstreamBaseURL, "/") + upstreamPath
		slog.Info("forward", "downstream", r.Method+" "+r.URL.Path, "upstream", targetURL)
		var resp *http.Response
		var lastErr error
		for attempt := 0; ; attempt++ {
			req, buildErr := http.NewRequestWithContext(r.Context(), http.MethodPost, targetURL, bytes.NewReader(newBody))
			if buildErr != nil {
				http.Error(w, `{"type":"error","message":"build upstream request failed"}`, http.StatusBadGateway)
				return
			}
			req.Header = mimic.BuildUpstreamHeaders(mimic.HeaderInput{
				APIKey:     apiKey,
				AuthScheme: f.cfg.UpstreamAuthScheme,
				IsStream:   isStream,
				SessionID:  sessionID,
				RetryCount: attempt,
				ExtraBetas: extraBetas,
			})

			if attempt == 0 {
				// "rewritten" = 下游原始头 + 改写后 body 的对照视图;
				// 真正发出的上游 headers 由下面 "upstream" dump 记录。
				f.dumpRequest("rewritten", r, newBody)
				f.dumpHeaders("upstream", targetURL, req.Header)
			}

			resp, lastErr = f.client.Do(req)
			if lastErr == nil {
				if resp.StatusCode < 500 && resp.StatusCode != http.StatusTooManyRequests {
					break
				}
				lastErr = fmt.Errorf("upstream status %d", resp.StatusCode)
				io.Copy(io.Discard, resp.Body) //nolint:errcheck
				resp.Body.Close()
			}
			if attempt >= f.cfg.MaxUpstreamRetries || r.Context().Err() != nil {
				break
			}
			select {
			case <-time.After(time.Duration(1<<attempt) * 500 * time.Millisecond):
			case <-r.Context().Done():
			}
		}
		if lastErr != nil && resp == nil {
			slog.Error("upstream request failed", "err", lastErr)
			http.Error(w, `{"type":"error","message":"upstream request failed"}`, http.StatusBadGateway)
			return
		}
		if resp == nil {
			http.Error(w, `{"type":"error","message":"upstream unavailable"}`, http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()

		// === 响应透传(SSE 流式 / 非流式)===
		for _, k := range hopByHopHeaders {
			resp.Header.Del(k)
		}
		resp.Header.Del("Content-Length")
		for k, vs := range resp.Header {
			for _, v := range vs {
				w.Header().Add(k, v)
			}
		}
		w.WriteHeader(resp.StatusCode)
		// 模型映射还原:上游响应(含 SSE chunk)中的模型名换回下游请求名。
		var modelFrom, modelTo []byte
		if upstreamModel != "" && requestedModel != upstreamModel {
			modelFrom = []byte(`"model":"` + upstreamModel + `"`)
			modelTo = []byte(`"model":"` + requestedModel + `"`)
		}
		flusher, _ := w.(http.Flusher)

		if isStream {
			// 流式:SSE 事件重组透传(保持事件边界,顺带解析 usage)。
			scanner := &sseEventScanner{}
			var lastIn, lastOut, lastCR, lastCC int64
			buf := make([]byte, 32*1024)
			writeEvent := func(ev []byte) {
				if modelFrom != nil {
					ev = bytes.ReplaceAll(ev, modelFrom, modelTo)
				}
				if _, writeErr := w.Write(ev); writeErr == nil && flusher != nil {
					flusher.Flush()
				}
			}
			for {
				n, readErr := resp.Body.Read(buf)
				if n > 0 {
					for _, ev := range scanner.Feed(buf[:n]) {
						if in, out, cr, cc, ok := extractUsage(ev); ok {
							if in > 0 || cr > 0 || cc > 0 {
								lastIn, lastCR, lastCC = in, cr, cc
							}
							if out > lastOut {
								lastOut = out
							}
						}
						writeEvent(ev)
					}
				}
				if readErr != nil {
					if !errors.Is(readErr, io.EOF) {
						slog.Warn("upstream body read", "err", readErr)
					}
					writeEvent(scanner.Flush())
					f.usage.AddUsage(identityKey, lastIn, lastOut, lastCR, lastCC)
					return
				}
			}
		}

		// 非流式:整体读取,解析 usage 后写回。
		respBody, readErr := io.ReadAll(resp.Body)
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			slog.Warn("upstream body read", "err", readErr)
			return
		}
		if in, out, cr, cc, ok := extractNonStreamUsage(respBody); ok {
			f.usage.AddUsage(identityKey, in, out, cr, cc)
		}
		if modelFrom != nil {
			respBody = bytes.ReplaceAll(respBody, modelFrom, modelTo)
		}
		w.Write(respBody) //nolint:errcheck
	}
}

// applyModelMap 按配置映射 body.model,返回(新body, 请求模型名, 上游模型名)。
// 未配置映射或未命中(且无 "*" 兜底)时原样透传。
func (f *Forwarder) applyModelMap(body []byte) ([]byte, string, string) {
	var top struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(body, &top); err != nil || top.Model == "" || len(f.cfg.ModelMap) == 0 {
		return body, top.Model, top.Model
	}
	requested := top.Model
	mapped, ok := f.cfg.ModelMap[requested]
	if !ok {
		mapped, ok = f.cfg.ModelMap["*"]
	}
	if !ok || mapped == "" || mapped == requested {
		return body, requested, requested
	}
	var topMap map[string]json.RawMessage
	if err := json.Unmarshal(body, &topMap); err != nil {
		return body, requested, requested
	}
	raw, _ := json.Marshal(mapped)
	topMap["model"] = raw
	out, err := json.Marshal(topMap)
	if err != nil {
		return body, requested, requested
	}
	slog.Info("model mapped", "from", requested, "to", mapped)
	return out, requested, mapped
}

// downstreamSessionAnchor 提取下游会话锚点。
func downstreamSessionAnchor(body []byte, r *http.Request) string {
	var top struct {
		Metadata struct {
			UserID string `json:"user_id"`
		} `json:"metadata"`
	}
	if err := json.Unmarshal(body, &top); err == nil && top.Metadata.UserID != "" {
		if parsed := mimic.ParseMetadataUserID(top.Metadata.UserID); parsed != nil && parsed.SessionID != "" {
			return parsed.SessionID
		}
	}
	if sid := strings.TrimSpace(r.Header.Get("X-Claude-Code-Session-Id")); sid != "" {
		return sid
	}
	return ""
}

// bodyFlagStream 读取 body 的 stream 标记。
func bodyFlagStream(body []byte) bool {
	var top struct {
		Stream bool `json:"stream"`
	}
	if err := json.Unmarshal(body, &top); err != nil {
		return false
	}
	return top.Stream
}

// dumpRequest 返回一个"可选落盘"函数;dump 关闭时为 no-op。
func (f *Forwarder) dumpRequest(tag string, r *http.Request, body []byte) func() {
	if f.cfg.DumpDir == "" {
		return func() {}
	}
	ts := time.Now().Format("20060102-150405.000000000")
	meta := map[string]any{
		"tag":     tag,
		"method":  r.Method,
		"path":    r.URL.Path,
		"headers": orderedHeaders(r.Header),
	}
	writeDump(f.cfg.DumpDir, tag, ts, meta, body)
	f.pruneDumps()
	return func() {} // 预留:响应侧落盘
}

// dumpHeaders 把发往上游的最终 header 按 wire order 落盘。
func (f *Forwarder) dumpHeaders(tag, url string, h http.Header) {
	if f.cfg.DumpDir == "" {
		return
	}
	ts := time.Now().Format("20060102-150405.000000000")
	meta := map[string]any{
		"tag":     tag,
		"url":     url,
		"headers": orderedHeaders(h),
	}
	writeDump(f.cfg.DumpDir, tag, ts, meta, nil)
}

func orderedHeaders(h http.Header) []map[string]string {
	out := make([]map[string]string, 0, len(h))
	for _, k := range mimic.SortHeadersByWireOrder(h) {
		// 直接按 wire 形态 key 取值:http.Header.Get 走 canonical 化查找,
		// 与 setHeaderRaw 写入的 wire 形态(如 "anthropic-version")不匹配,会取到空值。
		val := ""
		if vals := h[k]; len(vals) > 0 {
			val = vals[0]
		}
		out = append(out, map[string]string{"key": k, "value": val})
	}
	return out
}

func writeDump(dir, tag, ts string, meta map[string]any, body []byte) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		slog.Warn("dump mkdir", "err", err)
		return
	}
	name := filepath.Join(dir, fmt.Sprintf("%s_%s.json", ts, tag))
	entry := meta
	entry["time"] = ts
	if body != nil {
		entry["body_len"] = len(body)
		entry["body"] = string(body)
	}
	data, err := json.MarshalIndent(entry, "", "  ")
	if err != nil {
		return
	}
	if err := os.WriteFile(name, data, 0o600); err != nil {
		slog.Warn("dump write", "err", err)
	}
}

// pruneDumps 清理超过保留时长(dump_retention_hours)的 dump 文件。
// 每个请求触发一次;目录内文件数有限,readdir 开销可忽略。
func (f *Forwarder) pruneDumps() {
	retention := time.Duration(f.cfg.DumpRetentionHours) * time.Hour
	if retention <= 0 || f.cfg.DumpDir == "" {
		return
	}
	entries, err := os.ReadDir(f.cfg.DumpDir)
	if err != nil {
		return
	}
	cutoff := time.Now().Add(-retention)
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		info, err := e.Info()
		if err != nil || info.ModTime().After(cutoff) {
			continue
		}
		_ = os.Remove(filepath.Join(f.cfg.DumpDir, e.Name()))
	}
}
