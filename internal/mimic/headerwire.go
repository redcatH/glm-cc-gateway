package mimic

import (
	"net/http"
	"strings"
)

// headerWireCasing 定义每个 header 在真实 Claude CLI 抓包中的准确大小写。
// Go 的 HTTP 层会将 header key 规范化为 Canonical 形式(如 x-app → X-App),
// 此表用于在转发时恢复真实 wire format。
// 来源: sub2api internal/service/header_util.go(原样移植)。
var headerWireCasing = map[string]string{
	// Title case
	"accept":     "Accept",
	"user-agent": "User-Agent",

	// X-Stainless-* 保持 SDK 原始大小写
	"x-stainless-retry-count":     "X-Stainless-Retry-Count",
	"x-stainless-timeout":         "X-Stainless-Timeout",
	"x-stainless-lang":            "X-Stainless-Lang",
	"x-stainless-package-version": "X-Stainless-Package-Version",
	"x-stainless-os":              "X-Stainless-OS",
	"x-stainless-arch":            "X-Stainless-Arch",
	"x-stainless-runtime":         "X-Stainless-Runtime",
	"x-stainless-runtime-version": "X-Stainless-Runtime-Version",
	"x-stainless-helper-method":   "x-stainless-helper-method",

	// Anthropic SDK 自身设置的 header,全小写
	"anthropic-dangerous-direct-browser-access": "anthropic-dangerous-direct-browser-access",
	"anthropic-version":                         "anthropic-version",
	"anthropic-beta":                            "anthropic-beta",
	"x-app":                                     "x-app",
	"content-type":                              "content-type",
	"accept-language":                           "accept-language",
	"sec-fetch-mode":                            "sec-fetch-mode",
	"accept-encoding":                           "accept-encoding",
	"authorization":                             "authorization",

	// Claude Code 2.1.87+ 新增 header
	"x-claude-code-session-id": "X-Claude-Code-Session-Id",
	"x-client-request-id":      "x-client-request-id",
	"content-length":           "content-length",

	// API-key 认证(Anthropic SDK 发送形式,小写)
	"x-api-key": "x-api-key",
}

// headerWireOrder 定义真实 Claude CLI 发送 header 的顺序(基于抓包),
// 用于 dump 输出按此顺序排列,便于与抓包直接对比。
var headerWireOrder = []string{
	"Accept",
	"X-Stainless-Retry-Count",
	"X-Stainless-Timeout",
	"X-Stainless-Lang",
	"X-Stainless-Package-Version",
	"X-Stainless-OS",
	"X-Stainless-Arch",
	"X-Stainless-Runtime",
	"X-Stainless-Runtime-Version",
	"anthropic-dangerous-direct-browser-access",
	"anthropic-version",
	"authorization",
	"x-app",
	"User-Agent",
	"X-Claude-Code-Session-Id",
	"content-type",
	"anthropic-beta",
	"x-client-request-id",
	"accept-language",
	"sec-fetch-mode",
	"accept-encoding",
	"content-length",
	"x-stainless-helper-method",
}

// resolveWireCasing 将 canonical key(如 X-Stainless-Os)映射为真实 wire casing
// (如 X-Stainless-OS)。表中无条目时原样返回。
func resolveWireCasing(key string) string {
	if wk, ok := headerWireCasing[strings.ToLower(key)]; ok {
		return wk
	}
	return key
}

// setHeaderRaw 绕过 Go canonical 化写入 header(保留指定大小写),
// 并先删除同 key 的所有形态避免重复。
// 来源: sub2api internal/service/header_util.go setHeaderRaw。
func setHeaderRaw(h http.Header, key, value string) {
	h.Del(key)
	if wk := resolveWireCasing(key); wk != key {
		delete(h, wk)
	}
	delete(h, key)
	h[key] = []string{value}
}

// getHeaderRaw 读取 header 值,依次尝试:精确 key → wire casing 形态 → canonical 形态。
func getHeaderRaw(h http.Header, key string) string {
	if vals := h[key]; len(vals) > 0 {
		return vals[0]
	}
	if wk := resolveWireCasing(key); wk != key {
		if vals := h[wk]; len(vals) > 0 {
			return vals[0]
		}
	}
	return h.Get(key)
}

// deleteHeaderAllForms 删除某 header 的所有 key 形态(raw/wire/canonical)。
func deleteHeaderAllForms(h http.Header, key string) {
	if h == nil || key == "" {
		return
	}
	h.Del(key)
	delete(h, key)
	if wk := resolveWireCasing(key); wk != key {
		delete(h, wk)
	}
}

// SortHeadersByWireOrder 按真实 Claude CLI 的 header 顺序返回排序后的 key 列表,
// 未在顺序表中的 key 追加到末尾。用于 dump 输出。
func SortHeadersByWireOrder(h http.Header) []string {
	present := make(map[string]string, len(h))
	for k := range h {
		present[strings.ToLower(k)] = k
	}

	result := make([]string, 0, len(h))
	seen := make(map[string]struct{}, len(h))
	for _, wk := range headerWireOrder {
		lk := strings.ToLower(wk)
		if actual, ok := present[lk]; ok {
			if _, dup := seen[lk]; !dup {
				result = append(result, actual)
				seen[lk] = struct{}{}
			}
		}
	}
	for k := range h {
		lk := strings.ToLower(k)
		if _, ok := seen[lk]; !ok {
			result = append(result, k)
			seen[lk] = struct{}{}
		}
	}
	return result
}
