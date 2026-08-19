package guard

import (
	"encoding/json"
	"hash/fnv"
	"math/rand"
	"os"
	"path/filepath"
	"sync"
	"time"

	"glm-cc-gateway/internal/mimic"
)

// poolSlot 是会话池的一个槽位:一个对上游可见的 session_id 及其寿命。
type poolSlot struct {
	SessionID string    `json:"session_id"`
	ExpiresAt time.Time `json:"expires_at"`
}

// SessionPool 为每个 key 维护一个固定大小的 session_id 池:
// 上游同一时刻最多看到 size 个活跃会话(贴近真人"同时开几个终端"),
// 池内槽位按随机寿命轮换(真人隔几小时开新会话)。
// 下游会话通过 slotSeed 稳定哈希到某个槽位,跨请求保持不变。
type SessionPool struct {
	mu      sync.Mutex
	path    string
	size    int
	minLife time.Duration
	maxLife time.Duration
	pools   map[string][]*poolSlot
}

// NewSessionPool 创建会话池。size<=0 时 Assign 直接退化返回空(不启用)。
// path 非空时状态落盘(重启不换 session)。寿命参数非法(<=0 或 max<=min)时
// 用安全默认(min 1h,max 3h)——寿命 0 会导致每次请求都触发轮换。
func NewSessionPool(path string, size int, minLife, maxLife time.Duration) *SessionPool {
	if minLife <= 0 {
		minLife = time.Hour
	}
	if maxLife <= minLife {
		maxLife = 3 * minLife
	}
	p := &SessionPool{
		path:    path,
		size:    size,
		minLife: minLife,
		maxLife: maxLife,
		pools:   map[string][]*poolSlot{},
	}
	if path != "" {
		if data, err := os.ReadFile(path); err == nil {
			_ = json.Unmarshal(data, &p.pools)
		}
	}
	return p
}

// Assign 返回 slotSeed 对应的 session_id;槽位过期时轮换为新 UUID。
// 池未启用(size<=0)返回空字符串,调用方回退到默认派生。
func (p *SessionPool) Assign(identityKey, slotSeed string) string {
	if p.size <= 0 {
		return ""
	}
	p.mu.Lock()
	defer p.mu.Unlock()

	slots := p.pools[identityKey]
	if len(slots) != p.size {
		slots = make([]*poolSlot, p.size)
		p.pools[identityKey] = slots
	}

	h := fnv.New32a()
	h.Write([]byte(slotSeed))
	idx := int(h.Sum32() % uint32(p.size))

	now := time.Now()
	s := slots[idx]
	if s == nil || now.After(s.ExpiresAt) {
		life := p.minLife
		if p.maxLife > p.minLife {
			// 随机寿命在 [min,max):固定周期本身就是机器特征。
			life += time.Duration(rand.Int63n(int64(p.maxLife - p.minLife)))
		}
		s = &poolSlot{SessionID: mimic.NewRandomUUID(), ExpiresAt: now.Add(life)}
		slots[idx] = s
		p.persistLocked()
	}
	return s.SessionID
}

// ActiveSessions 返回该 key 池内未过期的槽数(供 /stats)。
func (p *SessionPool) ActiveSessions(identityKey string) int {
	if p.size <= 0 {
		return 0
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	slots := p.pools[identityKey]
	now := time.Now()
	n := 0
	for _, s := range slots {
		if s != nil && now.Before(s.ExpiresAt) {
			n++
		}
	}
	return n
}

func (p *SessionPool) persistLocked() {
	if p.path == "" {
		return
	}
	data, err := json.Marshal(p.pools)
	if err != nil {
		return
	}
	_ = os.MkdirAll(filepath.Dir(p.path), 0o700)
	tmp := p.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err == nil {
		_ = os.Rename(tmp, p.path)
	}
}

// EachKey 遍已有池的 key(供 /stats 聚合)。
func (p *SessionPool) EachKey(fn func(key string)) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for k := range p.pools {
		fn(k)
	}
}
