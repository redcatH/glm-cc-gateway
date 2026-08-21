package guard

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// 用户提供的真实 1308 错误消息(2026-08-14 抓获)。
const sample429Body = `{
  "error": {
    "code": "1308",
    "message": "[1308][已达到 5 小时的使用上限。您的限额将在 2026-08-14 20:01:13 重置。][20260814154613bf245c239e8c4782]",
    "type": "rate_limit_error"
  },
  "request_id": "20260814154613bf245c239e8c4782",
  "type": "error"
}`

func TestParseZhipuRateLimitReset(t *testing.T) {
	until, ok := ParseZhipuRateLimitReset([]byte(sample429Body))
	if !ok {
		t.Fatal("expected parse success")
	}
	// 北京时间 2026-08-14 20:01:13(+8)→ UTC 12:01:13。
	want := time.Date(2026, 8, 14, 12, 1, 13, 0, time.UTC)
	if !until.Equal(want) {
		t.Fatalf("until = %s, want %s(时区必须按北京时间解析,不依赖本地时区)", until, want)
	}

	// 无"重置"字样 → 解析失败。
	if _, ok := ParseZhipuRateLimitReset([]byte(`{"error":{"code":"429","message":"too many requests"}}`)); ok {
		t.Fatal("plain 429 without reset info must not parse")
	}
}

func TestCooldownSetWithCap(t *testing.T) {
	c := NewRateLimitCooldown()
	now := time.Date(2026, 8, 14, 12, 0, 0, 0, time.UTC)
	c.now = func() time.Time { return now }

	// 普通冻结(4h)< 24h:原样。
	c.Set("k", now.Add(4*time.Hour))
	got, _ := c.Get("k")
	if !got.Equal(now.Add(4 * time.Hour)) {
		t.Fatalf("short freeze altered: %s", got)
	}

	// 超过 24h(周上限):截断为 24h。
	c.Set("k2", now.Add(6*24*time.Hour))
	got2, _ := c.Get("k2")
	if d := got2.Sub(now); d != MaxCooldown {
		t.Fatalf("cap not applied: %v", d)
	}

	// Clear。
	c.Clear("k")
	if _, ok := c.Get("k"); ok {
		t.Fatal("clear failed")
	}
}

func TestQuotaParseTiers(t *testing.T) {
	// 新套餐:5h + weekly 两条 TOKENS_LIMIT。
	data := json.RawMessage(`{"level":"MAX","limits":[
		{"type":"TOKENS_LIMIT","unit":3,"percentage":87.5,"nextResetTime":1755148873000},
		{"type":"TOKENS_LIMIT","unit":6,"percentage":42.0,"nextResetTime":1755667273000}
	]}`)
	tiers := parseZhipuTiers(data)
	if len(tiers) != 2 || tiers[0].Window != "5h" || tiers[0].UsedPercent != 87.5 ||
		tiers[1].Window != "weekly" || tiers[1].UsedPercent != 42.0 {
		t.Fatalf("tiers wrong: %+v", tiers)
	}
	if tiers[0].ResetAt != "2025-08-14T05:21:13Z" {
		t.Fatalf("reset ISO wrong: %s", tiers[0].ResetAt)
	}

	// 单 5h 套餐:仅 1 条,unit 缺失 → 归 5h,不臆造 weekly。
	single := json.RawMessage(`{"limits":[{"type":"TOKENS_LIMIT","percentage":100}]}`)
	tiers2 := parseZhipuTiers(single)
	if len(tiers2) != 1 || tiers2[0].Window != "5h" {
		t.Fatalf("single-5h plan wrong: %+v", tiers2)
	}

	// CREDIT 降级:无 TOKENS_LIMIT 时才用。
	credit := json.RawMessage(`{"limits":[{"type":"CREDIT_LIMIT","percentage":10}]}`)
	tiers3 := parseZhipuTiers(credit)
	if len(tiers3) != 1 {
		t.Fatalf("credit fallback wrong: %+v", tiers3)
	}
	mixed := json.RawMessage(`{"limits":[
		{"type":"TOKENS_LIMIT","unit":3,"percentage":50},
		{"type":"CREDIT_LIMIT","percentage":10}
	]}`)
	if tiers4 := parseZhipuTiers(mixed); len(tiers4) != 1 {
		t.Fatalf("credit must not mix with token windows: %+v", tiers4)
	}
}

func TestQuotaServiceCacheAndRefresh(t *testing.T) {
	var hits int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		if got := r.Header.Get("Authorization"); got != "sk-test" {
			t.Errorf("auth header = %q(必须是裸 key,无 Bearer)", got)
		}
		w.Header().Set("Content-Type", "application/json")
		io_WriteString(w, `{"success":true,"data":{"level":"PRO","limits":[{"type":"TOKENS_LIMIT","unit":3,"percentage":30}]}}`)
	}))
	t.Cleanup(upstream.Close)

	cooldown := NewRateLimitCooldown()
	cooldown.Set("idkey", time.Now().Add(time.Hour)) // 模拟已冻结
	svc := NewQuotaService(upstream.Client(), upstream.URL, cooldown)

	// 第一次:探测。
	if _, err := svc.Get(context.Background(), "idkey", "sk-test", false); err != nil {
		t.Fatal(err)
	}
	// 第二次(缓存):不再打上游。
	svc.Get(context.Background(), "idkey", "sk-test", false) //nolint:errcheck
	if n := atomic.LoadInt32(&hits); n != 1 {
		t.Fatalf("cache miss, upstream hits = %d", n)
	}
	// 冻结仍在(refresh=false 不解冻)。
	if _, ok := cooldown.Get("idkey"); !ok {
		t.Fatal("cooldown must survive non-refresh query")
	}

	// refresh=true:清冻结;3 秒合并窗口内的第二次 refresh 不再打上游。
	if _, err := svc.Get(context.Background(), "idkey", "sk-test", true); err != nil {
		t.Fatal(err)
	}
	if _, ok := cooldown.Get("idkey"); ok {
		t.Fatal("refresh must clear cooldown")
	}
	svc.Get(context.Background(), "idkey", "sk-test", true) //nolint:errcheck
	if n := atomic.LoadInt32(&hits); n != 2 {
		t.Fatalf("refresh merge window failed, hits = %d", n)
	}
}

// io_WriteString 避免 helper 依赖。
func io_WriteString(w http.ResponseWriter, s string) { w.Write([]byte(s)) } //nolint:errcheck
