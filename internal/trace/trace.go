package trace

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"
)

const traceSchemaVersion = 1

// TraceWriter writes structured request traces to disk.
type TraceWriter struct {
	baseDir    string
	sessionDir string
	mu         sync.Mutex
	nextSeq    atomic.Int64
}

// NewTraceWriter creates a new trace writer.
func NewTraceWriter(baseDir string) (*TraceWriter, error) {
	expandedDir, err := filepath.Abs(baseDir)
	if err != nil {
		return nil, fmt.Errorf("cannot resolve trace directory: %w", err)
	}

	if err := os.MkdirAll(expandedDir, 0700); err != nil {
		return nil, fmt.Errorf("cannot create trace directory: %w", err)
	}

	sessionName := fmt.Sprintf("%s-pid%d", time.Now().UTC().Format("20060102T150405.000Z"), os.Getpid())
	sessionDir := filepath.Join(expandedDir, sessionName)
	if err := os.MkdirAll(sessionDir, 0700); err != nil {
		return nil, fmt.Errorf("cannot create session directory: %w", err)
	}

	tw := &TraceWriter{
		baseDir:    expandedDir,
		sessionDir: sessionDir,
	}
	tw.nextSeq.Store(1)

	tw.writeManifest()
	return tw, nil
}

// SessionDir returns the current session directory.
func (tw *TraceWriter) SessionDir() string {
	return tw.sessionDir
}

// StartRequest creates a new trace for a request.
func (tw *TraceWriter) StartRequest(
	method,
	path,
	clientAddr string,
	headers map[string]string,
) *TraceRequest {

	seq := tw.nextSeq.Add(1) - 1
	tracePath := filepath.Join(tw.sessionDir, fmt.Sprintf("request-%06d.json", seq))

	data := map[string]any{
		"schema_version": traceSchemaVersion,
		"sequence":       seq,
		"created_at":     utcNowISO(),
		"request": map[string]any{
			"method":         method,
			"path":           path,
			"client_address": clientAddr,
			"headers":        sanitizedHeaders(headers),
		},
		"transform":       map[string]any{},
		"upstream":        map[string]any{},
		"cursor_response": map[string]any{},
		"completion":      map[string]any{},
	}

	return &TraceRequest{
		writer:  tw,
		seq:     int(seq),
		path:    tracePath,
		data:    data,
		started: time.Now(),
	}
}

func (tw *TraceWriter) writeManifest() {
	manifestPath := filepath.Join(tw.sessionDir, "manifest.json")
	manifest := map[string]any{
		"schema_version": traceSchemaVersion,
		"created_at":     utcNowISO(),
		"pid":            os.Getpid(),
		"base_dir":       tw.baseDir,
		"session_dir":    tw.sessionDir,
		"format":         "one JSON file per traced POST request",
	}
	writeJSONPrivate(manifestPath, manifest)
}

// TraceRequestInterface defines the tracing methods used by the proxy server.
// Implementations can be real (persist to disk) or noop (when tracing is disabled).
type TraceRequestInterface interface {
	RecordCursorBody(payload map[string]any)
	RecordUpstreamRequest(url string, headers map[string]string, bodyLen int)
	RecordUpstreamResponse(status int, headers map[string]string, body []byte, stream *bool)
	Finish(status string, extra map[string]any)
}

// NoopTraceRequest is a no-op implementation of TraceRequestInterface.
type NoopTraceRequest struct{}

func (NoopTraceRequest) RecordCursorBody(map[string]any)                              {}
func (NoopTraceRequest) RecordUpstreamRequest(string, map[string]string, int)         {}
func (NoopTraceRequest) RecordUpstreamResponse(int, map[string]string, []byte, *bool) {}
func (NoopTraceRequest) Finish(string, map[string]any)                                {}

// TraceRequest represents a single request trace.
type TraceRequest struct {
	writer  *TraceWriter
	seq     int
	path    string
	data    map[string]any
	started time.Time
	mu      sync.Mutex
}

// RecordCursorBody records the cursor request body.
func (tr *TraceRequest) RecordCursorBody(payload map[string]any) {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	tr.data["request"].(map[string]any)["body"] = payload
	tr.data["request"].(map[string]any)["summary"] = payloadSummary(payload)
}

// RecordTransform records transform metadata.
func (tr *TraceRequest) RecordTransform(originalModel, upstreamModel, cacheNamespace string,
	patched, missing, recovered, dropped int, notice string, contexts []any,
	continuedBoundary bool, retired int, diagnostics, steps []any, body map[string]any) {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	tr.data["transform"] = map[string]any{
		"original_model":               originalModel,
		"upstream_model":               upstreamModel,
		"cache_namespace":              cacheNamespace,
		"patched_reasoning_messages":   patched,
		"missing_reasoning_messages":   missing,
		"recovered_reasoning_messages": recovered,
		"recovery_dropped_messages":    dropped,
		"recovery_notice":              notice,
		"continued_recovery_boundary":  continuedBoundary,
		"retired_prefix_messages":      retired,
		"reasoning_diagnostics":        diagnostics,
		"recovery_steps":               steps,
		"upstream_request_body":        body,
	}
}

// RecordUpstreamRequest records the upstream request details.
func (tr *TraceRequest) RecordUpstreamRequest(url string, headers map[string]string, bodyLen int) {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	tr.data["upstream"].(map[string]any)["request"] = map[string]any{
		"url":        url,
		"headers":    sanitizedHeaders(headers),
		"body_bytes": bodyLen,
	}
}

// RecordUpstreamResponse records the upstream response details.
func (tr *TraceRequest) RecordUpstreamResponse(status int, headers map[string]string, body []byte, stream *bool) {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	resp := map[string]any{
		"status": status,
	}
	if headers != nil {
		resp["headers"] = sanitizedHeaders(headers)
	}
	if stream != nil {
		resp["stream"] = *stream
	}
	if body != nil {
		resp["body"] = jsonableBody(body)
	}
	tr.data["upstream"].(map[string]any)["response"] = resp
}

// RecordCursorResponse records the cursor response details.
func (tr *TraceRequest) RecordCursorResponse(status int, headers map[string]string, body []byte) {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	resp := map[string]any{
		"status": status,
	}
	if headers != nil {
		resp["headers"] = sanitizedHeaders(headers)
	}
	if body != nil {
		resp["body"] = jsonableBody(body)
	}
	tr.data["cursor_response"].(map[string]any)["response"] = resp
}

// RecordStreamChunk records a streaming chunk.
func (tr *TraceRequest) RecordStreamChunk(upstreamLine, cursorLine string) {
	tr.mu.Lock()
	defer tr.mu.Unlock()

	upStream := tr.data["upstream"].(map[string]any)
	upChunks := upStream["stream"].(map[string]any)["chunks"].([]any)
	idx := len(upChunks)
	upChunks = append(upChunks, map[string]any{
		"index": idx,
		"line":  upstreamLine,
	})
	upStream["stream"].(map[string]any)["chunks"] = upChunks

	cursorResp := tr.data["cursor_response"].(map[string]any)
	cursorChunks := cursorResp["stream"].(map[string]any)["chunks"].([]any)
	cursorChunks = append(cursorChunks, map[string]any{
		"index": idx,
		"line":  cursorLine,
	})
	cursorResp["stream"].(map[string]any)["chunks"] = cursorChunks
}

// RecordUsage records token usage.
func (tr *TraceRequest) RecordUsage(usage map[string]any) {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	tr.data["upstream"].(map[string]any)["usage"] = usage
}

// Finish marks the trace as complete and writes to disk.
func (tr *TraceRequest) Finish(
	status string,
	extra map[string]any,
) {

	tr.mu.Lock()
	defer tr.mu.Unlock()

	completion := map[string]any{
		"status":      status,
		"finished_at": utcNowISO(),
		"elapsed_ms":  time.Since(tr.started).Milliseconds(),
	}
	maps.Copy(completion, extra)
	tr.data["completion"] = completion

	writeJSONPrivate(tr.path, tr.data)
}

// ---------------------------------------------------------------------------
// Utility functions
// ---------------------------------------------------------------------------

func utcNowISO() string {
	return time.Now().UTC().Format("2006-01-02T15:04:05.000Z")
}

func sha256Text(value string) string {
	h := sha256.Sum256([]byte(value))
	return fmt.Sprintf("%x", h)
}

func sanitizedHeaders(headers map[string]string) map[string]any {
	if headers == nil {
		return nil
	}
	sanitized := make(map[string]any)
	for name, value := range headers {
		if name == "Authorization" {
			sanitized[name] = map[string]any{
				"present": true,
				"sha256":  sha256Text(value),
			}
		} else {
			sanitized[name] = value
		}
	}
	return sanitized
}

func jsonableBody(body []byte) map[string]any {
	text := string(body)
	var payload any
	if err := json.Unmarshal(body, &payload); err == nil {
		return map[string]any{
			"json": payload,
		}
	}
	return map[string]any{
		"text": text,
	}
}

func payloadSummary(payload map[string]any) map[string]any {
	messages, _ := payload["messages"].([]any)
	if messages == nil {
		messages = []any{}
	}

	var systemHashes []string
	msgSummaries := make([]map[string]any, 0, len(messages))
	for i, raw := range messages {
		msg, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		content := getString(msg, "content")
		if msg["role"] == "system" {
			systemHashes = append(systemHashes, sha256Text(content))
		}
		tcs, _ := msg["tool_calls"].([]any)
		var tcIDs []string
		for _, tc := range tcs {
			if tcMap, ok := tc.(map[string]any); ok {
				if id, ok := tcMap["id"].(string); ok {
					tcIDs = append(tcIDs, id)
				}
			}
		}
		rc, _ := msg["reasoning_content"].(string)

		summary := map[string]any{
			"index":                    i,
			"role":                     msg["role"],
			"content":                  map[string]any{"length": len(content), "sha256": sha256Text(content)},
			"has_tool_calls":           len(tcIDs) > 0 || len(tcs) > 0,
			"tool_call_ids":            tcIDs,
			"has_reasoning_content":    rc != "",
			"reasoning_content_length": len(rc),
		}
		msgSummaries = append(msgSummaries, summary)
	}

	tools, _ := payload["tools"].([]any)
	var toolNames []string
	for _, t := range tools {
		if tMap, ok := t.(map[string]any); ok {
			if fn, ok := tMap["function"].(map[string]any); ok {
				toolNames = append(toolNames, getString(fn, "name"))
			}
		}
	}

	return map[string]any{
		"model":                payload["model"],
		"stream":               payload["stream"],
		"message_count":        len(messages),
		"tool_count":           len(tools),
		"tool_names":           toolNames,
		"system_prompt_hashes": systemHashes,
		"messages":             msgSummaries,
	}
}

func getString(m map[string]any, key string) string {
	if v, ok := m[key].(string); ok {
		return v
	}
	return ""
}

func writeJSONPrivate(
	path string,
	data map[string]any,
) {
	tmpPath := path + ".tmp"
	b, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		return
	}
	b = append(b, '\n')
	if err := os.WriteFile(tmpPath, b, 0600); err != nil {
		return
	}
	os.Rename(tmpPath, path)
	os.Chmod(path, 0600)
}
