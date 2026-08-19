package mimic

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
)

// fingerprintSalt 是计算 cc_version 后缀指纹的盐值。
// 来源: sub2api gateway_billing_block.go(原值来自真实 Claude Code CLI 抓包推导,
// 与 Parrot src/transform/cc_mimicry.py 的 FINGERPRINT_SALT 一致)。
// 改动会导致 fp 与真实 CLI 不一致,进而触发第三方检测。
const fingerprintSalt = "59cf53e54c78"

// ComputeClaudeCodeFingerprint 复刻真实 Claude Code CLI 的 cc_version 指纹算法:
//
//  1. 取 messages 中第一条 role=user 的纯文本(首块 text)
//  2. 取该文本的第 4、7、20 字符(不足以 '0' 补齐,0-based)
//  3. SHA256(SALT + chars + cc_version) 取 hex 前 3 字符
//
// 来源: sub2api gateway_billing_block.go computeClaudeCodeFingerprint。
// 任何偏差都会导致 cc_version=X.Y.Z.{fp} 与真实 CLI 不一致。
func ComputeClaudeCodeFingerprint(body []byte, version string) string {
	firstText := ExtractFirstUserText(body)
	indices := []int{4, 7, 20}
	chars := make([]byte, 0, 3)
	for _, i := range indices {
		if i < len(firstText) {
			chars = append(chars, firstText[i])
		} else {
			chars = append(chars, '0')
		}
	}
	sum := sha256.Sum256([]byte(fingerprintSalt + string(chars) + version))
	return hex.EncodeToString(sum[:])[:3]
}

// ExtractFirstUserText 提取 messages 中第一条 user 消息的首段 text 内容。
// 兼容 string 和 []block 两种 content 格式。
// 来源: sub2api gateway_billing_block.go extractFirstUserText(gjson → 标准库重写,
// 语义等价:遇到首条 user 消息即停;content 为数组时取首个 type=="text" 的块)。
func ExtractFirstUserText(body []byte) string {
	var top struct {
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(body, &top); err != nil {
		return ""
	}
	for _, msg := range top.Messages {
		if msg.Role != "user" {
			continue
		}
		// content 为纯字符串
		var s string
		if err := json.Unmarshal(msg.Content, &s); err == nil {
			return s
		}
		// content 为 block 数组,取首个 text 块
		var blocks []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		if err := json.Unmarshal(msg.Content, &blocks); err == nil {
			for _, b := range blocks {
				if b.Type == "text" {
					return b.Text
				}
			}
		}
		return ""
	}
	return ""
}

// BuildBillingAttributionText 构造 system 数组的 billing attribution 文本。
// 形态对齐真实 Claude Code CLI:
//
//	x-anthropic-billing-header: cc_version=2.1.161.{fp}; cc_entrypoint=cli;
//
// 注意:新版 CLI 已不发送 cch=... 签名字段(sub2api issue #3358),
// 故不注入 cch 段。此 block 不带 cache_control(与真实 CLI 一致)。
// 来源: sub2api gateway_billing_block.go buildBillingAttributionText。
func BuildBillingAttributionText(body []byte, cliVersion string) (string, error) {
	if cliVersion == "" {
		return "", fmt.Errorf("cliVersion required")
	}
	fp := ComputeClaudeCodeFingerprint(body, cliVersion)
	return fmt.Sprintf(
		"x-anthropic-billing-header: cc_version=%s.%s; cc_entrypoint=cli;",
		cliVersion, fp,
	), nil
}
