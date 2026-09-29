package transform

import (
	"testing"

	"github.com/mrksmt/deepseek-cursor-proxy/internal/models"
)

func TestMessageToMapPreservesToolCallID(t *testing.T) {
	msg := models.Message{
		Role:       "tool",
		Content:    `{"ok":true}`,
		ToolCallID: "call_abc123",
		Name:       "Shell",
	}
	got := messageToMap(msg)
	if got["tool_call_id"] != "call_abc123" {
		t.Fatalf("tool_call_id = %v, want call_abc123", got["tool_call_id"])
	}
	if got["name"] != "Shell" {
		t.Fatalf("name = %v, want Shell", got["name"])
	}
	if got["role"] != "tool" {
		t.Fatalf("role = %v, want tool", got["role"])
	}
}

func TestMessageToMapOmitsEmptyOptionalFields(t *testing.T) {
	msg := models.Message{Role: "user", Content: "hi"}
	got := messageToMap(msg)
	for _, key := range []string{"tool_call_id", "name", "prefix", "reasoning_content", "tool_calls"} {
		if _, ok := got[key]; ok {
			t.Fatalf("unexpected key %q in map: %v", key, got)
		}
	}
}

// Round-trip through messagesToRaw must keep tool_call_id, otherwise the
// second normalizeMessages pass (and stripRecoveryNoticeForUpstream) would
// drop it before the body is sent upstream.
func TestMessagesToRawRoundTripToolCallID(t *testing.T) {
	in := []models.Message{
		{Role: "assistant", Content: "", ToolCalls: []models.ToolCall{
			{ID: "call_1", Type: "function", Function: models.ToolCallFunction{Name: "Shell", Arguments: "{}"}},
		}},
		{Role: "tool", Content: "out", ToolCallID: "call_1"},
	}
	raw := messagesToRaw(in)
	tool, ok := raw[1].(map[string]any)
	if !ok {
		t.Fatalf("raw[1] type = %T, want map", raw[1])
	}
	if tool["tool_call_id"] != "call_1" {
		t.Fatalf("tool_call_id lost in messagesToRaw: %v", tool)
	}
}
