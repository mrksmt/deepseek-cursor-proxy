package transform

import (
	"testing"

	"github.com/mrksmt/deepseek-cursor-proxy/internal/models"
)

func hasTool(t *testing.T, msgs []models.Message, id string) bool {
	t.Helper()
	for _, m := range msgs {
		for _, tc := range m.ToolCalls {
			if tc.ID == id {
				return true
			}
		}
	}
	return false
}

func hasToolResult(t *testing.T, msgs []models.Message, id string) bool {
	t.Helper()
	for _, m := range msgs {
		if m.Role == "tool" && m.ToolCallID == id {
			return true
		}
	}
	return false
}

// TestTruncateOrphansToolCallAtRoundBoundary documents the "thinking loop"
// root cause: a thinking turn ends in an assistant tool_call, so cutting at a
// user-message round boundary drops the tool result but keeps the tool_call
// (or vice versa), leaving an orphan upstream rejects. The fix bypasses
// truncation entirely on the thinking/recovery path, so nothing is cut here.
func TestTruncateOrphansToolCallAtRoundBoundary(t *testing.T) {
	msgs := []models.Message{
		mkMsg("system", "sys"),
		mkMsg("user", "u1"),
		{Role: "assistant", ToolCalls: []models.ToolCall{{ID: "call_1"}}},
		{Role: "tool", ToolCallID: "call_1", Content: "result"},
		mkMsg("user", "u2"),
		mkMsg("assistant", "a2"),
	}

	// maxMessages=4 forces the cut onto the "u2" round start.
	got, removed, _ := truncateMessages(msgs, 4, 0)
	if removed != 3 {
		t.Fatalf("removed = %d, want 3 (cut before u2)", removed)
	}
	if hasTool(t, got, "call_1") {
		t.Fatalf("orphaned tool_call survived truncation: %+v", got)
	}
	if hasToolResult(t, got, "call_1") {
		t.Fatalf("orphaned tool result survived truncation: %+v", got)
	}
}
