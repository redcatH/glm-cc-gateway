package mimic

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// 本文件移植自 sub2api:
//   - internal/service/metadata_userid.go  (user_id 两种格式/解析/格式化/版本判断)
//   - internal/service/identity_service.go (generateClientID/generateRandomUUID/generateUUIDFromSeed)
//   - internal/service/gateway_claude_oauth_body.go (buildStableSessionSeed/generateSessionUUID)

// NewMetadataFormatMinVersion 是使用 JSON 格式 metadata.user_id 的最低 CLI 版本
// (旧版为拼接字符串格式)。
const NewMetadataFormatMinVersion = "2.1.78"

// ParsedUserID 是从 metadata.user_id 中解析出的各组成部分。
type ParsedUserID struct {
	DeviceID    string // 64-char hex (or arbitrary client id)
	AccountUUID string // may be empty
	SessionID   string // UUID
	IsNewFormat bool   // true 表示原值为 JSON 格式
}

// legacyUserIDRegex 匹配旧版 user_id 格式:
//
//	user_{64hex}_account_{optional_uuid}_session_{uuid}
var legacyUserIDRegex = regexp.MustCompile(`^user_([a-fA-F0-9]{64})_account_([a-fA-F0-9-]*)_session_([a-fA-F0-9-]{36})$`)

// claudeCodeUAVersionPattern 匹配 claude-cli UA 中的版本号。
var claudeCodeUAVersionPattern = regexp.MustCompile(`(?i)^claude-cli/(\d+\.\d+\.\d+)`)

type jsonUserID struct {
	DeviceID    string `json:"device_id"`
	AccountUUID string `json:"account_uuid"`
	SessionID   string `json:"session_id"`
}

// ParseMetadataUserID 解析两种格式的 metadata.user_id,无法解析时返回 nil。
func ParseMetadataUserID(raw string) *ParsedUserID {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	if raw[0] == '{' {
		var j jsonUserID
		if err := json.Unmarshal([]byte(raw), &j); err != nil {
			return nil
		}
		if j.DeviceID == "" || j.SessionID == "" {
			return nil
		}
		return &ParsedUserID{
			DeviceID:    j.DeviceID,
			AccountUUID: j.AccountUUID,
			SessionID:   j.SessionID,
			IsNewFormat: true,
		}
	}
	matches := legacyUserIDRegex.FindStringSubmatch(raw)
	if matches == nil {
		return nil
	}
	return &ParsedUserID{
		DeviceID:    matches[1],
		AccountUUID: matches[2],
		SessionID:   matches[3],
		IsNewFormat: false,
	}
}

// FormatMetadataUserID 按 CLI 版本选择格式构造 metadata.user_id 字符串。
func FormatMetadataUserID(deviceID, accountUUID, sessionID, uaVersion string) string {
	if IsNewMetadataFormatVersion(uaVersion) {
		b, _ := json.Marshal(jsonUserID{
			DeviceID:    deviceID,
			AccountUUID: accountUUID,
			SessionID:   sessionID,
		})
		return string(b)
	}
	return "user_" + deviceID + "_account_" + accountUUID + "_session_" + sessionID
}

// IsNewMetadataFormatVersion 判断该 CLI 版本是否使用 JSON 格式 user_id (>= 2.1.78)。
func IsNewMetadataFormatVersion(version string) bool {
	if version == "" {
		return false
	}
	return CompareVersions(version, NewMetadataFormatMinVersion) >= 0
}

// ExtractCLIVersion 从 User-Agent 中提取 Claude Code 版本号,不匹配返回 ""。
func ExtractCLIVersion(ua string) string {
	matches := claudeCodeUAVersionPattern.FindStringSubmatch(ua)
	if len(matches) >= 2 {
		return matches[1]
	}
	return ""
}

// CompareVersions 比较两个三段 semver(语义与 sub2api claude_code_validator.go 一致):
// 返回 -1/0/1。无法解析的段按 0 处理。
func CompareVersions(a, b string) int {
	pa := strings.Split(strings.TrimPrefix(a, "v"), ".")
	pb := strings.Split(strings.TrimPrefix(b, "v"), ".")
	for i := 0; i < 3; i++ {
		var va, vb int
		if i < len(pa) {
			va, _ = strconv.Atoi(pa[i])
		}
		if i < len(pb) {
			vb, _ = strconv.Atoi(pb[i])
		}
		if va != vb {
			if va < vb {
				return -1
			}
			return 1
		}
	}
	return 0
}

// GenerateClientID 生成 64 位十六进制客户端 ID(32 字节随机数)。
// 来源: sub2api identity_service.go generateClientID。
func GenerateClientID() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		h := sha256.Sum256([]byte(fmt.Sprintf("%d", time.Now().UnixNano())))
		return hex.EncodeToString(h[:])
	}
	return hex.EncodeToString(b)
}

// NewRandomUUID 生成随机 UUID v4 格式字符串。
// 来源: sub2api identity_service.go generateRandomUUID。
func NewRandomUUID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		h := sha256.Sum256([]byte(fmt.Sprintf("%d", time.Now().UnixNano())))
		b = h[:16]
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return formatUUID(b)
}

// GenerateSessionUUID 从种子生成确定性 UUID v4 格式字符串;种子为空则随机。
// 来源: sub2api gateway_claude_oauth_body.go generateSessionUUID。
func GenerateSessionUUID(seed string) string {
	if seed == "" {
		return NewRandomUUID()
	}
	hash := sha256.Sum256([]byte(seed))
	b := hash[:16]
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return formatUUID(b)
}

// BuildStableSessionSeed 为合成的 session_id 生成"会话级稳定"种子:
// 身份键 + 客户端区分因子 + 首条 user 消息文本。对话尾部追加消息时三者不变,
// 因此派生出的 session_id 跨轮稳定,贴近真实 CC 进程级 session_id。
// 来源: sub2api gateway_claude_oauth_body.go buildStableSessionSeed
// (原版第一段是 int64 accountID,这里用 key 哈希字符串,语义等价)。
func BuildStableSessionSeed(identityKey, clientDiscriminator, firstUserText string) string {
	var b strings.Builder
	b.WriteString(identityKey)
	b.WriteString("::")
	b.WriteString(clientDiscriminator)
	b.WriteString("::")
	b.WriteString(firstUserText)
	return b.String()
}

// SessionDiscriminator 把请求上下文(客户端 IP / 归一化 UA / 身份键)拼成
// 跨客户端的区分因子,避免不同用户的相同首条消息派生出相同 session_id。
// 来源: sub2api sessionContextDiscriminator(简化:APIKeyID → 身份键)。
func SessionDiscriminator(clientIP, userAgent, identityKey string) string {
	return clientIP + ":" + NormalizeSessionUserAgent(userAgent) + ":" + identityKey
}

// NormalizeSessionUserAgent 对 UA 做归一化,剥离高频变化的版本细节,
// 只保留产品标识,用于 session 区分因子。
func NormalizeSessionUserAgent(raw string) string {
	s := strings.TrimSpace(raw)
	if s == "" {
		return ""
	}
	if idx := strings.IndexByte(s, '/'); idx > 0 {
		s = s[:idx] // 去掉版本号部分,如 "opencode/1.2.3" → "opencode"
	}
	s = strings.ToLower(s)
	if len(s) > 64 {
		s = s[:64]
	}
	return s
}

func formatUUID(b []byte) string {
	return fmt.Sprintf("%x-%x-%x-%x-%x",
		b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
