package mimic

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
)

// Identity 是某个下游 key 对应的持久伪装身份。
// 模型对齐 sub2api 的"每账号一份指纹"(identity_service.go Fingerprint):
// 同一 key 的所有请求在上游侧始终呈现同一 device_id。
type Identity struct {
	ClientID string `json:"client_id"` // 64-hex,首见时生成,永不更换
}

// IdentityStore 按"身份键"(下游 key 的哈希)持久化 Identity。
// sub2api 用 Redis 按账号 ID 存指纹;这里用本地 JSON 文件实现同样的语义,
// 后续可替换为 Redis 而不影响调用方。
type IdentityStore struct {
	mu      sync.Mutex
	path    string
	entries map[string]Identity
}

// NewIdentityStore 创建存储并从 path 加载已有身份。path 为空表示仅内存态。
func NewIdentityStore(path string) (*IdentityStore, error) {
	s := &IdentityStore{path: path, entries: map[string]Identity{}}
	if path == "" {
		return s, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return s, nil
		}
		return nil, err
	}
	if err := json.Unmarshal(data, &s.entries); err != nil {
		return nil, err
	}
	return s, nil
}

// GetOrCreate 返回身份键对应的身份;首见时生成 ClientID 并落盘。
func (s *IdentityStore) GetOrCreate(identityKey string) (Identity, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if id, ok := s.entries[identityKey]; ok && id.ClientID != "" {
		return id, nil
	}
	id := Identity{ClientID: GenerateClientID()}
	s.entries[identityKey] = id
	if err := s.persistLocked(); err != nil {
		return id, err
	}
	return id, nil
}

// KeyIdentityHash 由下游 API key 派生稳定的身份键(不落盘原文 key)。
func KeyIdentityHash(apiKey string) string {
	sum := sha256.Sum256([]byte("glm-cc-gateway::" + apiKey))
	return hex.EncodeToString(sum[:16])
}

func (s *IdentityStore) persistLocked() error {
	if s.path == "" {
		return nil
	}
	data, err := json.MarshalIndent(s.entries, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}
