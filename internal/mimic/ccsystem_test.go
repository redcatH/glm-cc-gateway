package mimic

import (
	"encoding/json"
	"strings"
	"testing"
)

// thirdPartyBody 构造第三方客户端(如 opencode)形态的请求体。
func thirdPartyBody(t *testing.T) []byte {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"model": "glm-5.2",
		"system": map[string]any{ // 会被 marshal 成对象,改为字符串更典型;下方覆盖
			"placeholder": true},
		"messages": []any{
			map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "text", "text": "写一个快排"},
			}},
		},
		"stream": true,
	})
	if err != nil {
		t.Fatal(err)
	}
	// system 用字符串形态(典型第三方)
	var top map[string]json.RawMessage
	json.Unmarshal(body, &top)
	top["system"], _ = json.Marshal("You are a helpful assistant.")
	out, _ := json.Marshal(top)
	return out
}

// realCCBody 构造真实 Claude Code 直连形态(基于 2026-08-19 dump)。
func realCCBody(t *testing.T) []byte {
	t.Helper()
	body, _ := json.Marshal(map[string]any{
		"model": "glm-5.2",
		"system": []any{
			map[string]any{"type": "text", "text": "x-anthropic-billing-header: cc_version=2.1.235.4e7; cc_entrypoint=cli;"},
			map[string]any{"type": "text", "text": CCSystemPrompt, "cache_control": map[string]string{"type": "ephemeral"}},
		},
		"messages": []any{
			map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "text", "text": "你好"},
			}},
		},
	})
	return body
}

func TestIsRealClaudeCodeBody(t *testing.T) {
	if !IsRealClaudeCodeBody(realCCBody(t)) {
		t.Fatal("real CC body (with billing block) must be detected")
	}
	if IsRealClaudeCodeBody(thirdPartyBody(t)) {
		t.Fatal("third-party body must not be detected as real CC")
	}
}

func TestRewriteSystemForNonCC(t *testing.T) {
	body := thirdPartyBody(t)
	out := RewriteSystemForNonCC(body)

	var top struct {
		System []struct {
			Text         string          `json:"text"`
			CacheControl json.RawMessage `json:"cache_control"`
		} `json:"system"`
		Messages []struct {
			Role    string `json:"role"`
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(out, &top); err != nil {
		t.Fatal(err)
	}
	if len(top.System) != 3 {
		t.Fatalf("expected 3 system blocks, got %d", len(top.System))
	}
	// block[0]: billing,版本=CLICurrentVersion,无 cache_control
	if !strings.HasPrefix(top.System[0].Text, "x-anthropic-billing-header: cc_version="+CLICurrentVersion+".") {
		t.Fatalf("billing block wrong: %q", top.System[0].Text)
	}
	if top.System[0].CacheControl != nil {
		t.Fatal("billing block must not carry cache_control")
	}
	// block[1]: banner
	if top.System[1].Text != CCSystemPrompt {
		t.Fatalf("banner block wrong: %q", top.System[1].Text)
	}
	// block[2]: expansion + ephemeral ttl
	if top.System[2].Text != CCSystemPromptExpansion {
		t.Fatal("expansion block wrong")
	}
	var cc struct {
		Type string `json:"type"`
		TTL  string `json:"ttl"`
	}
	if err := json.Unmarshal(top.System[2].CacheControl, &cc); err != nil || cc.Type != "ephemeral" || cc.TTL != DefaultCacheControlTTL {
		t.Fatalf("expansion cache_control wrong: %s", top.System[2].CacheControl)
	}
	// messages 前插 [System Instructions] + ack
	if len(top.Messages) != 3 || top.Messages[0].Role != "user" || top.Messages[1].Role != "assistant" {
		t.Fatalf("degraded messages not injected: %+v", top.Messages)
	}
	if !strings.HasPrefix(top.Messages[0].Content[0].Text, "[System Instructions]\nYou are a helpful assistant.") {
		t.Fatalf("original system not preserved: %q", top.Messages[0].Content[0].Text)
	}
	if top.Messages[1].Content[0].Text != "Understood. I will follow these instructions." {
		t.Fatalf("ack message wrong: %q", top.Messages[1].Content[0].Text)
	}
	// 原用户消息保留在尾部
	if top.Messages[2].Content[0].Text != "写一个快排" {
		t.Fatal("user message lost")
	}
}

func TestRewriteSystemNoInjectForCCPrefixedSystem(t *testing.T) {
	// 客户端已发 CC 风格 system(如 opencode 伪装 CC):不注入降级消息。
	var top map[string]json.RawMessage
	body := thirdPartyBody(t)
	json.Unmarshal(body, &top)
	top["system"], _ = json.Marshal(CCSystemPrompt + " extra words")
	body, _ = json.Marshal(top)

	out := RewriteSystemForNonCC(body)
	var parsed struct {
		Messages []json.RawMessage `json:"messages"`
	}
	json.Unmarshal(out, &parsed)
	if len(parsed.Messages) != 1 {
		t.Fatalf("should not inject for CC-prefixed system, got %d messages", len(parsed.Messages))
	}
}

func TestNormalizeParams(t *testing.T) {
	body := thirdPartyBody(t)
	out := NormalizeParams(body)
	var top map[string]json.RawMessage
	if err := json.Unmarshal(out, &top); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"tools", "temperature", "max_tokens"} {
		if _, ok := top[k]; !ok {
			t.Fatalf("%s not filled", k)
		}
	}
	if string(top["tools"]) != "[]" {
		t.Fatalf("tools = %s", top["tools"])
	}
	if string(top["temperature"]) != "1" {
		t.Fatalf("temperature = %s", top["temperature"])
	}
	if string(top["max_tokens"]) != "128000" {
		t.Fatalf("max_tokens = %s", top["max_tokens"])
	}
	if _, ok := top["context_management"]; ok {
		t.Fatal("context_management should not be added without thinking")
	}

	// thinking enabled → 补 context_management;已有值不覆盖。
	body2, _ := json.Marshal(map[string]any{
		"model":       "m",
		"messages":    []any{},
		"thinking":    map[string]any{"type": "enabled", "budget_tokens": 10000},
		"temperature": 0.3,
	})
	out2 := NormalizeParams(body2)
	var top2 struct {
		ContextManagement json.RawMessage `json:"context_management"`
		Temperature       float64         `json:"temperature"`
	}
	json.Unmarshal(out2, &top2)
	if string(top2.ContextManagement) != `{"edits":[{"type":"clear_thinking_20251015","keep":"all"}]}` {
		t.Fatalf("context_management = %s", top2.ContextManagement)
	}
	if top2.Temperature != 0.3 {
		t.Fatal("existing temperature must not be overridden")
	}
}

func TestEnforceCacheControlLimit(t *testing.T) {
	// 构造 6 个 cache_control:2 in messages + 3 in tools + 1 in system(其中 1 个 thinking 非法)
	body, _ := json.Marshal(map[string]any{
		"model": "m",
		"system": []any{
			map[string]any{"type": "text", "text": "sys", "cache_control": map[string]string{"type": "ephemeral"}},
		},
		"tools": []any{
			map[string]any{"name": "a", "cache_control": map[string]string{"type": "ephemeral"}},
			map[string]any{"name": "b", "cache_control": map[string]string{"type": "ephemeral"}},
			map[string]any{"name": "c", "cache_control": map[string]string{"type": "ephemeral"}},
		},
		"messages": []any{
			map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "text", "text": "1", "cache_control": map[string]string{"type": "ephemeral"}},
				map[string]any{"type": "thinking", "thinking": "x", "cache_control": map[string]string{"type": "ephemeral"}},
			}},
			map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "text", "text": "2", "cache_control": map[string]string{"type": "ephemeral"}},
			}},
		},
	})
	out := EnforceCacheControlLimit(body)
	if got := countCacheControls(t, out); got != MaxCacheControlBlocks {
		t.Fatalf("cache_control count = %d, want %d", got, MaxCacheControlBlocks)
	}
}

func countCacheControls(t *testing.T, body []byte) int {
	t.Helper()
	count := 0
	var walk func(v any)
	walk = func(v any) {
		switch x := v.(type) {
		case map[string]any:
			if _, ok := x["cache_control"]; ok {
				count++
			}
			for _, child := range x {
				walk(child)
			}
		case []any:
			for _, child := range x {
				walk(child)
			}
		}
	}
	var top any
	if err := json.Unmarshal(body, &top); err != nil {
		t.Fatal(err)
	}
	walk(top)
	return count
}
