package transform

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"maps"
	"regexp"
	"strings"

	"go.opentelemetry.io/otel/attribute"

	"github.com/mrksmt/deepseek-cursor-proxy/internal/config"
	"github.com/mrksmt/deepseek-cursor-proxy/internal/models"
	"github.com/mrksmt/deepseek-cursor-proxy/internal/otel_ctx"
	"github.com/mrksmt/deepseek-cursor-proxy/internal/store"
	"github.com/mrksmt/deepseek-cursor-proxy/internal/streaming"
)

// context key for counting store lookups during request preparation
type storeLookupsKey struct{}

func withStoreLookupsCounter(ctx context.Context) context.Context {
	return context.WithValue(ctx, storeLookupsKey{}, new(int))
}

func getStoreLookups(ctx context.Context) int {
	if p, ok := ctx.Value(storeLookupsKey{}).(*int); ok {
		return *p
	}
	return 0
}

func incStoreLookups(ctx context.Context) {
	if p, ok := ctx.Value(storeLookupsKey{}).(*int); ok {
		*p++
	}
}

// Supported request fields that are forwarded to the upstream.
var supportedRequestFields = map[string]bool{
	"model":             true,
	"messages":          true,
	"stream":            true,
	"stream_options":    true,
	"max_tokens":        true,
	"temperature":       true,
	"top_p":             true,
	"tools":             true,
	"tool_choice":       true,
	"thinking":          true,
	"reasoning_effort":  true,
	"stop":              true,
	"response_format":   true,
	"presence_penalty":  true,
	"frequency_penalty": true,
	"logprobs":          true,
	"top_logprobs":      true,
	"user":              true,
	"seed":              true,
	"n":                 true,
	"logit_bias":        true,
}

// Allowed message fields per role.
var roleMessageFields = map[string]map[string]bool{
	"system":    {"role": true, "content": true, "name": true},
	"user":      {"role": true, "content": true, "name": true},
	"assistant": {"role": true, "content": true, "name": true, "tool_calls": true, "reasoning_content": true, "prefix": true},
	"tool":      {"role": true, "content": true, "tool_call_id": true},
}

var allMessageFields = map[string]bool{
	"role": true, "content": true, "name": true, "tool_call_id": true,
	"tool_calls": true, "reasoning_content": true, "prefix": true,
}

// Effort aliases map various effort levels to normalized values.
var effortAliases = map[string]string{
	"low":    "high",
	"medium": "high",
	"high":   "high",
	"max":    "max",
	"xhigh":  "max",
}

// Recovery notice text constants.
const (
	RecoveryNoticeText    = "[deepseek-cursor-proxy] Refreshed reasoning_content history."
	RecoveryNoticeContent = RecoveryNoticeText + "\n\n"
	RecoverySystemContent = "deepseek-cursor-proxy recovered this request because older DeepSeek " +
		"thinking-mode tool-call reasoning_content was unavailable. Older " +
		"unrecoverable tool-call history was omitted; continue using only the " +
		"remaining recovered context."
)

var cursorThinkingBlockRE = regexp.MustCompile(`(?is)(?:<(?:think|thinking)\b[^>]*>[\s\S]*?(?:</(?:think|thinking)>|\z)|<details\b[^>]*>\s*<summary\b[^>]*>\s*Thinking\s*</summary>[\s\S]*?(?:</details>|\z))\s*`)

// NormalizeReasoningEffort normalizes a reasoning effort value.
func NormalizeReasoningEffort(value string) string {
	if alias, ok := effortAliases[strings.TrimSpace(strings.ToLower(value))]; ok {
		return alias
	}
	return "high"
}

// ExtractTextContent extracts plain text from a content field.
func ExtractTextContent(content any) string {
	if content == nil {
		return ""
	}
	switch v := content.(type) {
	case string:
		return v
	case []any:
		var parts []string
		for _, item := range v {
			switch item := item.(type) {
			case string:
				parts = append(parts, item)
			case map[string]any:
				text, _ := item["text"].(string)
				if text == "" {
					text, _ = item["content"].(string)
				}
				if text != "" {
					parts = append(parts, text)
				} else if t, ok := item["type"].(string); ok {
					parts = append(parts, fmt.Sprintf("[%s omitted by DeepSeek text proxy]", t))
				}
			default:
				parts = append(parts, fmt.Sprintf("%v", item))
			}
		}
		return strings.Join(parts, "\n")
	default:
		return fmt.Sprintf("%v", content)
	}
}

// StripCursorThinkingBlocks removes thinking blocks inserted by Cursor.
func StripCursorThinkingBlocks(content string) string {
	return strings.TrimLeft(cursorThinkingBlockRE.ReplaceAllString(content, ""), "\r\n")
}

// NormalizeToolCall normalizes a tool call structure.
func NormalizeToolCall(tc map[string]any) map[string]any {
	function, _ := tc["function"].(map[string]any)
	if function == nil {
		function = make(map[string]any)
	}

	arguments := ""
	switch args := function["arguments"].(type) {
	case string:
		arguments = args
	default:
		if b, err := json.Marshal(args); err == nil {
			arguments = string(b)
		}
	}

	name, _ := function["name"].(string)
	id, _ := tc["id"].(string)
	tcType, _ := tc["type"].(string)
	if tcType == "" {
		tcType = "function"
	}

	return map[string]any{
		"id":   id,
		"type": tcType,
		"function": map[string]any{
			"name":      name,
			"arguments": arguments,
		},
	}
}

// NormalizeTool normalizes a tool definition.
func NormalizeTool(tool map[string]any) map[string]any {
	normalized := make(map[string]any)
	maps.Copy(normalized, tool)
	if _, ok := normalized["type"]; !ok {
		normalized["type"] = "function"
	}
	if _, ok := normalized["function"]; !ok {
		normalized["function"] = map[string]any{
			"name":        "",
			"description": "",
			"parameters":  map[string]any{},
		}
	}
	return normalized
}

// LegacyFunctionToTool converts a legacy function definition to a tool.
func LegacyFunctionToTool(function map[string]any) map[string]any {
	return map[string]any{
		"type":     "function",
		"function": function,
	}
}

// NormalizeToolChoice normalizes a tool_choice value.
func NormalizeToolChoice(toolChoice any) any {
	switch v := toolChoice.(type) {
	case string:
		if v == "auto" || v == "none" || v == "required" {
			return v
		}
		return nil
	case map[string]any:
		if v["type"] == "function" {
			if fn, ok := v["function"].(map[string]any); ok {
				if name, ok := fn["name"].(string); ok && name != "" {
					return map[string]any{
						"type": "function",
						"function": map[string]any{
							"name": name,
						},
					}
				}
			}
		}
		return v
	}
	return toolChoice
}

// ConvertFunctionCall converts a legacy function_call to tool_choice.
func ConvertFunctionCall(functionCall any) any {
	switch v := functionCall.(type) {
	case string:
		if v == "auto" || v == "none" || v == "required" {
			return v
		}
		return nil
	case map[string]any:
		if name, ok := v["name"].(string); ok && name != "" {
			return map[string]any{
				"type": "function",
				"function": map[string]any{
					"name": name,
				},
			}
		}
		return nil
	}
	return nil
}

// PrepareUpstreamRequest transforms a Cursor request into an upstream request.
func PrepareUpstreamRequest(
	ctx context.Context,
	payload map[string]any,
	cfg *config.Config,
	rs *store.ReasoningStore,
	authorization string,
) *models.PreparedRequest {

	// Initialize store lookups counter in context
	ctx = withStoreLookupsCounter(ctx)

	originalModel, _ := payload["model"].(string)
	if originalModel == "" {
		originalModel = cfg.UpstreamModel
	}

	upstreamModel := upstreamModelFor(originalModel, cfg)

	// Filter supported fields
	prepared := make(map[string]any)
	for key, value := range payload {
		if supportedRequestFields[key] {
			prepared[key] = value
		}
	}

	// Handle max_completion_tokens
	if _, ok := prepared["max_tokens"]; !ok {
		if mct, ok := payload["max_completion_tokens"]; ok {
			prepared["max_tokens"] = mct
		}
	}

	prepared["model"] = upstreamModel

	// Stream options
	if stream, _ := prepared["stream"].(bool); stream {
		streamOpts, _ := prepared["stream_options"].(map[string]any)
		if streamOpts == nil {
			streamOpts = make(map[string]any)
		}
		streamOpts["include_usage"] = true
		prepared["stream_options"] = streamOpts
	}

	// Normalize tools
	if tools, ok := prepared["tools"].([]any); ok {
		normalizedTools := make([]any, 0, len(tools))
		for _, t := range tools {
			if tMap, ok := t.(map[string]any); ok {
				normalizedTools = append(normalizedTools, NormalizeTool(tMap))
			}
		}
		prepared["tools"] = normalizedTools
	} else if functions, ok := payload["functions"].([]any); ok {
		tools := make([]any, 0, len(functions))
		for _, f := range functions {
			if fMap, ok := f.(map[string]any); ok {
				tools = append(tools, LegacyFunctionToTool(fMap))
			}
		}
		prepared["tools"] = tools
	}

	// Normalize tool_choice
	if tc, ok := prepared["tool_choice"]; ok {
		if normalized := NormalizeToolChoice(tc); normalized != nil {
			prepared["tool_choice"] = normalized
		} else {
			delete(prepared, "tool_choice")
		}
	} else if fc, ok := payload["function_call"]; ok {
		if normalized := ConvertFunctionCall(fc); normalized != nil {
			prepared["tool_choice"] = normalized
		}
	}

	// Thinking config
	prepared["thinking"] = map[string]any{
		"type": cfg.Thinking,
	}
	thinkingEnabled := cfg.Thinking == "enabled"
	thinkingDisabled := cfg.Thinking == "disabled"
	if thinkingEnabled {
		prepared["reasoning_effort"] = NormalizeReasoningEffort(cfg.ReasoningEffort)
	}

	// Compute cache namespace
	cacheNamespace := store.ComputeReasoningCacheNamespace(
		cfg.UpstreamBaseURL,
		upstreamModel,
		cfg.Thinking,
		cfg.ReasoningEffort,
		authorization,
	)

	// Pre-repair normalization
	rawMessages, _ := payload["messages"].([]any)
	preRepairMessages, _, _, _ := normalizeMessages(ctx, nil, cacheNamespace, false, !thinkingDisabled, rawMessages...)

	recordResponseMessages := preRepairMessages
	recordResponseScope := conversationScopeFromMessages(preRepairMessages, cacheNamespace)
	currentRaw := messagesToRaw(preRepairMessages)

	continuedRecoveryBoundary := false
	retiredPrefixMessages := 0
	recoveredCount := 0
	recoveryDropped := 0
	recoveryNotice := ""
	var recoverySteps []models.RecoveryStep

	// Check for existing recovery boundary
	if thinkingEnabled && cfg.MissingReasoningStrategy == "recover" {
		if boundary := activeMessagesFromRecoveryBoundary(preRepairMessages); boundary != nil {
			currentRaw = messagesToRaw(boundary.messages)
			retiredPrefixMessages = boundary.retiredMessages
			continuedRecoveryBoundary = true
			recoverySteps = append(recoverySteps, boundary.step)
		}
	}

	// Main normalization with reasoning repair
	messages, patchedCount, missingIndexes, diagnostics := normalizeMessages(
		ctx,
		rs,
		cacheNamespace,
		thinkingEnabled,
		!thinkingDisabled,
		currentRaw,
	)

	// Recovery loop — after recovery, convert back to raw for re-normalization
	recoveryLoopCtx, recoveryLoopSpan := otel_ctx.Tracer(ctx).Start(ctx, "transform.recoveryLoop")
	recoveryIterations := 0
	for len(missingIndexes) > 0 && cfg.MissingReasoningStrategy == "recover" {
		recovered, dropped, notice, step := recoverMessagesFromMissingReasoning(messages, missingIndexes)
		recoverySteps = append(recoverySteps, step)
		if dropped == 0 {
			break
		}
		recoveryIterations++
		recoveredCount += len(missingIndexes)
		recoveryDropped += dropped
		if notice != "" {
			recoveryNotice = notice
		}
		recoveredRaw := messagesToRaw(recovered)
		var latestDiags []models.ReasoningDiagnostic
		messages, patchedCount, missingIndexes, latestDiags = normalizeMessages(
			recoveryLoopCtx,
			rs,
			cacheNamespace,
			thinkingEnabled,
			!thinkingDisabled,
			recoveredRaw...,
		)
		diagnostics = append(diagnostics, latestDiags...)
	}
	if recoveryLoopSpan.IsRecording() {
		recoveryLoopSpan.SetAttributes(
			attribute.Int("recovery.iterations", recoveryIterations),
			attribute.Int("recovery.recovered", recoveredCount),
			attribute.Int("recovery.dropped", recoveryDropped),
		)
	}
	recoveryLoopSpan.End()

	activeRecordScope := conversationScopeFromMessages(messages, cacheNamespace)
	recordContexts := responseRecordingContexts(
		&models.ResponseContext{Scope: recordResponseScope, Messages: recordResponseMessages},
		&models.ResponseContext{Scope: activeRecordScope, Messages: messages},
	)

	// Strip recovery notice for upstream
	prepared["messages"] = stripRecoveryNoticeForUpstream(messages)

	return &models.PreparedRequest{
		Payload:                    prepared,
		OriginalModel:              originalModel,
		UpstreamModel:              upstreamModel,
		CacheNamespace:             cacheNamespace,
		PatchedReasoningMessages:   patchedCount,
		MissingReasoningMessages:   len(missingIndexes),
		RecoveredReasoningMessages: recoveredCount,
		RecoveryDroppedMessages:    recoveryDropped,
		RecoveryNotice:             recoveryNotice,
		RecordResponseScope:        recordResponseScope,
		RecordResponseMessages:     recordResponseMessages,
		RecordResponseContexts:     recordContexts,
		ReasoningDiagnostics:       diagnostics,
		RecoverySteps:              recoverySteps,
		ContinuedRecoveryBoundary:  continuedRecoveryBoundary,
		RetiredPrefixMessages:      retiredPrefixMessages,
		StoreLookups:               getStoreLookups(ctx),
	}
}

// RecordResponseReasoning stores reasoning from a response into the cache.
func RecordResponseReasoning(
	ctx context.Context,
	response map[string]any,
	rs *store.ReasoningStore,
	requestMessages []models.Message,
	cacheNamespace string,
	scope string,
	priorMessages []models.Message,
	contexts []models.ResponseContext,
) int {
	if rs == nil {
		return 0
	}

	choicesRaw, ok := response["choices"].([]any)
	if !ok {
		return 0
	}

	if contexts == nil {
		responseScope := scope
		if responseScope == "" {
			responseScope = conversationScopeFromMessages(requestMessages, cacheNamespace)
		}
		responsePrior := priorMessages
		if responsePrior == nil {
			responsePrior = requestMessages
		}
		contexts = []models.ResponseContext{
			{Scope: responseScope, Messages: responsePrior},
		}
	}

	stored := 0
	for _, raw := range choicesRaw {
		choice, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		msg, ok := choice["message"].(map[string]any)
		if !ok {
			continue
		}
		for _, rc := range contexts {
			n, _ := rs.StoreAssistantMessage(ctx, msg, rc.Scope, cacheNamespace, rc.Messages)
			stored += n
		}
	}
	return stored
}

// RewriteResponseBody rewrites a non-streaming upstream response.
func RewriteResponseBody(
	ctx context.Context,
	body []byte,
	originalModel string,
	rs *store.ReasoningStore,
	requestMessages []models.Message,
	cacheNamespace string,
	contentPrefix string,
	scope string,
	priorMessages []models.Message,
	contexts []models.ResponseContext,
	displayReasoning bool,
	collapsibleReasoning bool,
) ([]byte, error) {
	var response map[string]any
	if err := json.Unmarshal(body, &response); err != nil {
		return body, nil // return original on error
	}

	if contentPrefix != "" {
		prefixResponseContent(response, contentPrefix)
	}

	RecordResponseReasoning(ctx, response, rs, requestMessages, cacheNamespace, scope, priorMessages, contexts)

	if displayReasoning {
		streaming.FoldReasoningIntoContent(response, collapsibleReasoning)
	}

	if model, ok := response["model"].(string); ok && model != "" {
		response["model"] = originalModel
	}

	return json.Marshal(response)
}

// SSE helpers
// SSEEncode encodes a payload as an SSE data line.
func SSEEncode(payload map[string]any) ([]byte, error) {
	data, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	return append(append([]byte("data: "), data...), '\n', '\n'), nil
}

// SSEDone returns the SSE [DONE] terminator.
func SSEDone() []byte {
	return []byte("data: [DONE]\n\n")
}

// InjectRecoveryNotice injects a recovery notice into the first content-bearing chunk.
func InjectRecoveryNotice(chunk map[string]any, notice string) bool {
	choicesRaw, ok := chunk["choices"].([]any)
	if !ok {
		return false
	}

	for _, raw := range choicesRaw {
		choice, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		delta, ok := choice["delta"].(map[string]any)
		if !ok {
			continue
		}

		_, hasContent := delta["content"]
		_, hasToolCalls := delta["tool_calls"]
		if !hasContent && !hasToolCalls {
			continue
		}

		existing, _ := delta["content"].(string)
		delta["content"] = notice + existing
		return true
	}
	return false
}

// RecoveryNoticeChunk creates an SSE chunk for the recovery notice.
func RecoveryNoticeChunk(model string) map[string]any {
	return map[string]any{
		"id":      "chatcmpl-deepseek-cursor-proxy-recovery",
		"object":  "chat.completion.chunk",
		"created": timeNow(),
		"model":   model,
		"choices": []any{
			map[string]any{
				"index": 0,
				"delta": map[string]any{
					"content": RecoveryNoticeContent,
				},
				"finish_reason": nil,
			},
		},
	}
}

// ---------------------------------------------------------------------------
// Internal helpers
// ---------------------------------------------------------------------------

func timeNow() int64 {
	return timeNowUnix()
}

var timeNowUnix = func() int64 {
	return 0 // replaced at startup
}

// SetTimeNow sets the time function (for testing).
func SetTimeNow(fn func() int64) {
	timeNowUnix = fn
}

func upstreamModelFor(model string, cfg *config.Config) string {
	if strings.HasPrefix(model, "deepseek-") {
		return model
	}
	return cfg.UpstreamModel
}

func conversationScopeFromMessages(messages []models.Message, namespace string) string {
	if len(messages) == 0 {
		return ""
	}
	scopeMsgs := make([]map[string]any, 0, len(messages))
	for _, msg := range messages {
		cm := map[string]any{
			"role": msg.Role,
		}
		if msg.Content != "" {
			cm["content"] = msg.Content
		}
		if msg.Name != "" {
			cm["name"] = msg.Name
		}
		if len(msg.ToolCalls) > 0 {
			tcs := make([]map[string]any, 0, len(msg.ToolCalls))
			for _, tc := range msg.ToolCalls {
				tcs = append(tcs, map[string]any{
					"id":   tc.ID,
					"type": tc.Type,
					"function": map[string]any{
						"name":      tc.Function.Name,
						"arguments": tc.Function.Arguments,
					},
				})
			}
			cm["tool_calls"] = tcs
		}
		scopeMsgs = append(scopeMsgs, cm)
	}

	var payload any = scopeMsgs
	if namespace != "" {
		payload = map[string]any{
			"namespace": namespace,
			"messages":  scopeMsgs,
		}
	}
	canonical, _ := json.Marshal(payload)
	return sha256Hex(string(canonical))
}

func sha256Hex(data string) string {
	h := sha256.Sum256([]byte(data))
	return fmt.Sprintf("%x", h)
}

func normalizeMessage(
	ctx context.Context,
	raw any,
	rs *store.ReasoningStore,
	cacheNamespace string,
	repairReasoning bool,
	keepReasoning bool,
) (
	normalized models.Message,
	patched bool,
	missing bool,
	diagnostic *models.ReasoningDiagnostic,
) {
	msg, ok := raw.(map[string]any)
	if !ok {
		normalized = models.Message{Role: "user", Content: fmt.Sprintf("%v", raw)}
		return
	}

	normalized = models.Message{}
	for key, value := range msg {
		if allMessageFields[key] {
			switch key {
			case "role":
				if v, ok := value.(string); ok {
					normalized.Role = v
				}
			case "content":
				normalized.Content = ExtractTextContent(value)
			case "name":
				if v, ok := value.(string); ok {
					normalized.Name = v
				}
			case "tool_call_id":
				if v, ok := value.(string); ok {
					normalized.ToolCallID = v
				}
			case "prefix":
				if v, ok := value.(string); ok {
					normalized.Prefix = v
				}
			case "tool_calls":
				if tcs, ok := value.([]any); ok {
					for _, tc := range tcs {
						if tcMap, ok := tc.(map[string]any); ok {
							normalized.ToolCalls = append(normalized.ToolCalls, tcMapToStruct(NormalizeToolCall(tcMap)))
						}
					}
				}
			case "reasoning_content":
				if v, ok := value.(string); ok {
					normalized.ReasoningContent = v
				}
			}
		}
	}

	if normalized.Role == "" {
		normalized.Role = "user"
	}
	if normalized.Role == "function" {
		normalized.Role = "tool"
	}

	if normalized.Content == "" && (normalized.Role == "assistant" || normalized.Role == "tool" || normalized.Role == "system" || normalized.Role == "user") {
		normalized.Content = ""
	}

	if normalized.Role == "assistant" {
		normalized.Content = StripCursorThinkingBlocks(normalized.Content)
	}

	patched = false
	missing = false
	diagnostic = nil

	if normalized.Role == "assistant" {
		if !keepReasoning {
			normalized.ReasoningContent = ""
		} else if repairReasoning {
			if normalized.ReasoningContent == "" || !keepReasoning {
				normalized.ReasoningContent = ""
				// Check if reasoning is needed (tool context)
				needsReasoning := assistantNeedsReasoningForToolContext(normalized, nil)

				if needsReasoning && rs != nil {
					// Build lookup keys and search
					scope := conversationScopeFromMessages(nil, cacheNamespace)
					// Try to find cached reasoning
					msgMap := messageToMap(normalized)
					incStoreLookups(ctx)
					if cached, err := rs.LookupForMessage(ctx, msgMap, scope, cacheNamespace, nil); err == nil && cached != "" {
						normalized.ReasoningContent = cached
						patched = true
						// Backfill portable (namespace-scoped) keys so this cache hit
						// is also available in other conversations with the same context.
						rs.BackfillPortableAliases(ctx, msgMap, cached, cacheNamespace, nil)
					}
				}

				if needsReasoning && !patched {
					missing = true
				}
			}
		}
	}

	// Apply role-specific field filtering
	allowedFields, ok := roleMessageFields[normalized.Role]
	if !ok {
		allowedFields = allMessageFields
	}
	_ = allowedFields

	return
}

func tcMapToStruct(tc map[string]any) models.ToolCall {
	toStruct := models.ToolCall{
		ID:   getString(tc, "id"),
		Type: getString(tc, "type"),
	}
	if fn, ok := tc["function"].(map[string]any); ok {
		toStruct.Function = models.ToolCallFunction{
			Name:      getString(fn, "name"),
			Arguments: getString(fn, "arguments"),
		}
	}
	return toStruct
}

func getString(m map[string]any, key string) string {
	if v, ok := m[key].(string); ok {
		return v
	}
	return ""
}

func messageToMap(msg models.Message) map[string]any {
	m := map[string]any{
		"role":    msg.Role,
		"content": msg.Content,
	}
	if msg.ReasoningContent != "" {
		m["reasoning_content"] = msg.ReasoningContent
	}
	if len(msg.ToolCalls) > 0 {
		tcs := make([]any, 0, len(msg.ToolCalls))
		for _, tc := range msg.ToolCalls {
			tcs = append(tcs, map[string]any{
				"id":   tc.ID,
				"type": tc.Type,
				"function": map[string]any{
					"name":      tc.Function.Name,
					"arguments": tc.Function.Arguments,
				},
			})
		}
		m["tool_calls"] = tcs
	}
	return m
}

func normalizeMessages(
	ctx context.Context,
	rs *store.ReasoningStore,
	cacheNamespace string,
	repairReasoning bool,
	keepReasoning bool,
	rawMessages ...any,
) (
	messages []models.Message,
	patchedCount int, missingIndexes []int,
	diagnostics []models.ReasoningDiagnostic,
) {

	ctx, span := otel_ctx.Tracer(ctx).Start(ctx, "transform.normalizeMessages")
	defer span.End()

	if len(rawMessages) == 0 {
		return
	}

	for _, raw := range rawMessages {
		normalized, patched, missing, diag := normalizeMessage(
			ctx,
			raw, rs,
			cacheNamespace,
			repairReasoning,
			keepReasoning,
		)
		messages = append(messages, normalized)
		if patched {
			patchedCount++
		}
		if missing {
			missingIndexes = append(missingIndexes, len(messages)-1)
		}
		if diag != nil {
			diagnostics = append(diagnostics, *diag)
		}
	}

	return
}

// messagesToRaw converts structured messages back to raw interface{} slice.
func messagesToRaw(messages []models.Message) []any {
	raw := make([]any, len(messages))
	for i, msg := range messages {
		raw[i] = messageToMap(msg)
	}
	return raw
}

func hasRecoveryNotice(msg models.Message) bool {
	return msg.Role == "assistant" && strings.HasPrefix(msg.Content, RecoveryNoticeText)
}

func stripRecoveryNoticeForUpstream(messages []models.Message) []any {
	stripped := make([]any, 0, len(messages))
	for _, msg := range messages {
		if msg.Role != "assistant" || !strings.HasPrefix(msg.Content, RecoveryNoticeText) {
			stripped = append(stripped, messageToMap(msg))
			continue
		}
		cleaned := msg
		cleaned.Content = strings.TrimLeft(msg.Content[len(RecoveryNoticeText):], "\r\n")
		stripped = append(stripped, messageToMap(cleaned))
	}
	return stripped
}

func leadingSystemMessages(messages []models.Message) []models.Message {
	var leading []models.Message
	for _, msg := range messages {
		if msg.Role == "system" {
			leading = append(leading, msg)
			continue
		}
		break
	}
	return leading
}

type recoveryBoundaryResult struct {
	messages        []models.Message
	retiredMessages int
	step            models.RecoveryStep
}

func activeMessagesFromRecoveryBoundary(messages []models.Message) *recoveryBoundaryResult {
	recoveryBoundaryIdx := -1
	for i := len(messages) - 1; i >= 0; i-- {
		if hasRecoveryNotice(messages[i]) {
			recoveryBoundaryIdx = i
			break
		}
	}
	if recoveryBoundaryIdx == -1 {
		return nil
	}

	userIdx := -1
	for i := recoveryBoundaryIdx - 1; i >= 0; i-- {
		if messages[i].Role == "user" {
			userIdx = i
			break
		}
	}

	leadingMsgs := leadingSystemMessages(messages)
	var recoveredTail []models.Message
	if userIdx != -1 {
		recoveredTail = append(recoveredTail, messages[userIdx])
	}
	recoveredTail = append(recoveredTail, messages[recoveryBoundaryIdx:]...)

	activeMsgs := make([]models.Message, 0, len(leadingMsgs)+1+len(recoveredTail))
	activeMsgs = append(activeMsgs, leadingMsgs...)
	activeMsgs = append(activeMsgs, models.Message{Role: "system", Content: RecoverySystemContent})
	activeMsgs = append(activeMsgs, recoveredTail...)

	keptContext := 0
	if userIdx != -1 {
		keptContext = 1
	}
	retired := max(recoveryBoundaryIdx-len(leadingMsgs)-keptContext, 0)

	return &recoveryBoundaryResult{
		messages:        activeMsgs,
		retiredMessages: retired,
		step: models.RecoveryStep{
			Strategy:              "continued_recovery_boundary",
			RecoveryBoundaryIndex: recoveryBoundaryIdx,
			ContextUserIndex:      userIdx,
			DroppedMessages:       retired,
		},
	}
}

func recoverMessagesFromMissingReasoning(
	messages []models.Message,
	missingIndexes []int,
) ([]models.Message, int, string, models.RecoveryStep) {
	// Check for existing recovery boundary
	recoveryBoundaryIdx := -1
	for i := len(messages) - 1; i >= 0; i-- {
		if hasRecoveryNotice(messages[i]) {
			// Check if any missing message is before this boundary
			for _, mi := range missingIndexes {
				if mi < i {
					recoveryBoundaryIdx = i
					break
				}
			}
			if recoveryBoundaryIdx != -1 {
				break
			}
		}
	}

	if recoveryBoundaryIdx != -1 {
		userIdx := -1
		for i := recoveryBoundaryIdx - 1; i >= 0; i-- {
			if messages[i].Role == "user" {
				userIdx = i
				break
			}
		}

		leadingMsgs := leadingSystemMessages(messages)
		var recoveredTail []models.Message
		if userIdx != -1 {
			recoveredTail = append(recoveredTail, messages[userIdx])
		}
		recoveredTail = append(recoveredTail, messages[recoveryBoundaryIdx:]...)

		recovered := make([]models.Message, 0, len(leadingMsgs)+1+len(recoveredTail))
		recovered = append(recovered, leadingMsgs...)
		recovered = append(recovered, models.Message{Role: "system", Content: RecoverySystemContent})
		recovered = append(recovered, recoveredTail...)

		keptContext := 0
		if userIdx != -1 {
			keptContext = 1
		}
		omitted := max(recoveryBoundaryIdx-len(leadingMsgs)-keptContext, 0)

		return recovered, omitted, "", models.RecoveryStep{
			Strategy:              "recovery_boundary",
			MissingIndexes:        missingIndexes,
			RecoveryBoundaryIndex: recoveryBoundaryIdx,
			ContextUserIndex:      userIdx,
			DroppedMessages:       omitted,
		}
	}

	// Find last user message
	lastUserIdx := -1
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role == "user" {
			lastUserIdx = i
			break
		}
	}

	if lastUserIdx == -1 {
		return messages, 0, "", models.RecoveryStep{
			Strategy:        "none",
			MissingIndexes:  missingIndexes,
			DroppedMessages: 0,
		}
	}

	leadingMsgs := leadingSystemMessages(messages)
	omitted := len(messages) - len(leadingMsgs) - 1
	recovered := make([]models.Message, 0, len(leadingMsgs)+2)
	recovered = append(recovered, leadingMsgs...)
	recovered = append(recovered, models.Message{Role: "system", Content: RecoverySystemContent})
	recovered = append(recovered, messages[lastUserIdx])

	return recovered, omitted, RecoveryNoticeContent, models.RecoveryStep{
		Strategy:        "latest_user",
		MissingIndexes:  missingIndexes,
		DroppedMessages: omitted,
		Notice:          RecoveryNoticeContent,
	}
}

func assistantNeedsReasoningForToolContext(msg models.Message, priorMessages []models.Message) bool {
	if len(msg.ToolCalls) > 0 {
		return true
	}

	msgs := priorMessages
	if msgs == nil {
		msgs = []models.Message{}
	}

	for i := len(msgs) - 1; i >= 0; i-- {
		role := msgs[i].Role
		if role == "tool" {
			return true
		}
		if role == "user" || role == "system" {
			return false
		}
	}
	return false
}

func prefixResponseContent(response map[string]any, prefix string) bool {
	choicesRaw, ok := response["choices"].([]any)
	if !ok {
		return false
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
		content, _ := msg["content"].(string)
		msg["content"] = prefix + content
		return true
	}
	return false
}

func responseRecordingContexts(ctxs ...*models.ResponseContext) []models.ResponseContext {
	var result []models.ResponseContext
	seen := make(map[string]bool)
	for _, ctx := range ctxs {
		if ctx == nil {
			continue
		}
		if seen[ctx.Scope] {
			continue
		}
		seen[ctx.Scope] = true
		result = append(result, *ctx)
	}
	return result
}
