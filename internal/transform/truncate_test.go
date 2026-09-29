package transform

import (
	"strings"
	"testing"

	"github.com/mrksmt/deepseek-cursor-proxy/internal/models"
)

// mkMsg builds a message with content long enough to give a predictable
// token estimate: n/4 + 1, where n is the content length.
func mkMsg(role, content string) models.Message {
	return models.Message{Role: role, Content: content}
}

// fill returns a string of n bytes so estimateMessageTokens is deterministic.
func fill(n int) string {
	return strings.Repeat("a", n)
}

func TestEstimateMessageTokens(t *testing.T) {
	msg := models.Message{
		Role:             "assistant",
		Content:          fill(40), // 40/4 + 1 = 11
		ReasoningContent: fill(40), // +11
		ToolCalls: []models.ToolCall{
			{Function: models.ToolCallFunction{Name: "x", Arguments: fill(8)}}, // (1+8)/4 + 1 = 3
		},
	}
	// 40+40+1+8 = 89; 89/4 = 22; +1 = 23
	if got, want := estimateMessageTokens(msg), 23; got != want {
		t.Fatalf("estimateMessageTokens = %d, want %d", got, want)
	}
}

func TestTruncateMessagesDisabled(t *testing.T) {
	msgs := []models.Message{
		mkMsg("system", "sys"),
		mkMsg("user", "u1"),
		mkMsg("assistant", "a1"),
		mkMsg("user", "u2"),
		mkMsg("assistant", "a2"),
	}
	got, removed, _ := truncateMessages(msgs, 0, 0)
	if removed != 0 {
		t.Fatalf("removed = %d, want 0", removed)
	}
	if len(got) != len(msgs) {
		t.Fatalf("len = %d, want %d", len(got), len(msgs))
	}
}

func TestTruncateMessagesNoopWhenWithinLimits(t *testing.T) {
	msgs := []models.Message{
		mkMsg("system", "sys"),
		mkMsg("user", "u1"),
		mkMsg("assistant", "a1"),
		mkMsg("user", "u2"),
		mkMsg("assistant", "a2"),
	}
	got, removed, _ := truncateMessages(msgs, 100, 0)
	if removed != 0 {
		t.Fatalf("removed = %d, want 0", removed)
	}
	if len(got) != len(msgs) {
		t.Fatalf("len = %d, want %d", len(got), len(msgs))
	}
}

func TestTruncateMessagesByCountDropsOldestRounds(t *testing.T) {
	msgs := []models.Message{
		mkMsg("system", "sys"),
		mkMsg("user", "u1"),
		mkMsg("assistant", "a1"),
		mkMsg("tool", "t1"),
		mkMsg("user", "u2"),
		mkMsg("assistant", "a2"),
		mkMsg("user", "u3"),
		mkMsg("assistant", "a3"),
	}
	// 8 messages, cap at 5 -> keep system + last two rounds (u2,a2,u3,a3)? No:
	// 5 total means system + 4 = u2,a2,u3,a3.
	got, removed, removedTokens := truncateMessages(msgs, 5, 0)
	if removed != 3 {
		t.Fatalf("removed = %d, want 3", removed)
	}
	if len(got) != 5 {
		t.Fatalf("len(got) = %d, want 5", len(got))
	}
	if got[0].Role != "system" {
		t.Fatalf("system message not preserved: got role %q", got[0].Role)
	}
	// The first surviving non-system message must be a user message: cutting
	// at a round boundary means no orphaned assistant/tool messages.
	if got[1].Role != "user" || got[1].Content != "u2" {
		t.Fatalf("first kept message = %q/%q, want user/u2", got[1].Role, got[1].Content)
	}
	if removedTokens <= 0 {
		t.Fatalf("removedTokens = %d, want > 0", removedTokens)
	}
}

func TestTruncateMessagesByCountKeepsLatestRoundOnly(t *testing.T) {
	msgs := []models.Message{
		mkMsg("system", "sys"),
		mkMsg("user", "u1"),
		mkMsg("assistant", "a1"),
		mkMsg("user", "u2"),
		mkMsg("assistant", "a2"),
	}
	// Cap of 3 cannot fit system+u2+a2 (that's 3) but must never drop the
	// final round, so it keeps system + u2 + a2 exactly.
	got, removed, _ := truncateMessages(msgs, 3, 0)
	if removed != 2 {
		t.Fatalf("removed = %d, want 2", removed)
	}
	if len(got) != 3 {
		t.Fatalf("len(got) = %d, want 3", len(got))
	}
	if got[1].Content != "u2" || got[2].Content != "a2" {
		t.Fatalf("kept %q/%q, want u2/a2", got[1].Content, got[2].Content)
	}
}

func TestTruncateMessagesNeverDropsLatestRound(t *testing.T) {
	msgs := []models.Message{
		mkMsg("system", "sys"),
		mkMsg("user", "u1"),
		mkMsg("assistant", "a1"),
		mkMsg("user", fill(100000)), // lone huge final turn
	}
	// Cap absurdly low: the algorithm must refuse to cut into the last round
	// and pass the oversized turn through instead of returning nothing.
	got, removed, _ := truncateMessages(msgs, 1, 0)
	if len(got) < 2 {
		t.Fatalf("len(got) = %d, want >= 2 (system + final turn)", len(got))
	}
	last := got[len(got)-1]
	if last.Content != fill(100000) {
		t.Fatalf("final turn was dropped or altered")
	}
	if removed > 2 {
		t.Fatalf("removed = %d, want <= 2 (must not cut last round)", removed)
	}
}

func TestTruncateMessagesByTokens(t *testing.T) {
	// Each round's content is ~1000 bytes -> ~251 tokens. Four rounds plus a
	// small system message is well over a 600-token ceiling, so the oldest
	// rounds must be dropped until only the recent ones fit.
	msgs := []models.Message{
		mkMsg("system", "sys"),
		mkMsg("user", fill(1000)),
		mkMsg("assistant", fill(1000)),
		mkMsg("user", fill(1000)),
		mkMsg("assistant", fill(1000)),
		mkMsg("user", fill(1000)),
		mkMsg("assistant", fill(1000)),
	}
	got, removed, _ := truncateMessages(msgs, 0, 600)
	if removed == 0 {
		t.Fatalf("removed = 0, want > 0")
	}
	tokens := 0
	for _, m := range got {
		tokens += estimateMessageTokens(m)
	}
	if tokens > 600 {
		t.Fatalf("kept %d estimated tokens, want <= 600", tokens)
	}
	if got[0].Role != "system" {
		t.Fatalf("system message not preserved")
	}
	if got[1].Role != "user" {
		t.Fatalf("first kept non-system message = %q, want user (round boundary)", got[1].Role)
	}
}

func TestTruncateMessagesBothLimitsCountIsBinding(t *testing.T) {
	msgs := []models.Message{
		mkMsg("system", "sys"),
		mkMsg("user", "u1"),
		mkMsg("assistant", "a1"),
		mkMsg("user", "u2"),
		mkMsg("assistant", "a2"),
		mkMsg("user", "u3"),
		mkMsg("assistant", "a3"),
	}
	// Token ceiling is generous; the message-count cap must be what trims.
	//
	// Trimming happens on round boundaries, so the achievable counts are
	// 7 -> 5 -> 3 (system + N two-message rounds). A cap of 4 therefore
	// lands on 3, not 4: undershooting the cap is fine, tearing a round
	// apart is not (an orphaned tool message is rejected upstream).
	got, removed, _ := truncateMessages(msgs, 4, 1000000)
	if len(got) != 3 {
		t.Fatalf("len(got) = %d, want 3", len(got))
	}
	if removed != 4 {
		t.Fatalf("removed = %d, want 4", removed)
	}
	if got[0].Role != "system" {
		t.Fatalf("system message not preserved")
	}
	// Round boundary: the surviving tail starts at a user message.
	if got[1].Content != "u3" || got[2].Content != "a3" {
		t.Fatalf("kept %q/%q, want u3/a3", got[1].Content, got[2].Content)
	}
}

func TestTruncateMessagesSystemAlwaysFirst(t *testing.T) {
	msgs := []models.Message{
		mkMsg("system", "sys1"),
		mkMsg("system", "sys2"),
		mkMsg("user", "u1"),
		mkMsg("assistant", "a1"),
		mkMsg("user", "u2"),
		mkMsg("assistant", "a2"),
	}
	got, _, _ := truncateMessages(msgs, 4, 0)
	if len(got) != 4 {
		t.Fatalf("len(got) = %d, want 4", len(got))
	}
	if got[0].Role != "system" || got[1].Role != "system" {
		t.Fatalf("leading system messages not preserved: %q/%q", got[0].Role, got[1].Role)
	}
}

func TestTruncateMessagesNoUserMessages(t *testing.T) {
	// Degenerate history with no user turns: index 0 is the only round, and it
	// is also the last round, so nothing may be cut.
	msgs := []models.Message{
		mkMsg("system", "sys"),
		mkMsg("assistant", "a1"),
		mkMsg("assistant", "a2"),
	}
	got, removed, _ := truncateMessages(msgs, 1, 0)
	if removed != 0 {
		t.Fatalf("removed = %d, want 0", removed)
	}
	if len(got) != len(msgs) {
		t.Fatalf("len(got) = %d, want %d", len(got), len(msgs))
	}
}

func TestTruncateMessagesOnlySystemAndUser(t *testing.T) {
	msgs := []models.Message{
		mkMsg("system", "sys"),
		mkMsg("user", "u1"),
		mkMsg("user", "u2"),
	}
	// Cap of 2 keeps system + the final user turn.
	got, removed, _ := truncateMessages(msgs, 2, 0)
	if removed != 1 {
		t.Fatalf("removed = %d, want 1", removed)
	}
	if len(got) != 2 || got[1].Content != "u2" {
		t.Fatalf("kept %v, want [system u2]", got)
	}
}

func TestTruncateMessagesEmpty(t *testing.T) {
	got, removed, tokens := truncateMessages(nil, 10, 100)
	if len(got) != 0 || removed != 0 || tokens != 0 {
		t.Fatalf("empty input: got len=%d removed=%d tokens=%d, want all zero", len(got), removed, tokens)
	}
}

// TestTruncateMessagesOffPathIsIdentity locks in the kill-switch contract: when
// both limits are disabled, the very same slice must come back untouched, so a
// default configuration can never alter a request.
func TestTruncateMessagesOffPathIsIdentity(t *testing.T) {
	msgs := []models.Message{
		mkMsg("system", "sys"),
		mkMsg("user", fill(100000)),
		mkMsg("assistant", fill(100000)),
		mkMsg("user", fill(100000)),
		mkMsg("assistant", fill(100000)),
	}
	cases := []struct {
		name               string
		maxMsgs, maxTokens int
	}{
		{"both zero", 0, 0},
		{"both negative", -1, -1},
		{"count zero tokens negative", 0, -5},
		{"count negative tokens zero", -5, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, removed, tokens := truncateMessages(msgs, tc.maxMsgs, tc.maxTokens)
			if removed != 0 || tokens != 0 {
				t.Fatalf("disabled limits: removed=%d tokens=%d, want 0/0", removed, tokens)
			}
			if len(got) != len(msgs) {
				t.Fatalf("len(got) = %d, want %d (must be untouched)", len(got), len(msgs))
			}
			// Identity in the strong sense: same backing array, not a copy.
			if &got[0] != &msgs[0] {
				t.Fatalf("disabled limits returned a different slice; want the input slice unchanged")
			}
		})
	}
}

// TestTruncateMessagesNeverReturnsShorterThanLastRound guards the degradation
// path: even when the latest round alone blows every limit, the result must
// still contain it rather than collapsing to nothing.
func TestTruncateMessagesNeverReturnsShorterThanLastRound(t *testing.T) {
	msgs := []models.Message{
		mkMsg("system", "sys"),
		mkMsg("user", "u1"),
		mkMsg("assistant", "a1"),
		mkMsg("user", fill(500000)),
		mkMsg("assistant", fill(500000)),
	}
	got, _, _ := truncateMessages(msgs, 1, 1)
	if len(got) < 2 {
		t.Fatalf("len(got) = %d, want >= 2: final round must survive", len(got))
	}
	if got[len(got)-1].Content != fill(500000) {
		t.Fatalf("final assistant message was dropped or altered")
	}
	if got[0].Role != "system" {
		t.Fatalf("system message not preserved")
	}
}

func TestWithinLimits(t *testing.T) {
	cases := []struct {
		name                                       string
		keptCount, keptTokens, maxMessages, maxTok int
		want                                       bool
	}{
		{"both disabled", 100, 100, 0, 0, true},
		{"count under", 5, 100, 10, 0, true},
		{"count over", 11, 100, 10, 0, false},
		{"tokens under", 5, 100, 0, 200, true},
		{"tokens over", 5, 201, 0, 200, false},
		{"count ok tokens over", 5, 201, 10, 200, false},
		{"count over tokens ok", 11, 100, 10, 200, false},
		{"both ok", 5, 100, 10, 200, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := withinLimits(tc.keptCount, tc.keptTokens, tc.maxMessages, tc.maxTok); got != tc.want {
				t.Fatalf("withinLimits(%d, %d, %d, %d) = %v, want %v",
					tc.keptCount, tc.keptTokens, tc.maxMessages, tc.maxTok, got, tc.want)
			}
		})
	}
}
