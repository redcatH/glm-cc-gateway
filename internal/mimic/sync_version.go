package mimic

import (
	"bytes"
	"encoding/json"
	"regexp"
)

// billingVersionRegex 匹配 billing attribution block 中的版本段
// (含旧版 CLI 可能残留的三段 fp)。
var billingVersionRegex = regexp.MustCompile(`cc_version=\d+\.\d+\.\d+\.[0-9a-fA-F]{1,8}`)

// SyncBillingVersion 把 body system 中 billing attribution block 的
// cc_version=X.Y.Z.{fp} 同步为网关对外伪装的 CLI 版本(CLICurrentVersion),
// fp 用移植的算法按本请求内容重新计算。
//
// 场景:下游真实 Claude Code(如 2.1.235)直连时,body 自带其版本的 billing
// block,而网关上游 UA 固定为 CLICurrentVersion —— 版本不一致会被上游识别。
// 对齐 sub2api gateway_billing_header.go syncBillingHeaderVersion 的职责。
//
// 仅当 system 为 block 数组且某 text 块含 cc_version= 时改写该块的 text 字段,
// 其余字段(含 cache_control)原样保留;无 billing block 时不做任何事。
func SyncBillingVersion(body []byte) []byte {
	var top struct {
		System json.RawMessage `json:"system"`
	}
	if err := json.Unmarshal(body, &top); err != nil || len(top.System) == 0 {
		return body
	}
	var blocks []map[string]json.RawMessage
	if err := json.Unmarshal(top.System, &blocks); err != nil {
		return body // system 为字符串或畸形:不动(阶段 2 的 system 重写覆盖此场景)
	}

	changed := false
	repl := "cc_version=" + CLICurrentVersion + "." + ComputeClaudeCodeFingerprint(body, CLICurrentVersion)
	for _, blk := range blocks {
		rawText, ok := blk["text"]
		if !ok {
			continue
		}
		var text string
		if err := json.Unmarshal(rawText, &text); err != nil {
			continue
		}
		if !bytes.Contains([]byte(text), []byte("cc_version=")) {
			continue
		}
		next := billingVersionRegex.ReplaceAllString(text, repl)
		if next == text {
			continue
		}
		nextRaw, err := json.Marshal(next)
		if err != nil {
			continue
		}
		blk["text"] = nextRaw
		changed = true
	}
	if !changed {
		return body
	}

	// 用 map[string]RawMessage 顶层替换 system,保留其余顶层字段字节。
	var topMap map[string]json.RawMessage
	if err := json.Unmarshal(body, &topMap); err != nil {
		return body
	}
	newSystem, err := json.Marshal(blocks)
	if err != nil {
		return body
	}
	topMap["system"] = newSystem
	out, err := json.Marshal(topMap)
	if err != nil {
		return body
	}
	return out
}
