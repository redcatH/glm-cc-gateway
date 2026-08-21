package guard

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// 本文件的探测与解析逻辑移植自 sub2api internal/service/cn_provider_quota_service.go
// (智谱 GLM Coding Plan 滚动窗口额度,语义对齐 cc-switch parse_zhipu_token_tiers)。

// QuotaTier 是一个滚动用量窗口档位(5h / weekly)。
type QuotaTier struct {
	Window      string  `json:"window"`       // "5h" | "weekly"
	UsedPercent float64 `json:"used_percent"` // 已用百分比(0-100+)
	ResetAt     string  `json:"reset_at,omitempty"`
}

// QuotaResult 是额度探测的返回结构。
type QuotaResult struct {
	PlanLevel string      `json:"plan_level,omitempty"` // 套餐等级
	Tiers     []QuotaTier `json:"tiers,omitempty"`
}

const (
	quotaTimeout = 15 * time.Second
	quotaMaxBody = 256 * 1024

	zhipuUnit5h   = 3 // unit=3 → 5 小时滚动窗口
	zhipuUnitWeek = 6 // unit=6 → 周窗口
)

// ZhipuQuotaHost 根据上游 base_url 推导额度端点主机
// (与推理域名同站,对齐 sub2api zhipuQuotaHost)。
// 已知 CN 域名之外的显式主机(本地测试/自建中转)直接沿用其 origin。
func ZhipuQuotaHost(baseURL string) string {
	u := strings.ToLower(baseURL)
	switch {
	case strings.Contains(u, "bigmodel.cn"):
		return "https://open.bigmodel.cn"
	case strings.Contains(u, "z.ai"):
		return "https://api.z.ai"
	}
	if parsed, err := url.Parse(strings.TrimSpace(baseURL)); err == nil && parsed.Host != "" {
		return parsed.Scheme + "://" + parsed.Host
	}
	return "https://open.bigmodel.cn"
}

// ProbeZhipuQuota 查询智谱 Coding Plan 滚动窗口用量。
// 认证注意:该端点 Authorization 头直接放 key,不加 Bearer 前缀。
func ProbeZhipuQuota(ctx context.Context, client *http.Client, upstreamBaseURL, apiKey string) (*QuotaResult, error) {
	url := ZhipuQuotaHost(upstreamBaseURL) + "/api/monitor/usage/quota/limit"
	callCtx, cancel := context.WithTimeout(ctx, quotaTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(callCtx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", apiKey)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept-Language", "en-US,en")

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("quota request failed: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, quotaMaxBody))

	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return nil, fmt.Errorf("authentication failed (HTTP %d)", resp.StatusCode)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("API error (HTTP %d): %s", resp.StatusCode, truncateStr(string(body), 240))
	}

	var top struct {
		Success bool            `json:"success"`
		Msg     string          `json:"msg"`
		Data    json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(body, &top); err != nil {
		return nil, fmt.Errorf("decode quota response: %w", err)
	}
	if !top.Success {
		msg := strings.TrimSpace(top.Msg)
		if msg == "" {
			msg = "unknown zhipu quota error"
		}
		return nil, fmt.Errorf("API error: %s", msg)
	}

	result := &QuotaResult{Tiers: parseZhipuTiers(top.Data)}
	var lv struct {
		Level string `json:"level"`
	}
	if json.Unmarshal(top.Data, &lv) == nil {
		result.PlanLevel = strings.TrimSpace(lv.Level)
	}
	return result, nil
}

// parseZhipuTiers 解析 data.limits:
//   - TOKENS_LIMIT 条目按 unit 分类(3=5h,6=weekly;缺失按 reset 升序兜底填空缺槽位)
//   - CREDIT_LIMIT 仅在无任何 TOKENS_LIMIT 时降级展示(度量不同,不与 token 窗口混排)
//   - 单 5h 套餐(仅 1 条 TOKENS_LIMIT)只返回 5h 档,不臆造 weekly
func parseZhipuTiers(data json.RawMessage) []QuotaTier {
	var d struct {
		Limits []struct {
			Type       string          `json:"type"`
			Unit       json.RawMessage `json:"unit"`
			Percentage json.RawMessage `json:"percentage"`
			NextReset  json.RawMessage `json:"nextResetTime"`
		} `json:"limits"`
	}
	if err := json.Unmarshal(data, &d); err != nil || len(d.Limits) == 0 {
		return nil
	}

	var fiveH, weekly *QuotaTier
	var unclassified []QuotaTier
	var creditFallback []QuotaTier
	hasTokens := false

	for _, item := range d.Limits {
		limitType := strings.ToUpper(strings.TrimSpace(item.Type))
		if limitType != "TOKENS_LIMIT" && limitType != "CREDIT_LIMIT" {
			continue
		}
		tier := QuotaTier{
			UsedPercent: rawF64(item.Percentage),
			ResetAt:     rawResetISO(item.NextReset),
		}
		if limitType == "CREDIT_LIMIT" {
			creditFallback = append(creditFallback, tier)
			continue
		}
		hasTokens = true
		switch rawI64(item.Unit) {
		case zhipuUnit5h:
			if fiveH == nil {
				tier.Window = "5h"
				fiveH = &tier
				continue
			}
		case zhipuUnitWeek:
			if weekly == nil {
				tier.Window = "weekly"
				weekly = &tier
				continue
			}
		}
		unclassified = append(unclassified, tier)
	}

	// unit 缺失/未识别:无 reset 的优先归 5h(0% 状态下 5h 桶可能没有 reset),
	// 其余依次填入仍空缺的槽位。
	for _, t := range unclassified {
		switch {
		case t.ResetAt == "" && fiveH == nil:
			t.Window = "5h"
			fiveH = &t
		case fiveH == nil:
			t.Window = "5h"
			fiveH = &t
		case weekly == nil:
			t.Window = "weekly"
			weekly = &t
		}
	}

	var out []QuotaTier
	if fiveH != nil {
		out = append(out, *fiveH)
	}
	if weekly != nil {
		out = append(out, *weekly)
	}
	if !hasTokens && len(out) == 0 {
		out = creditFallback
	}
	return out
}

func rawF64(raw json.RawMessage) float64 {
	var f float64
	_ = json.Unmarshal(raw, &f)
	return f
}

func rawI64(raw json.RawMessage) int64 {
	var i int64
	_ = json.Unmarshal(raw, &i)
	return i
}

func rawResetISO(raw json.RawMessage) string {
	ms := rawI64(raw)
	if ms <= 0 {
		return ""
	}
	return time.UnixMilli(ms).UTC().Format(time.RFC3339)
}

// rawResetTime 与 rawResetISO 相同但返回 time(供冻结校准)。
func rawResetTime(raw json.RawMessage) (time.Time, bool) {
	ms := rawI64(raw)
	if ms <= 0 {
		return time.Time{}, false
	}
	return time.UnixMilli(ms), true
}

func truncateStr(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

// zhipuResetRegex 匹配 429 错误消息中的重置时间,如:
// "[1308][已达到 5 小时的使用上限。您的限额将在 2026-08-14 20:01:13 重置。][...]"
// 窗口无关:不依赖错误码,5h / 周上限消息同样适用。
var zhipuResetRegex = regexp.MustCompile(`(\d{4}-\d{2}-\d{2} \d{2}:\d{2}:\d{2})\s*重置`)

// beijingTZ:智谱错误消息中的时间为北京时间(国内服务面向国内用户的中文文案)。
// 固定时区而非依赖服务器本地时区(容器常为 UTC),避免冻结窗口偏移 8 小时;
// 429 冻结时会用 quota 端点的 nextResetTime(绝对时间戳)校准。
var beijingTZ = time.FixedZone("CST+8", 8*3600)

// ParseZhipuRateLimitReset 从上游 429 错误体中解析窗口重置时间。
// 返回 (重置时刻 UTC, 是否解析成功)。
func ParseZhipuRateLimitReset(body []byte) (time.Time, bool) {
	m := zhipuResetRegex.FindSubmatch(body)
	if m == nil {
		return time.Time{}, false
	}
	t, err := time.ParseInLocation("2006-01-02 15:04:05", string(m[1]), beijingTZ)
	if err != nil {
		return time.Time{}, false
	}
	return t.UTC(), true
}

// FormatBeijing 返回 t 的北京时间字符串(用于冻结响应文案)。
func FormatBeijing(t time.Time) string {
	return t.In(beijingTZ).Format("2006-01-02 15:04:05")
}

// RetryAfterSeconds 计算距 until 的向上取整秒数(至少 1)。
func RetryAfterSeconds(until time.Time) int {
	d := time.Until(until).Seconds()
	if d < 1 {
		return 1
	}
	return int(d + 0.999)
}
