package streaming

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/mrksmt/deepseek-cursor-proxy/internal/models"
	"github.com/mrksmt/deepseek-cursor-proxy/internal/store"
)

const (
	thinkingBlockStart            = "<think>\n"
	thinkingBlockEnd              = "\n</think>\n\n"
	collapsibleThinkingBlockStart = "<details>\n<summary>Thinking</summary>\n\n"
	collapsibleThinkingBlockEnd   = "\n</details>\n\n"
)

// StreamingChoice accumulates streaming data for a single choice index.
type StreamingChoice struct {
	mu               sync.RWMutex
	Role             string
	Content          string
	ReasoningContent string
	HasReasoning     bool
	ToolCalls        []map[string]any
	FinishReason     string
}

// ToMessage converts the accumulated choice to a message map.
func (sc *StreamingChoice) ToMessage() map[string]any {
	sc.mu.RLock()
	defer sc.mu.RUnlock()

	msg := map[string]any{
		"role":    sc.Role,
		"content": sc.Content,
	}
	if sc.HasReasoning {
		msg["reasoning_content"] = sc.ReasoningContent
	}
	if len(sc.ToolCalls) > 0 {
		tcs := make([]map[string]any, len(sc.ToolCalls))
		copy(tcs, sc.ToolCalls)
		msg["tool_calls"] = tcs
	}
	return msg
}

// StreamAccumulator accumulates streaming chunks.
type StreamAccumulator struct {
	mu            sync.Mutex
	choices       map[int]*StreamingChoice
	storedChoices map[string]int // "idx:scope" -> stage rank
}

// NewStreamAccumulator creates a new StreamAccumulator.
func NewStreamAccumulator() *StreamAccumulator {
	return &StreamAccumulator{
		choices:       make(map[int]*StreamingChoice),
		storedChoices: make(map[string]int),
	}
}

// IngestChunk processes a streaming chunk.
func (sa *StreamAccumulator) IngestChunk(chunk map[string]any) {
	choicesRaw, ok := chunk["choices"].([]any)
	if !ok {
		return
	}

	sa.mu.Lock()
	defer sa.mu.Unlock()

	for _, raw := range choicesRaw {
		rawChoice, ok := raw.(map[string]any)
		if !ok {
			continue
		}

		index := int(getFloat(rawChoice, "index"))
		choice, exists := sa.choices[index]
		if !exists {
			choice = &StreamingChoice{Role: "assistant"}
			sa.choices[index] = choice
		}

		if fr, ok := rawChoice["finish_reason"].(string); ok {
			choice.FinishReason = fr
		}

		delta, ok := rawChoice["delta"].(map[string]any)
		if !ok {
			continue
		}

		if role, ok := delta["role"].(string); ok && role != "" {
			choice.Role = role
		}
		if content, ok := delta["content"].(string); ok {
			choice.Content += content
		}
		if rc, ok := delta["reasoning_content"].(string); ok {
			choice.HasReasoning = true
			choice.ReasoningContent += rc
		}

		mergeToolCallDeltas(choice, delta["tool_calls"])
	}
}

// StoreReasoning stores all accumulated reasoning in the cache.
func (sa *StreamAccumulator) StoreReasoning(
	ctx context.Context,
	rs *store.ReasoningStore,
	scope, cacheNamespace string,
	priorMessages ...models.Message,
) int {
	sa.mu.Lock()
	defer sa.mu.Unlock()

	stored := 0
	for _, choice := range sa.choices {
		n, _ := rs.StoreAssistantMessage(
			ctx,
			choice.ToMessage(),
			scope,
			cacheNamespace,
			priorMessages,
		)
		stored += n
	}
	return stored
}

// StoreFinishedReasoning stores reasoning for finished choices only.
func (sa *StreamAccumulator) StoreFinishedReasoning(
	ctx context.Context,
	rs *store.ReasoningStore,
	scope, cacheNamespace string,
	priorMessages []models.Message,
) int {
	sa.mu.Lock()
	defer sa.mu.Unlock()

	stored := 0
	for _, choice := range sa.choices {
		if choice.FinishReason != "" {
			n, _ := rs.StoreAssistantMessage(
				ctx,
				choice.ToMessage(),
				scope,
				cacheNamespace,
				priorMessages,
			)
			stored += n
		}
	}
	return stored
}

// StoreReadyReasoning stores reasoning for choices that are ready (finished or have identified tool calls).
func (sa *StreamAccumulator) StoreReadyReasoning(
	ctx context.Context,
	rs *store.ReasoningStore,
	scope, cacheNamespace string,
	priorMessages []models.Message,
) int {
	sa.mu.Lock()
	defer sa.mu.Unlock()

	stored := 0
	for idx, choice := range sa.choices {
		stageRank := 0
		if choice.FinishReason != "" {
			stageRank = 2
		} else if hasIdentifiedToolCalls(choice) {
			stageRank = 1
		}
		if stageRank == 0 {
			continue
		}

		sk := fmt.Sprintf("%d:%s", idx, scope)
		prevRank := sa.storedChoices[sk]
		if prevRank >= stageRank {
			continue
		}

		n, _ := rs.StoreAssistantMessage(
			ctx,
			choice.ToMessage(),
			scope,
			cacheNamespace,
			priorMessages,
		)
		if n > 0 {
			sa.storedChoices[sk] = stageRank
			stored += n
		}
	}
	return stored
}

// Messages returns all accumulated messages.
func (sa *StreamAccumulator) Messages() []map[string]any {
	sa.mu.Lock()
	defer sa.mu.Unlock()

	indices := make([]int, 0, len(sa.choices))
	for idx := range sa.choices {
		indices = append(indices, idx)
	}

	msgs := make([]map[string]any, 0, len(indices))
	for _, idx := range indices {
		msgs = append(msgs, sa.choices[idx].ToMessage())
	}
	return msgs
}

func mergeToolCallDeltas(choice *StreamingChoice, raw any) {
	deltas, ok := raw.([]any)
	if !ok {
		return
	}

	for _, rd := range deltas {
		delta, ok := rd.(map[string]any)
		if !ok {
			continue
		}

		idx := int(getFloat(delta, "index"))
		for len(choice.ToolCalls) <= idx {
			choice.ToolCalls = append(choice.ToolCalls, map[string]any{
				"type":     "function",
				"function": map[string]any{"name": "", "arguments": ""},
			})
		}

		tc := choice.ToolCalls[idx]
		if id, ok := delta["id"].(string); ok && id != "" {
			tc["id"] = id
		}
		if t, ok := delta["type"].(string); ok && t != "" {
			tc["type"] = t
		}

		fnDelta, ok := delta["function"].(map[string]any)
		if !ok {
			continue
		}

		fn, _ := tc["function"].(map[string]any)
		if fn == nil {
			fn = map[string]any{"name": "", "arguments": ""}
			tc["function"] = fn
		}

		if name, ok := fnDelta["name"].(string); ok && name != "" {
			existing, _ := fn["name"].(string)
			fn["name"] = existing + name
		}
		if args, ok := fnDelta["arguments"].(string); ok {
			existing, _ := fn["arguments"].(string)
			fn["arguments"] = existing + args
		}
	}
}

func hasIdentifiedToolCalls(choice *StreamingChoice) bool {
	if !choice.HasReasoning || len(choice.ToolCalls) == 0 {
		return false
	}
	for _, tc := range choice.ToolCalls {
		if id, ok := tc["id"].(string); !ok || id == "" {
			return false
		}
	}
	return true
}

func getFloat(m map[string]any, key string) float64 {
	switch v := m[key].(type) {
	case float64:
		return v
	case int:
		return float64(v)
	case json.Number:
		f, _ := v.Float64()
		return f
	default:
		return 0
	}
}

// CursorReasoningDisplayAdapter mirrors reasoning_content into content for visible display.
type CursorReasoningDisplayAdapter struct {
	collapsible  bool
	openChoices  map[int]struct{}
	lastMetadata map[string]any
	blockStart   string
	blockEnd     string
}

// NewCursorReasoningDisplayAdapter creates a new display adapter.
func NewCursorReasoningDisplayAdapter(collapsible bool) *CursorReasoningDisplayAdapter {
	start := thinkingBlockStart
	end := thinkingBlockEnd
	if collapsible {
		start = collapsibleThinkingBlockStart
		end = collapsibleThinkingBlockEnd
	}
	return &CursorReasoningDisplayAdapter{
		collapsible:  collapsible,
		openChoices:  make(map[int]struct{}),
		lastMetadata: make(map[string]any),
		blockStart:   start,
		blockEnd:     end,
	}
}

// RewriteChunk rewrites a chunk to mirror reasoning into content.
func (a *CursorReasoningDisplayAdapter) RewriteChunk(chunk map[string]any) {
	// Remember metadata
	for _, key := range []string{"id", "object", "created"} {
		if v, ok := chunk[key]; ok {
			a.lastMetadata[key] = v
		}
	}

	choicesRaw, ok := chunk["choices"].([]any)
	if !ok {
		return
	}

	for _, raw := range choicesRaw {
		rawChoice, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		index := int(getFloat(rawChoice, "index"))

		delta, ok := rawChoice["delta"].(map[string]any)
		if !ok {
			delta = make(map[string]any)
			rawChoice["delta"] = delta
		}

		var mirroredParts []string

		rc, _ := delta["reasoning_content"].(string)
		if rc != "" {
			if _, open := a.openChoices[index]; !open {
				mirroredParts = append(mirroredParts, a.blockStart)
				a.openChoices[index] = struct{}{}
			}
			mirroredParts = append(mirroredParts, rc)
		}

		existingContent, _ := delta["content"].(string)
		_, hasToolCalls := delta["tool_calls"]
		finishReason, hasFinish := rawChoice["finish_reason"]

		_, isOpen := a.openChoices[index]
		shouldClose := isOpen && (existingContent != "" || hasToolCalls || (hasFinish && finishReason != nil))
		if shouldClose {
			mirroredParts = append(mirroredParts, a.blockEnd)
			delete(a.openChoices, index)
		}

		if len(mirroredParts) == 0 {
			continue
		}
		if existingContent != "" {
			mirroredParts = append(mirroredParts, existingContent)
		}
		delta["content"] = joinString(mirroredParts)
	}
}

// FlushChunk returns a closing chunk for any remaining open choices.
func (a *CursorReasoningDisplayAdapter) FlushChunk(model string) map[string]any {
	if len(a.openChoices) == 0 {
		return nil
	}

	indices := make([]int, 0, len(a.openChoices))
	for idx := range a.openChoices {
		indices = append(indices, idx)
	}
	a.openChoices = make(map[int]struct{})

	choices := make([]map[string]any, 0, len(indices))
	for _, idx := range indices {
		choices = append(choices, map[string]any{
			"index":         idx,
			"delta":         map[string]any{"content": a.blockEnd},
			"finish_reason": nil,
		})
	}

	id := "chatcmpl-reasoning-close"
	if v, ok := a.lastMetadata["id"].(string); ok {
		id = v
	}
	obj := "chat.completion.chunk"
	if v, ok := a.lastMetadata["object"].(string); ok {
		obj = v
	}
	created := time.Now().Unix()
	if v, ok := a.lastMetadata["created"].(int64); ok {
		created = v
	}

	return map[string]any{
		"id":      id,
		"object":  obj,
		"created": created,
		"model":   model,
		"choices": choices,
	}
}

func joinString(parts []string) string {
	var result strings.Builder
	for _, p := range parts {
		result.WriteString(p)
	}
	return result.String()
}

// FoldReasoningIntoContent mirrors reasoning into content for non-streaming responses.
func FoldReasoningIntoContent(response map[string]any, collapsible bool) {
	start := thinkingBlockStart
	end := thinkingBlockEnd
	if collapsible {
		start = collapsibleThinkingBlockStart
		end = collapsibleThinkingBlockEnd
	}

	choicesRaw, ok := response["choices"].([]any)
	if !ok {
		return
	}

	for _, raw := range choicesRaw {
		choice, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		msg, ok := choice["message"].(map[string]any)
		if !ok {
			continue
		}
		rc, ok := msg["reasoning_content"].(string)
		if !ok || rc == "" {
			continue
		}
		content, _ := msg["content"].(string)
		msg["content"] = start + rc + end + content
	}
}
