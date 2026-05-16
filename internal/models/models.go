package models

import "time"

// ChatCompletionRequest represents an incoming chat completion request.
type ChatCompletionRequest struct {
	Model            string          `json:"model"`
	Messages         []Message       `json:"messages"`
	Stream           bool            `json:"stream,omitempty"`
	StreamOptions    *StreamOptions  `json:"stream_options,omitempty"`
	MaxTokens        int             `json:"max_tokens,omitempty"`
	Temperature      float64         `json:"temperature,omitempty"`
	TopP             float64         `json:"top_p,omitempty"`
	Tools            []Tool          `json:"tools,omitempty"`
	ToolChoice       any             `json:"tool_choice,omitempty"`
	Thinking         *ThinkingConfig `json:"thinking,omitempty"`
	ReasoningEffort  string          `json:"reasoning_effort,omitempty"`
	Stop             any             `json:"stop,omitempty"`
	ResponseFormat   any             `json:"response_format,omitempty"`
	PresencePenalty  float64         `json:"presence_penalty,omitempty"`
	FrequencyPenalty float64         `json:"frequency_penalty,omitempty"`
	Logprobs         bool            `json:"logprobs,omitempty"`
	TopLogprobs      int             `json:"top_logprobs,omitempty"`
	N                int             `json:"n,omitempty"`
	Seed             int             `json:"seed,omitempty"`
	User             string          `json:"user,omitempty"`
	LogitBias        map[string]int  `json:"logit_bias,omitempty"`

	// Legacy fields
	Functions           []Function `json:"functions,omitempty"`
	FunctionCall        any        `json:"function_call,omitempty"`
	MaxCompletionTokens int        `json:"max_completion_tokens,omitempty"`
}

// StreamOptions contains options for streaming responses.
type StreamOptions struct {
	IncludeUsage bool `json:"include_usage,omitempty"`
}

// ThinkingConfig configures thinking mode.
type ThinkingConfig struct {
	Type string `json:"type"`
}

// Message represents a chat message.
type Message struct {
	Role             string     `json:"role"`
	Content          string     `json:"content,omitempty"`
	Name             string     `json:"name,omitempty"`
	ToolCallID       string     `json:"tool_call_id,omitempty"`
	ToolCalls        []ToolCall `json:"tool_calls,omitempty"`
	ReasoningContent string     `json:"reasoning_content,omitempty"`
	Prefix           string     `json:"prefix,omitempty"`
}

// Tool represents a tool definition.
type Tool struct {
	Type     string       `json:"type"`
	Function ToolFunction `json:"function"`
}

// ToolFunction represents the function definition of a tool.
type ToolFunction struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Parameters  any    `json:"parameters,omitempty"`
}

// ToolCall represents a tool call in an assistant message.
type ToolCall struct {
	ID       string           `json:"id"`
	Type     string           `json:"type"`
	Function ToolCallFunction `json:"function"`
}

// ToolCallFunction represents the function part of a tool call.
type ToolCallFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// Function represents a legacy function definition.
type Function struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Parameters  any    `json:"parameters,omitempty"`
}

// ChatCompletionResponse represents an OpenAI chat completion response.
type ChatCompletionResponse struct {
	ID      string   `json:"id"`
	Object  string   `json:"object"`
	Created int64    `json:"created"`
	Model   string   `json:"model"`
	Choices []Choice `json:"choices"`
	Usage   *Usage   `json:"usage,omitempty"`
}

// Choice represents a completion choice.
type Choice struct {
	Index        int     `json:"index"`
	Message      Message `json:"message"`
	FinishReason string  `json:"finish_reason,omitempty"`
	Logprobs     any     `json:"logprobs,omitempty"`
}

// ChatCompletionChunk represents a streaming chunk.
type ChatCompletionChunk struct {
	ID      string        `json:"id"`
	Object  string        `json:"object"`
	Created int64         `json:"created"`
	Model   string        `json:"model"`
	Choices []ChunkChoice `json:"choices"`
	Usage   *Usage        `json:"usage,omitempty"`
}

// ChunkChoice represents a choice in a streaming chunk.
type ChunkChoice struct {
	Index        int    `json:"index"`
	Delta        Delta  `json:"delta"`
	FinishReason string `json:"finish_reason,omitempty"`
	Logprobs     any    `json:"logprobs,omitempty"`
}

// Delta represents the delta in a streaming chunk.
type Delta struct {
	Role             string     `json:"role,omitempty"`
	Content          string     `json:"content,omitempty"`
	ReasoningContent string     `json:"reasoning_content,omitempty"`
	ToolCalls        []ToolCall `json:"tool_calls,omitempty"`
}

// Usage represents token usage.
type Usage struct {
	PromptTokens            int                      `json:"prompt_tokens"`
	CompletionTokens        int                      `json:"completion_tokens"`
	TotalTokens             int                      `json:"total_tokens"`
	CompletionTokensDetails *CompletionTokensDetails `json:"completion_tokens_details,omitempty"`
	PromptCacheHitTokens    int                      `json:"prompt_cache_hit_tokens,omitempty"`
	PromptCacheMissTokens   int                      `json:"prompt_cache_miss_tokens,omitempty"`
}

// CompletionTokensDetails contains details about completion token usage.
type CompletionTokensDetails struct {
	ReasoningTokens int `json:"reasoning_tokens,omitempty"`
}

// HealthResponse is the health check response.
type HealthResponse struct {
	Ok bool `json:"ok"`
}

// ModelsResponse is the list models response.
type ModelsResponse struct {
	Object string  `json:"object"`
	Data   []Model `json:"data"`
}

// Model represents a model entry.
type Model struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created"`
	OwnedBy string `json:"owned_by"`
}

// ErrorResponse represents an API error.
type ErrorResponse struct {
	Error ErrorDetail `json:"error"`
}

// ErrorDetail contains error details.
type ErrorDetail struct {
	Message                  string `json:"message"`
	Type                     string `json:"type,omitempty"`
	Code                     string `json:"code,omitempty"`
	MissingReasoningMessages int    `json:"missing_reasoning_messages,omitempty"`
}

// PreparedRequest holds the result of request transformation.
type PreparedRequest struct {
	Payload                    map[string]any
	OriginalModel              string
	UpstreamModel              string
	CacheNamespace             string
	PatchedReasoningMessages   int
	MissingReasoningMessages   int
	RecoveredReasoningMessages int
	RecoveryDroppedMessages    int
	RecoveryNotice             string
	RecordResponseScope        string
	RecordResponseMessages     []Message
	RecordResponseContexts     []ResponseContext
	ReasoningDiagnostics       []ReasoningDiagnostic
	RecoverySteps              []RecoveryStep
	ContinuedRecoveryBoundary  bool
	RetiredPrefixMessages      int
	StoreLookups               int
}

// ResponseContext pairs a scope with prior messages for recording.
type ResponseContext struct {
	Scope    string
	Messages []Message
}

// ReasoningDiagnostic logs reasoning lookup results for debugging.
type ReasoningDiagnostic struct {
	MessageIndex   int    `json:"message_index"`
	Role           string `json:"role"`
	NeedsReasoning bool   `json:"needs_reasoning"`
	HadReasoning   bool   `json:"had_reasoning_content"`
	Patched        bool   `json:"patched"`
	Missing        bool   `json:"missing"`
	LookupScope    string `json:"lookup_scope,omitempty"`
	HitKind        string `json:"hit_kind,omitempty"`
}

// RecoveryStep logs each recovery attempt.
type RecoveryStep struct {
	Strategy              string `json:"strategy"`
	MissingIndexes        []int  `json:"missing_indexes,omitempty"`
	RecoveryBoundaryIndex int    `json:"recovery_boundary_index,omitempty"`
	ContextUserIndex      int    `json:"context_user_index,omitempty"`
	DroppedMessages       int    `json:"dropped_messages,omitempty"`
	Notice                string `json:"notice,omitempty"`
}

// ReasoningCacheEntry is the SQLite storage model for reasoning content.
type ReasoningCacheEntry struct {
	Key         string    `bun:",pk"`
	Reasoning   string    `bun:",notnull"`
	MessageJSON string    `bun:",notnull"`
	CreatedAt   time.Time `bun:",notnull"`
}

// ModelsList are the model IDs to announce.
var ModelsList = []string{
	"deepseek-v4-pro",
	"deepseek-v4-flash",
}
