package guard

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// dayUsage 是单个 key 一天的用量累计。
type dayUsage struct {
	Input         int64 `json:"input"`
	Output        int64 `json:"output"`
	CacheRead     int64 `json:"cache_read"`
	CacheCreation int64 `json:"cache_creation"`
	Requests      int64 `json:"requests"`
}

func (d *dayUsage) total() int64 {
	return d.Input + d.Output + d.CacheRead + d.CacheCreation
}

// UsageTracker 按 key、按天(本地日期)累计 token 用量,支持日预算。
type UsageTracker struct {
	mu     sync.Mutex
	path   string
	budget int64
	days   map[string]map[string]*dayUsage // date(2006-01-02) → key → usage
}

// NewUsageTracker 创建用量统计器。path 非空时落盘;budget<=0 表示不限制。
func NewUsageTracker(path string, budget int64) *UsageTracker {
	u := &UsageTracker{path: path, budget: budget, days: map[string]map[string]*dayUsage{}}
	if path != "" {
		if data, err := os.ReadFile(path); err == nil {
			_ = json.Unmarshal(data, &u.days)
		}
	}
	return u
}

func today() string { return time.Now().Format("2006-01-02") }

// CountRequest 计一次请求(无论成败)。
func (u *UsageTracker) CountRequest(key string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.day(key).Requests++
	u.persistLocked()
}

// AddUsage 累计一次响应的 usage。
func (u *UsageTracker) AddUsage(key string, in, out, cacheRead, cacheCreation int64) {
	if in == 0 && out == 0 && cacheRead == 0 && cacheCreation == 0 {
		return
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	d := u.day(key)
	d.Input += in
	d.Output += out
	d.CacheRead += cacheRead
	d.CacheCreation += cacheCreation
	u.persistLocked()
}

// Exceeded 判断该 key 今日 token 是否已超预算(budget<=0 恒 false)。
func (u *UsageTracker) Exceeded(key string) bool {
	if u.budget <= 0 {
		return false
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	dayMap, ok := u.days[today()]
	if !ok {
		return false
	}
	if d, ok := dayMap[key]; ok {
		return d.total() >= u.budget
	}
	return false
}

// KeyStat 是 /stats 暴露的单 key 当日画像。
type KeyStat struct {
	Key          string `json:"key"`
	TokensToday  int64  `json:"tokens_today"`
	RequestsMade int64  `json:"requests_today"`
}

// SnapshotToday 返回今日所有 key 的用量。
func (u *UsageTracker) SnapshotToday() []KeyStat {
	u.mu.Lock()
	defer u.mu.Unlock()
	dayMap, ok := u.days[today()]
	if !ok {
		return nil
	}
	out := make([]KeyStat, 0, len(dayMap))
	for k, d := range dayMap {
		out = append(out, KeyStat{Key: k, TokensToday: d.total(), RequestsMade: d.Requests})
	}
	return out
}

func (u *UsageTracker) day(key string) *dayUsage {
	date := today()
	dayMap, ok := u.days[date]
	if !ok {
		// 顺带清理历史日期(只保留当天)。
		u.days = map[string]map[string]*dayUsage{}
		dayMap = map[string]*dayUsage{}
		u.days[date] = dayMap
	}
	d, ok := dayMap[key]
	if !ok {
		d = &dayUsage{}
		dayMap[key] = d
	}
	return d
}

func (u *UsageTracker) persistLocked() {
	if u.path == "" {
		return
	}
	data, err := json.Marshal(u.days)
	if err != nil {
		return
	}
	_ = os.MkdirAll(filepath.Dir(u.path), 0o700)
	tmp := u.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err == nil {
		_ = os.Rename(tmp, u.path)
	}
}
