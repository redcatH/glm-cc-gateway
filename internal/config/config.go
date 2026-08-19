// Package config 加载网关配置(JSON 文件)。
package config

import (
	"encoding/json"
	"fmt"
	"os"
)

// Config 是网关的全部配置。
type Config struct {
	// Listen 是网关监听地址,如 "127.0.0.1:8080"。
	Listen string `json:"listen"`

	// UpstreamBaseURL 是固定的上游端点(不含路径),如 "https://open.bigmodel.cn/api/anthropic"。
	UpstreamBaseURL string `json:"upstream_base_url"`

	// UpstreamAuthScheme 是发往上游的认证方案:"bearer"(默认)或 "x-api-key"。
	UpstreamAuthScheme string `json:"upstream_auth_scheme"`

	// UpstreamPath 是 /v1/messages 的上游路径(含查询串)。
	UpstreamPath string `json:"upstream_path"`

	// CountTokensPath 是 /v1/messages/count_tokens 的上游路径(含查询串)。
	CountTokensPath string `json:"count_tokens_path"`

	// IdentityFile 是按 key 持久化的伪装身份存储文件。
	IdentityFile string `json:"identity_file"`

	// DumpDir 非空时,把下游原始请求与上游改写后请求落盘到该目录,便于 diff 验证。
	// 置空(默认)= 关闭。dump 含对话明文与(脱敏后的)认证头,仅调试时开启。
	DumpDir string `json:"dump_dir"`

	// DumpRetentionHours 是 dump 文件自动清理的保留时长(小时);0 = 不清理。
	DumpRetentionHours int `json:"dump_retention_hours"`

	// APIKeysAllow 是可选的下游 key 白名单(原文匹配);为空表示允许任意 key。
	APIKeysAllow []string `json:"api_keys_allow"`

	// ModelMap 是请求模型名映射(在 body 层替换):精确名 → 上游模型名;
	// 特殊键 "*" 作为兜底(未命中精确名的模型全部映射到它)。
	// 为空表示不映射(透传)。响应中的上游模型名会被还原为请求名。
	ModelMap map[string]string `json:"model_map"`

	// MaxBodyBytes 是下游请求体上限(默认 32MB)。
	MaxBodyBytes int64 `json:"max_body_bytes"`

	// MaxUpstreamRetries 是上游 429/5xx 的重试次数上限(默认 2)。
	MaxUpstreamRetries int `json:"max_upstream_retries"`

	// Behavior 是行为收敛配置(把上游可见的并发/会话/用量收敛到"单个真人"包线)。
	// 各项设为 0 表示关闭对应限制。
	Behavior Behavior `json:"behavior"`
}

// Behavior 汇聚行为收敛参数(全部可配,0=关闭)。
type Behavior struct {
	// MaxConcurrency 是每 key 同时发往上游的最大并发,超出排队(默认 2)。
	MaxConcurrency int `json:"max_concurrency"`

	// RPMLimit 是每 key 每分钟请求数上限,超出排队(默认 60)。
	RPMLimit int `json:"rpm_limit"`

	// QueueTimeoutSeconds 是排队等待上限,超时返回 429(默认 120)。
	QueueTimeoutSeconds int `json:"queue_timeout_seconds"`

	// SessionPoolSize 是每 key 的会话池大小:上游同时可见的 session_id 数
	// (默认 3)。下游会话稳定映射到池内槽位,槽位按随机寿命轮换。
	SessionPoolSize int `json:"session_pool_size"`

	// SessionRotateMinMinutes / MaxMinutes 是池内单个 session 的寿命区间
	// (分钟,默认 120~360,在此区间内随机,固定周期本身是机器特征)。
	SessionRotateMinMinutes int `json:"session_rotate_min_minutes"`
	SessionRotateMaxMinutes int `json:"session_rotate_max_minutes"`

	// DailyTokenBudget 是每 key 每日 token 预算(input+cache+output 合计),
	// 超出直接返回 429(默认 0=不限制)。按本地日期零点重置。
	DailyTokenBudget int64 `json:"daily_token_budget"`
}

// Load 从 path 读取配置并填充默认值。
func Load(path string) (*Config, error) {
	c := &Config{}
	if path != "" {
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read config: %w", err)
		}
		if err := json.Unmarshal(data, c); err != nil {
			return nil, fmt.Errorf("parse config: %w", err)
		}
	}
	c.applyDefaults()
	return c, nil
}

func (c *Config) applyDefaults() {
	if c.Listen == "" {
		c.Listen = "127.0.0.1:8080"
	}
	if c.UpstreamAuthScheme == "" {
		c.UpstreamAuthScheme = "bearer"
	}
	if c.UpstreamPath == "" {
		c.UpstreamPath = "/v1/messages?beta=true"
	}
	if c.CountTokensPath == "" {
		c.CountTokensPath = "/v1/messages/count_tokens?beta=true"
	}
	if c.IdentityFile == "" {
		c.IdentityFile = "data/identity.json"
	}
	if c.MaxBodyBytes <= 0 {
		c.MaxBodyBytes = 32 << 20
	}
	if c.MaxUpstreamRetries < 0 {
		c.MaxUpstreamRetries = 2
	}
	b := &c.Behavior
	if b.MaxConcurrency < 0 {
		b.MaxConcurrency = 0
	}
	if b.RPMLimit < 0 {
		b.RPMLimit = 0
	}
	if b.QueueTimeoutSeconds <= 0 {
		b.QueueTimeoutSeconds = 120
	}
	if b.SessionPoolSize < 0 {
		b.SessionPoolSize = 0
	}
	if b.SessionRotateMinMinutes <= 0 {
		b.SessionRotateMinMinutes = 120
	}
	if b.SessionRotateMaxMinutes <= b.SessionRotateMinMinutes {
		b.SessionRotateMaxMinutes = b.SessionRotateMinMinutes * 3
	}
	if c.DumpRetentionHours < 0 {
		c.DumpRetentionHours = 0
	}
}
