package guard

import "sync"

// KeyRing 在内存中记录"身份键 → key 原文"映射,供 /quota 的免 key 列表模式
// 反查原文去上游探测。安全约束:**原文只存内存、永不落盘**(重启后由各 key
// 的首个请求重新记录);对外展示只使用身份键前缀,不回显原文。
type KeyRing struct {
	mu   sync.RWMutex
	keys map[string]string
}

// NewKeyRing 创建空环。
func NewKeyRing() *KeyRing {
	return &KeyRing{keys: map[string]string{}}
}

// Record 记录(或刷新)某身份键对应的 key 原文,每个请求调用。
func (k *KeyRing) Record(identityKey, apiKey string) {
	if identityKey == "" || apiKey == "" {
		return
	}
	k.mu.Lock()
	k.keys[identityKey] = apiKey
	k.mu.Unlock()
}

// All 返回全部映射的副本。
func (k *KeyRing) All() map[string]string {
	k.mu.RLock()
	defer k.mu.RUnlock()
	out := make(map[string]string, len(k.keys))
	for id, key := range k.keys {
		out[id] = key
	}
	return out
}
