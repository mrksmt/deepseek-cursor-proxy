package transform

import (
	"context"
	"testing"
)

// TestNormalizeMessagesUnpacksVariadic locks in the call-site contract that
// caused the live collapse: passing a []any as a single variadic argument
// (without ...) makes normalizeMessages see one raw value, which is not a
// map[string]any and gets coerced into a single "user" message. That is what
// produced truncate.before=1 / messages.upstream_count=1 while Cursor still
// sent 244+ messages.
func TestNormalizeMessagesUnpacksVariadic(t *testing.T) {
	raw := []any{
		map[string]any{"role": "system", "content": "sys"},
		map[string]any{"role": "user", "content": "u1"},
		map[string]any{"role": "assistant", "content": "a1"},
		map[string]any{"role": "user", "content": "u2"},
	}

	// Correct call: unpack so each map is its own argument.
	got, _, _, _ := normalizeMessages(context.Background(), nil, "", false, false, raw...)
	if len(got) != 4 {
		t.Fatalf("unpacked: len = %d, want 4", len(got))
	}
	if got[0].Role != "system" || got[1].Role != "user" || got[3].Content != "u2" {
		t.Fatalf("unpacked messages corrupted: %+v", got)
	}

	// Incorrect call (the live bug): pass the slice as one argument.
	collapsed, _, _, _ := normalizeMessages(context.Background(), nil, "", false, false, raw)
	if len(collapsed) != 1 {
		t.Fatalf("collapsed: len = %d, want 1 (documents the bug shape)", len(collapsed))
	}
	if collapsed[0].Role != "user" {
		t.Fatalf("collapsed role = %q, want user (non-map coerced)", collapsed[0].Role)
	}
}
