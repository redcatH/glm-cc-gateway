// glm-cc-gateway: 把下游 Anthropic 协议请求伪装成真实 Claude Code CLI
// 发往固定上游端点(base_url 固定,key 由下游传入,伪装身份按 key 隔离)。
//
// 伪装层移植自 sub2api(算法与常量一致),详见 internal/mimic。
package main

import (
	"flag"
	"log/slog"
	"net/http"
	"os"

	"glm-cc-gateway/internal/config"
	"glm-cc-gateway/internal/mimic"
	"glm-cc-gateway/internal/server"
)

func main() {
	cfgPath := flag.String("config", "config.json", "配置文件路径")
	flag.Parse()

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		slog.Error("load config", "err", err)
		os.Exit(1)
	}

	ident, err := mimic.NewIdentityStore(cfg.IdentityFile)
	if err != nil {
		slog.Error("load identity store", "path", cfg.IdentityFile, "err", err)
		os.Exit(1)
	}

	handler := server.New(cfg, ident)
	slog.Info("glm-cc-gateway listening",
		"addr", cfg.Listen,
		"upstream", cfg.UpstreamBaseURL,
		"auth_scheme", cfg.UpstreamAuthScheme,
		"dump_dir", cfg.DumpDir,
	)
	if err := http.ListenAndServe(cfg.Listen, handler); err != nil {
		slog.Error("server exited", "err", err)
		os.Exit(1)
	}
}
