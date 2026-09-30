package transform

import (
	"context"
	"testing"

	"github.com/mrksmt/deepseek-cursor-proxy/internal/models"
)

// Cursor resends prior assistant turns wrapped in a Thinking <details> block.
// Stripping that block must not erase our recovery notice marker — otherwise
// every follow-up request re-runs latest_user recovery and spams the notice
// into the SSE stream again.
func TestNormalizeMessage_PreservesRecoveryNoticeInsideThinkingBlock(t *testing.T) {
	raw := map[string]any{
		"role": "assistant",
		"content": "<details>\n<summary>Thinking</summary>\n\n" +
			RecoveryNoticeContent +
			"some prior reasoning\n</details>\n\nvisible reply",
	}

	msg, _, _, _ := normalizeMessage(context.Background(), raw, nil, "", false, true, nil)
	if !hasRecoveryNotice(msg) {
		t.Fatalf("expected recovery notice marker to survive thinking-block strip, got %q", msg.Content)
	}
	if got, want := msg.Content, RecoveryNoticeContent+"visible reply"; got != want {
		t.Fatalf("content = %q, want %q", got, want)
	}
}

func TestHasRecoveryNotice_DetectsEmbeddedMarker(t *testing.T) {
	msg := models.Message{Role: "assistant", Content: "prefix\n" + RecoveryNoticeText + "\nrest"}
	if !hasRecoveryNotice(msg) {
		t.Fatal("expected embedded recovery notice to match")
	}
}
