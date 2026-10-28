package transform

import (
	"encoding/json"
	"testing"

	"github.com/mrksmt/deepseek-cursor-proxy/internal/config"
	"github.com/mrksmt/deepseek-cursor-proxy/internal/store"
)

func toolFn(name string) map[string]any {
	return map[string]any{
		"type": "function",
		"function": map[string]any{
			"name":        name,
			"description": name,
			"parameters":  map[string]any{"type": "object"},
		},
	}
}

// cursorFreeformTool is what Cursor sends for GPT-named models: DeepSeek
// rejects it with `unknown variant `custom“, and one such tool kills the
// whole request including the valid function tools next to it.
func cursorFreeformTool(name string) map[string]any {
	return map[string]any{
		"type": "custom",
		"custom": map[string]any{
			"name":        name,
			"description": name,
			"format": map[string]any{
				"type":       "grammar",
				"grammar":    "lark",
				"definition": "start: /.+/",
			},
		},
	}
}

func prepareForToolTest(t *testing.T, payload map[string]any, thinking string) map[string]any {
	t.Helper()
	cfg := config.NewDefaultConfig()
	cfg.Thinking = thinking
	cfg.MissingReasoningStrategy = "recover"
	prepared := PrepareUpstreamRequest(t.Context(), payload, cfg, nil, "Bearer test")
	return prepared.Payload
}

func basePayload(tools []any, choice any) map[string]any {
	payload := map[string]any{
		"model":    "deepseek-flash",
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	}
	if tools != nil {
		payload["tools"] = tools
	}
	if choice != nil {
		payload["tool_choice"] = choice
	}
	return payload
}

// A custom tool must be dropped while the sibling function tool survives.
func TestCustomToolIsDroppedFunctionToolKept(t *testing.T) {
	payload := basePayload([]any{cursorFreeformTool("apply_patch"), toolFn("read_file")}, "auto")
	got := prepareForToolTest(t, payload, "enabled")

	tools, ok := got["tools"].([]any)
	if !ok {
		t.Fatalf("tools missing entirely: %v", got["tools"])
	}
	if len(tools) != 1 {
		t.Fatalf("tools len = %d, want 1 (custom dropped): %v", len(tools), tools)
	}
	only := tools[0].(map[string]any)
	if only["type"] != "function" {
		t.Fatalf("surviving tool type = %v, want function", only["type"])
	}
	if name := only["function"].(map[string]any)["name"]; name != "read_file" {
		t.Fatalf("surviving tool name = %v, want read_file", name)
	}
}

// When nothing survives, both tools and tool_choice must be gone: an empty
// tools list or a dangling choice is just another rejection.
func TestAllToolsDroppedRemovesToolsAndChoice(t *testing.T) {
	payload := basePayload([]any{cursorFreeformTool("apply_patch")},
		map[string]any{"type": "function", "function": map[string]any{"name": "apply_patch"}})
	got := prepareForToolTest(t, payload, "enabled")

	if v, ok := got["tools"]; ok {
		t.Fatalf("tools should be removed, got %v", v)
	}
	if v, ok := got["tool_choice"]; ok {
		t.Fatalf("tool_choice should be removed, got %v", v)
	}
}

// Thinking mode rejects `required`: DeepSeek answers 400 "Thinking mode does
// not support this tool_choice". It must be downgraded, not forwarded.
func TestRequiredChoiceDowngradedInThinkingMode(t *testing.T) {
	payload := basePayload([]any{toolFn("read_file")}, "required")
	got := prepareForToolTest(t, payload, "enabled")

	if got["tool_choice"] != "auto" {
		t.Fatalf("tool_choice = %v, want auto", got["tool_choice"])
	}
}

// The same choice is legal with thinking disabled and must be preserved.
func TestRequiredChoiceKeptWithoutThinking(t *testing.T) {
	payload := basePayload([]any{toolFn("read_file")}, "required")
	got := prepareForToolTest(t, payload, "disabled")

	if got["tool_choice"] != "required" {
		t.Fatalf("tool_choice = %v, want required", got["tool_choice"])
	}
}

// Named choices hit the same 400 in thinking mode.
func TestNamedChoiceDowngradedInThinkingMode(t *testing.T) {
	payload := basePayload([]any{toolFn("read_file")},
		map[string]any{"type": "function", "function": map[string]any{"name": "read_file"}})
	got := prepareForToolTest(t, payload, "enabled")

	if got["tool_choice"] != "auto" {
		t.Fatalf("tool_choice = %v, want auto", got["tool_choice"])
	}
}

// A named choice pointing at a dropped tool must not be sent at all.
func TestNamedChoiceForDroppedToolBecomesAuto(t *testing.T) {
	payload := basePayload([]any{toolFn("read_file"), cursorFreeformTool("apply_patch")},
		map[string]any{"type": "function", "function": map[string]any{"name": "apply_patch"}})
	got := prepareForToolTest(t, payload, "disabled")

	if got["tool_choice"] != "auto" {
		t.Fatalf("tool_choice = %v, want auto (apply_patch was dropped)", got["tool_choice"])
	}
}

// tool_choice without any tools is meaningless upstream.
func TestToolChoiceDroppedWhenNoTools(t *testing.T) {
	payload := basePayload(nil, "required")
	got := prepareForToolTest(t, payload, "enabled")

	if v, ok := got["tool_choice"]; ok {
		t.Fatalf("tool_choice should be removed, got %v", v)
	}
}

// The legacy functions/function_call path must go through the same filtering.
func TestLegacyFunctionsFilteredAndConverted(t *testing.T) {
	payload := map[string]any{
		"model":    "deepseek-flash",
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
		"functions": []any{
			map[string]any{"name": "read_file", "description": "read", "parameters": map[string]any{"type": "object"}},
			map[string]any{"name": "", "description": "nameless"},
		},
		"function_call": "auto",
	}
	got := prepareForToolTest(t, payload, "enabled")

	tools, ok := got["tools"].([]any)
	if !ok || len(tools) != 1 {
		t.Fatalf("tools = %v, want one surviving legacy function", got["tools"])
	}
	if got["tool_choice"] != "auto" {
		t.Fatalf("tool_choice = %v, want auto", got["tool_choice"])
	}
}

// A nameless function tool is not callable and must not reach upstream.
func TestNamelessFunctionToolDropped(t *testing.T) {
	payload := basePayload([]any{
		map[string]any{"type": "function", "function": map[string]any{"name": "  "}},
	}, nil)
	got := prepareForToolTest(t, payload, "enabled")

	if v, ok := got["tools"]; ok {
		b, _ := json.Marshal(v)
		t.Fatalf("tools should be removed for nameless function, got %s", b)
	}
}

// Sanity check that the surviving tool is byte-identical in shape to what
// DeepSeek expects (type + function{name,description,parameters}).
func TestSurvivingToolShapeUnchanged(t *testing.T) {
	payload := basePayload([]any{toolFn("read_file")}, "auto")
	got := prepareForToolTest(t, payload, "enabled")

	tools := got["tools"].([]any)
	fn, ok := tools[0].(map[string]any)["function"].(map[string]any)
	if !ok {
		t.Fatalf("function block missing: %v", tools[0])
	}
	for _, key := range []string{"name", "description", "parameters"} {
		if _, ok := fn[key]; !ok {
			t.Fatalf("function.%s missing: %v", key, fn)
		}
	}
}

var _ = store.ReasoningStore{}
