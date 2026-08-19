package mimic

import (
	"bytes"
	"encoding/json"
)

// 本文件合并 sub2api 的两条 metadata.user_id 处理路径:
//   - 身份注入(buildOAuthMetadataUserID):客户端没带 user_id 时合成注入
//   - 身份重写(RewriteUserID):客户端带了 user_id 时整体重写,避免泄露下游真实身份
//
// 两者的 session_id 派生规则统一为:
//   - 下游带会话锚点(其 user_id 的 session_id / X-Claude-Code-Session-Id 头):
//     seed = 身份键 + "::" + 锚点(对齐 sub2api RewriteUserID 的
//     SHA256(accountID::sessionTail) 语义 —— 同下游会话跨轮稳定)
//   - 无锚点:seed = BuildStableSessionSeed(身份键, 客户端区分因子, 首条 user 文本)
//     (对齐 sub2api buildStableSessionSeed —— 同对话追加消息时跨轮稳定)

// RewriteInput 汇聚 session_id 派生所需的请求上下文。
type RewriteInput struct {
	IdentityKey    string // 下游 key 的身份键(KeyIdentityHash 产物)
	ClientIP       string
	UserAgent      string // 下游原始 UA(仅用于区分因子,不透传)
	SessionAnchor  string // 下游会话锚点:其 metadata.user_id 的 session_id 或 session 头
	CLIVersionHint string // 输出格式版本,空则用 CLICurrentVersion

	// SessionOverride 非空时直接使用该 session_id(会话池分配),
	// 跳过内部派生。头同步与 body 写入逻辑不变。
	SessionOverride string
}

// RewriteMetadataUserID 将 body 的 metadata.user_id 统一重写为该 key 的伪装身份,
// 返回新 body 与派生的 session_id(供 X-Claude-Code-Session-Id 头同步)。
// 仅修改 metadata.user_id 一个字段,其余顶层字段保留原始字节
// (对齐 sub2api 用 RawMessage 避免重新序列化破坏 thinking 块的做法)。
func RewriteMetadataUserID(body []byte, id Identity, in RewriteInput) ([]byte, string) {
	firstUserText := ExtractFirstUserText(body)

	seed := BuildStableSessionSeed(in.IdentityKey, SessionDiscriminator(in.ClientIP, in.UserAgent, in.IdentityKey), firstUserText)
	if in.SessionAnchor != "" {
		seed = in.IdentityKey + "::" + in.SessionAnchor
	}
	sessionID := GenerateSessionUUID(seed)
	if in.SessionOverride != "" {
		sessionID = in.SessionOverride
	}

	version := in.CLIVersionHint
	if version == "" {
		version = CLICurrentVersion
	}
	userID := FormatMetadataUserID(id.ClientID, "", sessionID, version)

	// 解析顶层为 RawMessage map,只动 metadata,其余字段原样保留字节。
	var top map[string]json.RawMessage
	if err := json.Unmarshal(body, &top); err != nil {
		return body, sessionID
	}

	var metadata map[string]json.RawMessage
	if raw, ok := top["metadata"]; ok {
		trimmed := bytes.TrimSpace(raw)
		if len(trimmed) == 0 || string(trimmed) == "null" {
			metadata = map[string]json.RawMessage{}
		} else if err := json.Unmarshal(trimmed, &metadata); err != nil {
			// metadata 不是对象(畸形输入):整体替换为只含 user_id 的对象
			metadata = map[string]json.RawMessage{}
		}
	} else {
		metadata = map[string]json.RawMessage{}
	}

	uidRaw, _ := json.Marshal(userID)
	metadata["user_id"] = uidRaw
	metaRaw, err := json.Marshal(metadata)
	if err != nil {
		return body, sessionID
	}
	top["metadata"] = metaRaw

	out, err := json.Marshal(top)
	if err != nil {
		return body, sessionID
	}
	return out, sessionID
}
