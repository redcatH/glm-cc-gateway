// Package mimic 移植自 sub2api (github.com/Wei-Shaw/sub2api) 的 Claude Code 伪装层。
// 常量与算法与 sub2api backend/internal/pkg/claude/constants.go 保持一致,
// 仅在 beta 集合上做了 API-key 场景裁剪(去掉 oauth-2025-04-20)。
package mimic

// CLICurrentVersion 对外伪装的 Claude Code CLI 版本号(三段 semver)。
// 用于 billing attribution block 中的 cc_version=X.Y.Z.{fp} 前缀以及 fingerprint 计算。
// 必须与 DefaultHeaders["User-Agent"] 中的版本号严格一致;不一致会被上游判第三方。
// 来源: sub2api internal/pkg/claude/constants.go CLICurrentVersion。
const CLICurrentVersion = "2.1.220"

// Beta header 常量。与 sub2api 对齐(其来源为真实 Claude Code CLI 抓包 + Parrot)。
const (
	BetaOAuth               = "oauth-2025-04-20" // OAuth 会话专用,API-key 场景不携带
	BetaClaudeCode          = "claude-code-20250219"
	BetaInterleavedThinking = "interleaved-thinking-2025-05-14"
	BetaPromptCachingScope  = "prompt-caching-scope-2026-01-05"
	BetaEffort              = "effort-2025-11-24"
	BetaContextManagement   = "context-management-2025-06-27"
	BetaExtendedCacheTTL    = "extended-cache-ttl-2025-04-11"
	BetaTokenCounting       = "token-counting-2024-11-01"
)

// FullClaudeCodeMimicryBetas 返回最"像"真实 Claude Code CLI 的完整 beta 列表
// (sub2api 原版,OAuth 场景使用)。顺序与真实 CLI 抓包一致。
func FullClaudeCodeMimicryBetas() []string {
	return []string{
		BetaClaudeCode,
		BetaOAuth,
		BetaInterleavedThinking,
		BetaPromptCachingScope,
		BetaEffort,
		BetaContextManagement,
		BetaExtendedCacheTTL,
	}
}

// APIKeyMimicryBetas 是 API-key 场景的伪装 beta 集合:在 sub2api 的
// FullClaudeCodeMimicryBetas 基础上去掉 BetaOAuth —— oauth-2025-04-20 是
// OAuth 会话专用 token,API-key 请求携带它反而构成"不一致"信号。
// (伪装身份:使用 ANTHROPIC_API_KEY 的真实 Claude Code CLI。)
func APIKeyMimicryBetas() []string {
	return []string{
		BetaClaudeCode,
		BetaInterleavedThinking,
		BetaPromptCachingScope,
		BetaEffort,
		BetaContextManagement,
		BetaExtendedCacheTTL,
	}
}

// DefaultHeaders 是 Claude Code 客户端默认请求头。
// 来源: sub2api internal/pkg/claude/constants.go DefaultHeaders(原样移植)。
var DefaultHeaders = map[string]string{
	"User-Agent":                                "claude-cli/" + CLICurrentVersion + " (external, cli)",
	"X-Stainless-Lang":                          "js",
	"X-Stainless-Package-Version":               "0.94.0",
	"X-Stainless-OS":                            "Linux",
	"X-Stainless-Arch":                          "arm64",
	"X-Stainless-Runtime":                       "node",
	"X-Stainless-Runtime-Version":               "v24.3.0",
	"X-Stainless-Retry-Count":                   "0",
	"X-Stainless-Timeout":                       "600",
	"X-App":                                     "cli",
	"Anthropic-Dangerous-Direct-Browser-Access": "true",
}
