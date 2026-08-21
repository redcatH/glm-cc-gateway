package guard

import (
	"context"
	"log/slog"
	"net/http"
	"sync"
	"time"
)

// MaxCooldown 是单次冻结的时长上限:解析出的重置时刻若距今超过 24h,
// 按 24h 截断。防解析异常长冻;周上限场景每 24h 放行一次探针请求,
// 429 后重新学得精确重置时间,自动修正。用户也可经 /quota?refresh 提前解除。
const MaxCooldown = 24 * time.Hour

// RateLimitCooldown 按"下游身份键"记录窗口上限冻结(内存态;重启后由
// 下一次 429 重新学得,自愈)。写入路径:上游 429 消息解析;失效路径:
// 到期自动 / 用户 refresh 手动清除 / 校准修正。
type RateLimitCooldown struct {
	mu    sync.Mutex
	until map[string]time.Time
	now   func() time.Time
}

// NewRateLimitCooldown 创建冻结存储。now 可注入测试时钟。
func NewRateLimitCooldown() *RateLimitCooldown {
	return &RateLimitCooldown{until: map[string]time.Time{}, now: time.Now}
}

// Get 返回该 key 的冻结截止时刻(可能已过期,由调用方判断)。
func (c *RateLimitCooldown) Get(key string) (time.Time, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	t, ok := c.until[key]
	return t, ok
}

// Set 设置冻结截止(应用 24h 上限)。
func (c *RateLimitCooldown) Set(key string, until time.Time) {
	if d := until.Sub(c.now()); d > MaxCooldown {
		until = c.now().Add(MaxCooldown)
	}
	c.mu.Lock()
	c.until[key] = until
	c.mu.Unlock()
}

// Clear 清除冻结(refresh / 到期后)。
func (c *RateLimitCooldown) Clear(key string) {
	c.mu.Lock()
	delete(c.until, key)
	c.mu.Unlock()
}

// quotaCacheEntry 是额度探测的缓存条目。
type quotaCacheEntry struct {
	result    *QuotaResult
	err       error
	fetchedAt time.Time
}

// QuotaService 提供带缓存的额度查询与冻结联动。
// 探测策略为事件驱动(手动 /quota、429 校准、恢复点确认),不做周期轮询。
type QuotaService struct {
	client   *http.Client
	baseURL  string
	cooldown *RateLimitCooldown
	now      func() time.Time

	mu          sync.Mutex
	cache       map[string]*quotaCacheEntry
	lastRefresh map[string]time.Time // refresh 3 秒合并窗口
}

// NewQuotaService 创建额度服务。
func NewQuotaService(client *http.Client, upstreamBaseURL string, cooldown *RateLimitCooldown) *QuotaService {
	return &QuotaService{
		client:      client,
		baseURL:     upstreamBaseURL,
		cooldown:    cooldown,
		now:         time.Now,
		cache:       map[string]*quotaCacheEntry{},
		lastRefresh: map[string]time.Time{},
	}
}

const (
	quotaCacheTTL       = 30 * time.Second
	quotaRefreshWindow  = 3 * time.Second
	quotaCalibrateDelta = 2 * time.Minute
)

// Get 返回该 key 的额度。refresh=false 走缓存(TTL 内不打上游);
// refresh=true 绕过缓存实时探测,并清除该 key 的冻结(套餐升级/额度已刷新
// 场景)——清冻结不做条件判断:若额度未回,下次请求 429 会重新学得,代价一次。
// refresh 自身有 3 秒合并窗口,防脚本狂刷额度端点。
func (s *QuotaService) Get(ctx context.Context, identityKey, apiKey string, refresh bool) (*QuotaResult, error) {
	if !refresh {
		s.mu.Lock()
		entry, ok := s.cache[identityKey]
		s.mu.Unlock()
		if ok && s.now().Sub(entry.fetchedAt) < quotaCacheTTL {
			return entry.result, entry.err
		}
	} else {
		s.mu.Lock()
		if last, ok := s.lastRefresh[identityKey]; ok && s.now().Sub(last) < quotaRefreshWindow {
			entry := s.cache[identityKey]
			s.mu.Unlock()
			if entry != nil {
				return entry.result, entry.err
			}
		} else {
			s.lastRefresh[identityKey] = s.now()
			s.mu.Unlock()
		}
		s.cooldown.Clear(identityKey)
	}

	result, err := ProbeZhipuQuota(ctx, s.client, s.baseURL, apiKey)
	s.mu.Lock()
	s.cache[identityKey] = &quotaCacheEntry{result: result, err: err, fetchedAt: s.now()}
	s.mu.Unlock()
	return result, err
}

// CalibrateAsync 在 429 冻结建立后后台校准冻结时刻:用 quota 端点的
// nextResetTime(绝对毫秒时间戳)对照消息解析出的(假设北京时间的)时刻,
// 偏差超过阈值时以时间戳为准修正。探测失败静默保留原冻结。
func (s *QuotaService) CalibrateAsync(identityKey, apiKey string, parsedUntil time.Time) {
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), quotaTimeout+5*time.Second)
		defer cancel()
		result, err := ProbeZhipuQuota(ctx, s.client, s.baseURL, apiKey)
		if err != nil {
			slog.Debug("quota calibrate probe failed", "key", identityKey[:8], "err", err)
			return
		}
		s.mu.Lock()
		s.cache[identityKey] = &quotaCacheEntry{result: result, fetchedAt: s.now()}
		s.mu.Unlock()

		// 找与解析时刻最接近的 tier reset 做对照。
		var best time.Time
		for _, tier := range result.Tiers {
			if t, err := time.Parse(time.RFC3339, tier.ResetAt); err == nil {
				if best.IsZero() || absDuration(t.Sub(parsedUntil)) < absDuration(best.Sub(parsedUntil)) {
					best = t
				}
			}
		}
		if best.IsZero() {
			return
		}
		if d := best.Sub(parsedUntil); d > quotaCalibrateDelta || d < -quotaCalibrateDelta {
			s.cooldown.Set(identityKey, best)
			slog.Info("cooldown calibrated by quota timestamp",
				"key", identityKey[:8], "delta", best.Sub(parsedUntil).Round(time.Second))
		}
	}()
}

// RecoverAsync 在冻结到期后的首个放行请求时后台刷新额度缓存
// (确认额度已回,顺带给 /quota 提供新鲜数据)。不清冻结(已到期清除)。
func (s *QuotaService) RecoverAsync(identityKey, apiKey string) {
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), quotaTimeout+5*time.Second)
		defer cancel()
		result, err := ProbeZhipuQuota(ctx, s.client, s.baseURL, apiKey)
		if err != nil {
			return
		}
		s.mu.Lock()
		s.cache[identityKey] = &quotaCacheEntry{result: result, fetchedAt: s.now()}
		s.mu.Unlock()
	}()
}

func absDuration(d time.Duration) time.Duration {
	if d < 0 {
		return -d
	}
	return d
}
