package mimic

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestSyncBillingVersion 用 2026-08-19 真实 dump 的形态构造样本:
// 下游 CC 2.1.235 的 billing block 应被同步为 CLICurrentVersion。
func TestSyncBillingVersion(t *testing.T) {
	body, _ := json.Marshal(map[string]any{
		"system": []any{
			map[string]any{"type": "text", "text": "x-anthropic-billing-header: cc_version=2.1.235.4e7; cc_entrypoint=cli;"},
			map[string]any{"type": "text", "text": "You are Claude Code, Anthropic's official CLI for Claude.", "cache_control": map[string]string{"type": "ephemeral"}},
		},
		"messages": []any{
			map[string]any{"role": "user", "content": []any{map[string]any{"type": "text", "text": "hello world"}}},
		},
	})

	out := SyncBillingVersion(body)

	var top struct {
		System []struct {
			Text         string `json:"text"`
			CacheControl *struct {
				Type string `json:"type"`
			} `json:"cache_control"`
		} `json:"system"`
	}
	if err := json.Unmarshal(out, &top); err != nil {
		t.Fatal(err)
	}
	wantPrefix := "x-anthropic-billing-header: cc_version=" + CLICurrentVersion + "."
	if !strings.HasPrefix(top.System[0].Text, wantPrefix) {
		t.Fatalf("billing block not synced: %q", top.System[0].Text)
	}
	if strings.Contains(top.System[0].Text, "2.1.235") {
		t.Fatalf("old version leaked: %q", top.System[0].Text)
	}
	// fp 段与算法输出一致。
	wantFP := ComputeClaudeCodeFingerprint(body, CLICurrentVersion)
	if !strings.Contains(top.System[0].Text, CLICurrentVersion+"."+wantFP+";") {
		t.Fatalf("fp mismatch in %q (want %s)", top.System[0].Text, wantFP)
	}
	// 其余 block 的 cache_control 保留。
	if top.System[1].CacheControl == nil || top.System[1].CacheControl.Type != "ephemeral" {
		t.Fatalf("cache_control lost: %+v", top.System[1])
	}
}

func TestSyncBillingVersionNoBillingBlock(t *testing.T) {
	body, _ := json.Marshal(map[string]any{
		"system":   "plain string system",
		"messages": []any{},
	})
	if out := SyncBillingVersion(body); string(out) != string(body) {
		t.Fatal("string system must be untouched")
	}
}
