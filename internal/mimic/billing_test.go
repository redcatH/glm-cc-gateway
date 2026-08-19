package mimic

import (
	"encoding/json"
	"testing"
)

// capturedFirstUserText 是 2026-08-19 抓包样本中首条 user 消息的首块 text
// (Claude Code 注入的 currentDate system-reminder 块,逐字节还原)。
const capturedFirstUserText = "<system-reminder>\nAs you answer the user's questions, you can use the following context:\n# currentDate\nToday's date is 2026/08/19.\n\n      IMPORTANT: this context may or may not be relevant to your tasks. You should not respond to this context unless it is highly relevant to your task.\n</system-reminder>\n\n"

func capturedSampleBody(t *testing.T) []byte {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"model": "deepseek-v4-flash-0731",
		"messages": []any{
			map[string]any{
				"role": "user",
				"content": []any{
					map[string]any{"type": "text", "text": capturedFirstUserText},
					map[string]any{"type": "text", "text": "你好", "cache_control": map[string]string{"type": "ephemeral"}},
				},
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return body
}

// TestBillingFingerprintSub2apiParity 锁定移植行为:sub2api 算法在 2026-08-19
// 抓包样本(CLI 2.1.195)上的输出。
//
// ⚠ 已知偏差:该抓包中真实 CLI 的 billing block 为 cc_version=2.1.195.d80,
// 而 sub2api(及本移植)算法输出 fe4 —— 说明新版 CLI 修改了指纹算法
// (sub2api 算法校准自 CLI 2.1.1xx 时代的 Parrot 逆向)。
// 逆向新版算法需要更多同版本、不同首条 user 文本的抓包样本做交叉约束;
// 在此之前阶段 2 沿用 sub2api 算法保持自洽(上游判第三方的关键信号是
// "缺失 billing block",fp 数值本身是否精确复刻影响次之 —— 见 sub2api 注释)。
func TestBillingFingerprintSub2apiParity(t *testing.T) {
	body := capturedSampleBody(t)

	if got := ExtractFirstUserText(body); got != capturedFirstUserText {
		t.Fatalf("ExtractFirstUserText mismatch:\n got: %q\nwant: %q", got, capturedFirstUserText)
	}

	// 首块文本第 4/7/20 字符应为 't'、'-'、' '(0-based)。
	chars := []byte{capturedFirstUserText[4], capturedFirstUserText[7], capturedFirstUserText[20]}
	if s := string(chars); s != "t- " {
		t.Fatalf("expected chars at 4/7/20 = %q, got %q", "t- ", s)
	}

	if got := ComputeClaudeCodeFingerprint(body, "2.1.195"); got != "fe4" {
		t.Fatalf("sub2api parity fingerprint changed: got %q, want fe4 (locked)", got)
	}
}

func TestComputeClaudeCodeFingerprintShortText(t *testing.T) {
	// 文本不足 20 字符时以 '0' 补齐。
	body, _ := json.Marshal(map[string]any{
		"messages": []any{map[string]any{"role": "user", "content": "ab"}},
	})
	got := ComputeClaudeCodeFingerprint(body, "2.1.220")
	if len(got) != 3 {
		t.Fatalf("expected 3-hex fingerprint, got %q", got)
	}
}

func TestBuildBillingAttributionText(t *testing.T) {
	body := capturedSampleBody(t)
	got, err := BuildBillingAttributionText(body, "2.1.195")
	if err != nil {
		t.Fatal(err)
	}
	want := "x-anthropic-billing-header: cc_version=2.1.195.fe4; cc_entrypoint=cli;"
	if got != want {
		t.Fatalf("billing text mismatch:\n got: %q\nwant: %q", got, want)
	}
}
