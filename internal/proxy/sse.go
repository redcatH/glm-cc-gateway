package proxy

import (
	"bytes"
	"encoding/json"
)

// sseEventScanner 把可能被 TCP 边界劈开的 SSE 字节流重组为完整事件
// (以空行 "\n\n" 分隔),供透传时逐事件解析 usage 并保持 flush 实时性。
type sseEventScanner struct {
	buf []byte
}

// Feed 送入一段原始字节,返回其中所有完整事件(含结尾空行,原样字节)。
func (s *sseEventScanner) Feed(chunk []byte) [][]byte {
	s.buf = append(s.buf, chunk...)
	var events [][]byte
	for {
		idx := bytes.Index(s.buf, []byte("\n\n"))
		if idx < 0 {
			break
		}
		event := make([]byte, idx+2)
		copy(event, s.buf[:idx+2])
		s.buf = s.buf[idx+2:]
		events = append(events, event)
	}
	return events
}

// Flush 返回残留尾部(流结束时调用)。
func (s *sseEventScanner) Flush() []byte {
	rest := s.buf
	s.buf = nil
	return rest
}

// extractUsage 从单个 SSE 事件中提取 usage 增量:
//   - message_start.message.usage:input_tokens(全量)
//   - message_delta.usage:output_tokens(累计)
//
// 返回 ok=false 表示该事件不含 usage。
func extractUsage(event []byte) (in, out, cacheRead, cacheCreation int64, ok bool) {
	for _, line := range bytes.Split(event, []byte("\n")) {
		if !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}
		payload := bytes.TrimSpace(line[len("data:"):])
		if len(payload) == 0 || payload[0] != '{' {
			continue
		}
		var ev struct {
			Type    string `json:"type"`
			Message *struct {
				Usage *usageFields `json:"usage"`
			} `json:"message"`
			Usage *usageFields `json:"usage"`
		}
		if err := json.Unmarshal(payload, &ev); err != nil {
			continue
		}
		u := ev.Usage
		if u == nil && ev.Message != nil {
			u = ev.Message.Usage
		}
		if u != nil {
			return u.InputTokens, u.OutputTokens, u.CacheReadInputTokens, u.CacheCreationInputTokens, true
		}
	}
	return 0, 0, 0, 0, false
}

type usageFields struct {
	InputTokens              int64 `json:"input_tokens"`
	OutputTokens             int64 `json:"output_tokens"`
	CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
	CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
}

// extractNonStreamUsage 解析非流式 JSON 响应的 usage。
func extractNonStreamUsage(body []byte) (in, out, cacheRead, cacheCreation int64, ok bool) {
	var top struct {
		Usage *usageFields `json:"usage"`
	}
	if err := json.Unmarshal(body, &top); err != nil || top.Usage == nil {
		return 0, 0, 0, 0, false
	}
	return top.Usage.InputTokens, top.Usage.OutputTokens,
		top.Usage.CacheReadInputTokens, top.Usage.CacheCreationInputTokens, true
}
