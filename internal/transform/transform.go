package transform

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strconv"
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
	"low":  "low",
	"high": "high",
	"max":  "max",
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

// ParseModelSuffixes extracts a reasoning effort, a thinking toggle and a
// history cap encoded as model name suffixes, e.g. "deepseek-flash:max",
// "deepseek-flash:low:nothink" or "deepseek-flash:nothink:mm200".
//
// Suffixes are named and order-independent, following the convention used by
// OpenAI-compatible routers such as OpenRouter ("model:nitro:exacto"). Every
// token is inspected on its own, so the position of a token never matters:
// "low:nothink:mm200" and "mm200:low:nothink" are equivalent.
//
// The mmN token overrides the history cap (max_messages) for this request
// only. It exists because Cursor sends nothing but a model name — there is no
// other channel to vary the cap per chat without editing config and
// restarting the proxy.
//
// Known suffixes are stripped from the returned model name; the base model is
// returned unchanged when a suffix is not recognized.
func ParseModelSuffixes(model string) (base string, effort string, noThink bool, maxMessages int) {
	parts := strings.Split(model, ":")
	base = parts[0]

	// Only strip suffixes when every part after the base is recognized; an
	// unknown suffix is left intact so the model is routed upstream as-is.
	// This keeps names like "deepseek-v4-pro-0528" or a real upstream model
	// with a colon in it from being silently rewritten.
	if len(parts) > 1 {
		for _, p := range parts[1:] {
			if !strings.EqualFold(p, "nothink") && !isKnownEffort(p) && !isMaxMessagesToken(p) {
				return model, "", false, 0
			}
		}
		for _, p := range parts[1:] {
			switch {
			case strings.EqualFold(p, "nothink"):
				noThink = true
			case isMaxMessagesToken(p):
				maxMessages = parseMaxMessagesToken(p)
			case isKnownEffort(p):
				effort = p
			}
		}
	}
	return base, effort, noThink, maxMessages
}

// isMaxMessagesToken reports whether a model suffix token is a history-cap
// override. Only the explicit "mm" form is accepted ("mm200", "mm0"). A bare
// number is deliberately rejected: ":120" is ambiguous (model version? cap?)
// and would silently rewrite odd-but-legitimate model names.
func isMaxMessagesToken(token string) bool {
	t := strings.TrimSpace(token)
	if len(t) < 3 {
		return false
	}
	if (t[0] != 'm' && t[0] != 'M') || (t[1] != 'm' && t[1] != 'M') {
		return false
	}
	for _, r := range t[2:] {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// parseMaxMessagesToken parses a validated history-cap token. Callers must
// check isMaxMessagesToken first. Zero disables the cap (no truncation).
func parseMaxMessagesToken(token string) int {
	n, err := strconv.Atoi(strings.TrimSpace(token)[2:])
	if err != nil || n < 0 {
		return 0
	}
	return n
}

// ParseEffortFromModel extracts only the reasoning effort suffix from a model
// name (e.g. "deepseek-flash:max"), stripping it from the returned name.
func ParseEffortFromModel(model string) (string, string) {
	base, effort, _, _ := ParseModelSuffixes(model)
	return base, effort
}

func isKnownEffort(value string) bool {
	_, ok := effortAliases[strings.TrimSpace(strings.ToLower(value))]
	return ok
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

	clientModel, _ := payload["model"].(string)
	if clientModel == "" {
		clientModel = cfg.UpstreamModel
	}

	// A reasoning effort, thinking toggle and per-request history cap may be
	// encoded in the model name suffix (e.g. "deepseek-flash:max",
	// "deepseek-flash:low:nothink", "deepseek-flash:low:nothink:120").
	originalModel, modelEffort, modelNoThink, modelMaxMessages := ParseModelSuffixes(clientModel)

	// The numeric suffix overrides max_messages for this request only; 0
	// means "no suffix given" and falls back to the configured value.
	effectiveMaxMessages := cfg.MaxMessages
	if modelMaxMessages > 0 {
		effectiveMaxMessages = modelMaxMessages
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

	// Thinking config. A :nothink suffix forces thinking disabled regardless
	// of the configured default so simple tasks answer without reasoning.
	thinking := cfg.Thinking
	if modelNoThink {
		thinking = "disabled"
	}
	prepared["thinking"] = map[string]any{
		"type": thinking,
	}
	thinkingEnabled := thinking == "enabled"
	thinkingDisabled := thinking == "disabled"
	effort := cfg.ReasoningEffort
	if modelEffort != "" {
		effort = modelEffort
	}
	upstreamEffort := ""
	requestedEffort := modelEffort
	if thinkingEnabled {
		upstreamEffort = NormalizeReasoningEffort(effort)
		prepared["reasoning_effort"] = upstreamEffort
	}

	// Compute cache namespace
	cacheNamespace := store.ComputeReasoningCacheNamespace(
		cfg.UpstreamBaseURL,
		upstreamModel,
		thinking,
		effort,
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

	// Main normalization with reasoning repair.
	// currentRaw MUST be unpacked (currentRaw...): normalizeMessages takes
	// variadic any, and passing the slice as a single argument collapses the
	// entire history into one "user" message (fmt.Sprintf of the []any).
	// That is exactly what produced truncate.before=1 / upstream_count=1
	// while messages.count still reported the raw Cursor length (244+).
	messages, patchedCount, missingIndexes, diagnostics := normalizeMessages(
		ctx,
		rs,
		cacheNamespace,
		thinkingEnabled,
		!thinkingDisabled,
		currentRaw...,
	)

	// Recovery loop — after recovery, convert back to raw for re-normalization.
	// Skip entirely when thinking is disabled (:nothink): upstream returns no
	// reasoning_content, so "recover missing reasoning" cannot succeed and
	// would only shrink the conversation to the latest user turn.
	recoveryLoopCtx, recoveryLoopSpan := otel_ctx.Tracer(ctx).Start(ctx, "transform.recoveryLoop")
	recoveryIterations := 0
	for len(missingIndexes) > 0 && thinkingEnabled && cfg.MissingReasoningStrategy == "recover" {
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

	// Proactive truncation. Cursor's own context window (200k-300k) fills up
	// from accumulated history long before DeepSeek's does, and when it fills
	// Cursor silently self-summarizes and the agent forgets context. Trim the
	// oldest rounds here so the request stays under the limit we control.
	debugTruncBefore := len(messages)
	messages, truncated, truncatedTokens := truncateMessages(messages, effectiveMaxMessages, cfg.MaxPromptTokens)
	debugTruncAfter := len(messages)
	if debugTruncSpan := otel_ctx.Tracer(ctx); debugTruncSpan != nil {
		if _, span := debugTruncSpan.Start(ctx, "transform.truncateMessages"); span.IsRecording() {
			span.SetAttributes(
				attribute.Int("truncate.max_messages", effectiveMaxMessages),
				attribute.Int("truncate.max_prompt_tokens", cfg.MaxPromptTokens),
				attribute.Int("truncate.before", debugTruncBefore),
				attribute.Int("truncate.after", debugTruncAfter),
				attribute.Int("truncate.removed", truncated),
				attribute.Int("truncate.removed_tokens", truncatedTokens),
			)
			span.End()
		}
	}
	if truncated > 0 && thinkingEnabled && cfg.MissingReasoningStrategy == "recover" {
		// Re-derive the recovery boundary view after truncation so a stale
		// boundary pointing at dropped history is not carried forward.
		if boundary := activeMessagesFromRecoveryBoundary(messages); boundary != nil {
			messages = boundary.messages
			retiredPrefixMessages += boundary.retiredMessages
			recoverySteps = append(recoverySteps, boundary.step)
		}
	}

	activeRecordScope := conversationScopeFromMessages(messages, cacheNamespace)
	recordContexts := responseRecordingContexts(
		&models.ResponseContext{Scope: recordResponseScope, Messages: recordResponseMessages},
		&models.ResponseContext{Scope: activeRecordScope, Messages: messages},
	)

	// Strip recovery notice for upstream
	prepared["messages"] = stripRecoveryNoticeForUpstream(messages)

	// Never re-show the notice once this conversation already recovered —
	// continued boundary (or a preserved marker in history) means the user
	// already saw it; injecting again every turn is pure spam in Cursor.
	if continuedRecoveryBoundary || messagesContainRecoveryNotice(messages) {
		recoveryNotice = ""
	}

	return &models.PreparedRequest{
		Payload:                    prepared,
		OriginalModel:              clientModel,
		UpstreamModel:              upstreamModel,
		UpstreamEffort:             upstreamEffort,
		UpstreamThinking:           thinking,
		RequestedEffort:            requestedEffort,
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
		TruncatedMessages:          truncated,
		TruncatedEstimatedTokens:   truncatedTokens,
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
	priorMessages []models.Message,
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
		// Cursor often wraps prior assistant content (including our recovery
		// notice) in a Thinking <details> block when resending history. If we
		// strip that block wholesale, hasRecoveryNotice goes false and every
		// subsequent turn re-runs latest_user recovery + re-injects the notice
		// into the SSE stream. Preserve a marker across the strip.
		hadRecoveryNotice := strings.Contains(normalized.Content, RecoveryNoticeText)
		normalized.Content = StripCursorThinkingBlocks(normalized.Content)
		if hadRecoveryNotice && !strings.Contains(normalized.Content, RecoveryNoticeText) {
			normalized.Content = RecoveryNoticeContent + normalized.Content
		}
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
				needsReasoning := assistantNeedsReasoningForToolContext(normalized, priorMessages)

				if needsReasoning && rs != nil {
					// Build lookup keys and search
					scope := conversationScopeFromMessages(priorMessages, cacheNamespace)
					// Try to find cached reasoning
					msgMap := messageToMap(normalized)
					incStoreLookups(ctx)
					if cached, err := rs.LookupForMessage(ctx, msgMap, scope, cacheNamespace, priorMessages); err == nil && cached != "" {
						normalized.ReasoningContent = cached
						patched = true
						// Backfill portable (namespace-scoped) keys so this cache hit
						// is also available in other conversations with the same context.
						rs.BackfillPortableAliases(ctx, msgMap, cached, cacheNamespace, priorMessages)
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
	if msg.Name != "" {
		m["name"] = msg.Name
	}
	// tool_call_id is required by DeepSeek for role=tool. Dropping it here
	// used to be invisible while the currentRaw-without-... bug collapsed
	// history to a single message; once that was fixed, upstream started
	// rejecting with "messages[N]: missing field tool_call_id".
	if msg.ToolCallID != "" {
		m["tool_call_id"] = msg.ToolCallID
	}
	if msg.Prefix != "" {
		m["prefix"] = msg.Prefix
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

	for i, raw := range rawMessages {
		// Build prior messages slice for this message's index
		var prior []models.Message
		if i > 0 {
			prior = messages[:i]
		}

		normalized, patched, missing, diag := normalizeMessage(
			ctx,
			raw, rs,
			cacheNamespace,
			repairReasoning,
			keepReasoning,
			prior,
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
	return msg.Role == "assistant" && strings.Contains(msg.Content, RecoveryNoticeText)
}

func messagesContainRecoveryNotice(messages []models.Message) bool {
	return slices.ContainsFunc(messages, hasRecoveryNotice)
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
	for i, message := range slices.Backward(messages) {
		if hasRecoveryNotice(message) {
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
	for i, message := range slices.Backward(messages) {
		if hasRecoveryNotice(message) {
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
	for i, message := range slices.Backward(messages) {
		if message.Role == "user" {
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

	for _, msg := range slices.Backward(msgs) {
		role := msg.Role
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

// estimateMessageTokens is a coarse, dependency-free token estimate (~4 chars
// per token, ASCII-ish). It is deliberately rough: its job is to catch orders
// of magnitude, not to bill tokens. Multi-byte text is over-counted by bytes,
// which errs on the safe side for a ceiling guard.
func estimateMessageTokens(msg models.Message) int {
	n := len(msg.Content) + len(msg.ReasoningContent)
	for _, tc := range msg.ToolCalls {
		n += len(tc.Function.Name) + len(tc.Function.Arguments)
	}
	return n/4 + 1
}

// truncateMessages trims the oldest conversation rounds until the message
// count is within maxMessages and the estimated prompt size is within
// maxPromptTokens. Zero disables the corresponding limit.
//
// Rules:
//   - system messages are always preserved (they carry the agent's identity
//     and tool contracts; dropping them breaks the session).
//   - truncation happens on round boundaries: a "round" starts at a user
//     message, so the assistant/tool messages that answer it are never
//     orphaned. A tool message without its preceding assistant tool_call, or
//     an assistant tool_call without its tool result, is rejected upstream.
//   - max_messages is therefore a soft cap, not an exact one: the achievable
//     counts are the round boundaries, so a cap that falls mid-round lands
//     on the next boundary down. Undershooting is harmless; tearing a round
//     apart is not.
//   - the most recent user turn is always kept, so a lone oversized turn is
//     passed through rather than producing an empty request.
//
// It returns the possibly-trimmed messages, how many messages were removed,
// and the estimated tokens removed.
func truncateMessages(messages []models.Message, maxMessages, maxPromptTokens int) ([]models.Message, int, int) {
	// Kill switch: when both limits are disabled, return the input untouched
	// before doing any work, so the feature costs nothing and cannot alter a
	// request when it is off. Zero (or negative) disables a limit.
	if maxMessages <= 0 && maxPromptTokens <= 0 {
		return messages, 0, 0
	}
	if len(messages) == 0 {
		return messages, 0, 0
	}

	leading := leadingSystemMessages(messages)
	rest := messages[len(leading):]

	// Round start indices within `rest`. Index 0 always begins the first
	// round: if history does not start with a user message, the leading
	// non-user messages are grouped into that first round.
	roundStarts := []int{0}
	for i, msg := range rest {
		if i > 0 && msg.Role == "user" {
			roundStarts = append(roundStarts, i)
		}
	}

	// Token size of the leading system messages; they always survive, so this
	// is the floor any candidate cut point starts from.
	leadingTokens := 0
	for _, msg := range leading {
		leadingTokens += estimateMessageTokens(msg)
	}

	// suffixTokens[i] is the estimated size of rest[i:]. Built once so each
	// candidate cut point can be evaluated in O(1) instead of re-summing.
	suffixTokens := make([]int, len(rest)+1)
	for i := len(rest) - 1; i >= 0; i-- {
		suffixTokens[i] = suffixTokens[i+1] + estimateMessageTokens(rest[i])
	}

	// Choose the smallest cut point (largest surviving tail) that satisfies
	// both limits, walking cut points from the oldest round forward. This
	// drops the minimum amount of history needed to get under the limits,
	// preserving as much context as possible.
	//
	// Cut points are the round starts, and we never cut past the final round
	// start: the latest user turn and its answers always survive, so a lone
	// oversized turn is passed through rather than producing an empty
	// request. `chosen` stays 0 when nothing needs to be dropped.
	chosen := 0
	for r := 0; r < len(roundStarts); r++ {
		cut := roundStarts[r]
		keptCount := len(leading) + len(rest) - cut
		keptTokens := leadingTokens + suffixTokens[cut]
		if withinLimits(keptCount, keptTokens, maxMessages, maxPromptTokens) {
			chosen = cut
			break
		}
	}
	if chosen == 0 {
		// Either everything already fits (the full history is the first cut
		// point and it satisfied the limits) or even the latest round alone
		// exceeds them. In the latter case, fall back to keeping just the
		// final round: we refuse to cut into it, so it is the best we can do.
		last := roundStarts[len(roundStarts)-1]
		if last > 0 {
			keptCount := len(leading) + len(rest) - last
			if !withinLimits(keptCount, leadingTokens+suffixTokens[last], maxMessages, maxPromptTokens) {
				chosen = last
			}
		}
	}
	if chosen <= 0 || chosen >= len(rest) {
		// `chosen` is either 0 (nothing to drop / the latest round alone
		// already exceeds the limits) or degenerate. Never return a trimmed
		// slice in that case: an unchanged request is always safe, an empty
		// one never is.
		return messages, 0, 0
	}

	kept := make([]models.Message, 0, len(leading)+len(rest)-chosen)
	kept = append(kept, leading...)
	kept = append(kept, rest[chosen:]...)

	removedTokens := 0
	for _, msg := range messages {
		removedTokens += estimateMessageTokens(msg)
	}
	for _, msg := range kept {
		removedTokens -= estimateMessageTokens(msg)
	}

	return kept, len(messages) - len(kept), removedTokens
}

// withinLimits reports whether a candidate kept-message set satisfies both
// limits. A limit of zero is disabled.
func withinLimits(keptCount, keptTokens, maxMessages, maxPromptTokens int) bool {
	if maxMessages > 0 && keptCount > maxMessages {
		return false
	}
	if maxPromptTokens > 0 && keptTokens > maxPromptTokens {
		return false
	}
	return true
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
