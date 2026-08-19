package mimic

import (
	"net/http"
	"strconv"
	"strings"
)

// 上游认证方案。
const (
	AuthSchemeBearer  = "bearer"    // authorization: Bearer <key>
	AuthSchemeXAPIKey = "x-api-key" // x-api-key: <key>
)

// HeaderInput 是构建上游请求头所需的全部信息。
type HeaderInput struct {
	APIKey     string   // 下游传来的上游 key(原样转发)
	AuthScheme string   // bearer | x-api-key,空为 bearer
	IsStream   bool     // body 的 stream 标记
	SessionID  string   // 已派生的 session_id(X-Claude-Code-Session-Id 同步用)
	RetryCount int      // 上游重试次数(X-Stainless-Retry-Count,真实 SDK 逐次递增)
	ExtraBetas []string // 追加的 beta(如 count_tokens 的 token-counting),置于集合尾部
}

// AuthHeaderName 返回认证头名(wire 形式)。
func AuthHeaderName(scheme string) string {
	if strings.EqualFold(scheme, AuthSchemeXAPIKey) {
		return "x-api-key"
	}
	return "authorization"
}

// BuildUpstreamHeaders 从零构建发往上游的请求头:丢弃下游全部业务 header,
// 强制注入 Claude Code CLI 指纹(对齐 sub2api applyClaudeCodeMimicHeaders 的
// "无条件覆盖"语义——透传客户端头会引入与伪装值冲突的不一致信号)。
func BuildUpstreamHeaders(in HeaderInput) http.Header {
	h := http.Header{}

	// 1. CLI 默认头全量强制(UA / x-stainless-* / x-app / browser-access),
	//    wire casing 按 sub2api 抓包表还原。
	for key, value := range DefaultHeaders {
		if value == "" {
			continue
		}
		setHeaderRaw(h, resolveWireCasing(key), value)
	}

	// 2. 真实 CLI 用 Accept: application/json(即使流式)。
	setHeaderRaw(h, "Accept", "application/json")

	// 3. 固定基础头。
	setHeaderRaw(h, "content-type", "application/json")
	setHeaderRaw(h, "anthropic-version", "2023-06-01")
	setHeaderRaw(h, "accept-encoding", "gzip, deflate, br, zstd")

	// 4. anthropic-beta:API-key 伪装集合(无 oauth beta)+ 可选追加项。
	betas := APIKeyMimicryBetas()
	betas = append(betas, in.ExtraBetas...)
	setHeaderRaw(h, "anthropic-beta", strings.Join(betas, ","))

	// 5. 认证头:按上游要求的方案注入下游传来的 key。
	switch {
	case strings.EqualFold(in.AuthScheme, AuthSchemeXAPIKey):
		setHeaderRaw(h, "x-api-key", in.APIKey)
	default:
		setHeaderRaw(h, "authorization", "Bearer "+in.APIKey)
	}

	// 6. 每请求随机 UUID(真实 CLI 行为;缺失或重复可能触发第三方判定)。
	setHeaderRaw(h, "x-client-request-id", NewRandomUUID())

	// 7. session 头与 body 内 metadata.session_id 同步(真实 CLI 每请求都带)。
	if in.SessionID != "" {
		setHeaderRaw(h, "X-Claude-Code-Session-Id", in.SessionID)
	}

	// 8. 重试计数(默认 DefaultHeaders 中为 "0",重试时递增)。
	if in.RetryCount > 0 {
		setHeaderRaw(h, "X-Stainless-Retry-Count", strconv.Itoa(in.RetryCount))
	}

	// 9. 流式请求带 helper-method(对齐真实 CLI)。
	if in.IsStream {
		setHeaderRaw(h, "x-stainless-helper-method", "stream")
	}

	return h
}
