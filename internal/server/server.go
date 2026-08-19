// Package server 组装 HTTP 路由。
package server

import (
	"encoding/json"
	"log/slog"
	"net/http"

	"glm-cc-gateway/internal/config"
	"glm-cc-gateway/internal/mimic"
	"glm-cc-gateway/internal/proxy"
)

// New 构建网关 http.Handler。
func New(cfg *config.Config, ident *mimic.IdentityStore) http.Handler {
	f := proxy.New(cfg, ident)
	mux := http.NewServeMux()

	// 标准路径(客户端 baseURL 指到网关根,如 ANTHROPIC_BASE_URL=http://host:port)
	mux.HandleFunc("POST /v1/messages", f.HandleMessages(cfg.UpstreamPath, nil))
	mux.HandleFunc("POST /v1/messages/count_tokens",
		f.HandleMessages(cfg.CountTokensPath, []string{mimic.BetaTokenCounting}))

	// 别名:无 /v1 前缀(客户端 baseURL 指到 /v1 层,如 opencode/@ai-sdk/anthropic
	// 的 baseURL 默认值为 https://api.anthropic.com/v1,请求时拼 /messages)
	mux.HandleFunc("POST /messages", f.HandleMessages(cfg.UpstreamPath, nil))
	mux.HandleFunc("POST /messages/count_tokens",
		f.HandleMessages(cfg.CountTokensPath, []string{mimic.BetaTokenCounting}))

	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"status":"ok"}`)) //nolint:errcheck
	})

	// 行为画像:核对聚合行为是否仍在"单个真人"包线内。
	mux.HandleFunc("GET /stats", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"keys": f.Stats()}) //nolint:errcheck
	})

	// 兜底:打印未匹配的 method+path,方便排查客户端路径拼错类问题。
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		slog.Warn("no route", "method", r.Method, "path", r.URL.Path)
		http.NotFound(w, r)
	})

	return mux
}
