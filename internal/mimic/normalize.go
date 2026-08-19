package mimic

import (
	"encoding/json"
)

// 本文件移植 sub2api gateway_claude_oauth_body.go:
//   - normalizeClaudeOAuthRequestBody(参数缺省补齐部分)
//   - enforceCacheControlLimit / collectCacheControlPaths

// MaxCacheControlBlocks 是 Anthropic API 允许的最大 cache_control 块数量。
const MaxCacheControlBlocks = 4

// NormalizeParams 对齐真实 Claude Code CLI 的参数指纹,全部为**缺省才补、
// 客户端传了就透传**(对齐 sub2api 语义,覆盖会破坏客户端行为):
//   - tools 缺失 → 补 [](真实 CLI 总是发送 tools 字段)
//   - temperature 缺失 → 补 1
//   - max_tokens 缺失 → 补 128000(CLI 默认)
//   - context_management 缺失且 thinking 为 enabled/adaptive → 补
//     {"edits":[{"type":"clear_thinking_20251015","keep":"all"}]}
func NormalizeParams(body []byte) []byte {
	if len(body) == 0 {
		return body
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(body, &top); err != nil {
		return body
	}
	modified := false

	if _, ok := top["tools"]; !ok {
		top["tools"] = rawJSON("[]")
		modified = true
	}
	if _, ok := top["temperature"]; !ok {
		top["temperature"] = rawJSON("1")
		modified = true
	}
	if _, ok := top["max_tokens"]; !ok {
		top["max_tokens"] = rawJSON("128000")
		modified = true
	}
	if _, ok := top["context_management"]; !ok {
		var th struct {
			Type string `json:"type"`
		}
		if raw, ok := top["thinking"]; ok && json.Unmarshal(raw, &th) == nil {
			if th.Type == "enabled" || th.Type == "adaptive" {
				top["context_management"] = rawJSON(`{"edits":[{"type":"clear_thinking_20251015","keep":"all"}]}`)
				modified = true
			}
		}
	}
	if !modified {
		return body
	}
	out, err := json.Marshal(top)
	if err != nil {
		return body
	}
	return out
}

// EnforceCacheControlLimit 把 cache_control 块数压到 MaxCacheControlBlocks 以内。
// 策略对齐 sub2api enforceCacheControlLimit:
//  1. thinking 块不支持 cache_control,一律删除
//  2. 超限时依次从 tools(自后向前)→ messages(自前向后)→ system 删除
func EnforceCacheControlLimit(body []byte) []byte {
	if len(body) == 0 {
		return body
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(body, &top); err != nil {
		return body
	}
	modified := false

	count := 0

	// --- system 块 ---
	sysChanged := false
	var sysBlocks []map[string]json.RawMessage
	if raw, ok := top["system"]; ok && json.Unmarshal(raw, &sysBlocks) == nil {
		for i, blk := range sysBlocks {
			if _, has := blk["cache_control"]; !has {
				continue
			}
			if isThinkingBlock(blk) {
				delete(sysBlocks[i], "cache_control")
				sysChanged = true
				continue
			}
			count++
		}
	}
	if sysChanged {
		if b, err := json.Marshal(sysBlocks); err == nil {
			top["system"] = b
			modified = true
		}
	}

	// --- tools 元素 ---
	toolsChanged := false
	var tools []map[string]json.RawMessage
	if raw, ok := top["tools"]; ok && json.Unmarshal(raw, &tools) == nil {
		for i, blk := range tools {
			if _, has := blk["cache_control"]; !has {
				continue
			}
			if isThinkingBlock(blk) {
				delete(tools[i], "cache_control")
				toolsChanged = true
				continue
			}
			count++
		}
	}

	// --- messages 内容块 ---
	msgsChanged := false
	var msgs []json.RawMessage
	if raw, ok := top["messages"]; ok && json.Unmarshal(raw, &msgs) == nil {
		for mi, rawMsg := range msgs {
			var msg struct {
				Content json.RawMessage `json:"content"`
			}
			if json.Unmarshal(rawMsg, &msg) != nil {
				continue
			}
			var blocks []map[string]json.RawMessage
			if json.Unmarshal(msg.Content, &blocks) != nil {
				continue // content 为字符串等
			}
			blkChanged := false
			for bi, blk := range blocks {
				if _, has := blk["cache_control"]; !has {
					continue
				}
				if isThinkingBlock(blk) {
					delete(blocks[bi], "cache_control")
					blkChanged = true
					continue
				}
				count++
			}
			if blkChanged {
				if nb, err := json.Marshal(blocks); err == nil {
					var m map[string]json.RawMessage
					if json.Unmarshal(rawMsg, &m) == nil {
						m["content"] = nb
						if nm, err := json.Marshal(m); err == nil {
							msgs[mi] = nm
							msgsChanged = true
						}
					}
				}
			}
		}
	}

	if count <= MaxCacheControlBlocks {
		if msgsChanged {
			top["messages"] = rawJSONArray(msgs)
			modified = true
		}
		if toolsChanged {
			if b, err := json.Marshal(tools); err == nil {
				top["tools"] = b
				modified = true
			}
		}
		if !modified {
			return body
		}
		out, err := json.Marshal(top)
		if err != nil {
			return body
		}
		return out
	}

	// 超限:tools 自后向前删。
	remaining := count - MaxCacheControlBlocks
	for i := len(tools) - 1; i >= 0 && remaining > 0; i-- {
		if _, has := tools[i]["cache_control"]; !has {
			continue
		}
		delete(tools[i], "cache_control")
		remaining--
		toolsChanged = true
	}

	// 再从 messages 自前向后删。
	for mi := 0; mi < len(msgs) && remaining > 0; mi++ {
		var msg struct {
			Content json.RawMessage `json:"content"`
		}
		if json.Unmarshal(msgs[mi], &msg) != nil {
			continue
		}
		var blocks []map[string]json.RawMessage
		if json.Unmarshal(msg.Content, &blocks) != nil {
			continue
		}
		blkChanged := false
		for bi := range blocks {
			if remaining <= 0 {
				break
			}
			if _, has := blocks[bi]["cache_control"]; !has {
				continue
			}
			delete(blocks[bi], "cache_control")
			remaining--
			blkChanged = true
		}
		if blkChanged {
			if nb, err := json.Marshal(blocks); err == nil {
				var m map[string]json.RawMessage
				if json.Unmarshal(msgs[mi], &m) == nil {
					m["content"] = nb
					if nm, err := json.Marshal(m); err == nil {
						msgs[mi] = nm
						msgsChanged = true
					}
				}
			}
		}
	}

	// 最后从 system 删。
	for i := len(sysBlocks) - 1; i >= 0 && remaining > 0; i-- {
		if _, has := sysBlocks[i]["cache_control"]; !has {
			continue
		}
		delete(sysBlocks[i], "cache_control")
		remaining--
		sysChanged = true
	}

	if sysChanged {
		if b, err := json.Marshal(sysBlocks); err == nil {
			top["system"] = b
		}
	}
	if toolsChanged {
		if b, err := json.Marshal(tools); err == nil {
			top["tools"] = b
		}
	}
	if msgsChanged {
		top["messages"] = rawJSONArray(msgs)
	}
	out, err := json.Marshal(top)
	if err != nil {
		return body
	}
	return out
}

func isThinkingBlock(blk map[string]json.RawMessage) bool {
	raw, ok := blk["type"]
	if !ok {
		return false
	}
	var t string
	return json.Unmarshal(raw, &t) == nil && t == "thinking"
}
