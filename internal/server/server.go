package server

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	echo_otel "github.com/labstack/echo-opentelemetry"
	"github.com/labstack/echo/v5"
	"github.com/labstack/echo/v5/middleware"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	otel_trace "go.opentelemetry.io/otel/trace"

	"github.com/mrksmt/deepseek-cursor-proxy/internal/config"
	"github.com/mrksmt/deepseek-cursor-proxy/internal/models"
	"github.com/mrksmt/deepseek-cursor-proxy/internal/store"
	"github.com/mrksmt/deepseek-cursor-proxy/internal/streaming"
	"github.com/mrksmt/deepseek-cursor-proxy/internal/trace"
	"github.com/mrksmt/deepseek-cursor-proxy/internal/transform"
	"github.com/mrksmt/deepseek-cursor-proxy/internal/tunnel"
)

const serverVersion = "DeepSeekGoProxy/0.1"

// context key for storing the TraceRequest
type traceRequestKey struct{}

// httpError wraps an HTTP status code with a message for centralized response writing.
type httpError struct {
	Code    int
	Message string
}

func (e *httpError) Error() string { return e.Message }

// writeError writes a JSON error response from an *httpError, finishes
// the trace, and logs the rejection.
func writeError(ctx context.Context, c *echo.Context, err error) error {
	var he *httpError
	if !errors.As(err, &he) {
		he = &httpError{Code: http.StatusInternalServerError, Message: err.Error()}
	}
	log.Printf("rejected request status=%d reason=%s", he.Code, he.Message)
	getTraceRequest(ctx).Finish("rejected", map[string]any{"http_status": he.Code})
	return c.JSON(he.Code, models.ErrorResponse{
		Error: models.ErrorDetail{Message: he.Message},
	})
}

func withTraceRequest(ctx context.Context, tr trace.TraceRequestInterface) context.Context {
	if tr == nil {
		tr = trace.NoopTraceRequest{}
	}
	return context.WithValue(ctx, traceRequestKey{}, tr)
}

func getTraceRequest(ctx context.Context) trace.TraceRequestInterface {
	tr, _ := ctx.Value(traceRequestKey{}).(trace.TraceRequestInterface)
	if tr == nil {
		return trace.NoopTraceRequest{}
	}
	return tr
}

// ProxyServer wraps the Echo server with proxy-specific state.
type ProxyServer struct {
	echo           *echo.Echo
	httpServer     *http.Server
	cfg            *config.Config
	reasoningStore *store.ReasoningStore
	traceWriter    *trace.TraceWriter
	ngrokTunnel    *tunnel.NgrokTunnel

	tracer     otel_trace.Tracer
	httpClient *http.Client
}

// NewProxyServer creates a new proxy server.
func NewProxyServer(
	cfg *config.Config,
	rs *store.ReasoningStore,
	tw *trace.TraceWriter,
	otelTracer *trace.OTelTracer,
) *ProxyServer {

	s := &ProxyServer{
		cfg:            cfg,
		reasoningStore: rs,
		traceWriter:    tw,
		tracer:         otelTracer.Tracer(),
		httpClient:     &http.Client{Timeout: time.Duration(cfg.RequestTimeout) * time.Second},
	}

	e := echo.New()

	// e.Use(middleware.Recover())
	e.Use(echo_otel.NewMiddleware("deepseek-cursor-proxy"))

	if cfg.CORS {
		e.Use(middleware.CORSWithConfig(middleware.CORSConfig{
			AllowOrigins:     []string{"*"},
			AllowMethods:     []string{http.MethodGet, http.MethodPost, http.MethodOptions},
			AllowHeaders:     []string{"Origin", "Content-Type", "Accept", "Authorization"},
			ExposeHeaders:    []string{"Content-Length"},
			AllowCredentials: true,
		}))
	}

	e.GET("/healthz", s.handleHealth)
	e.GET("/v1/healthz", s.handleHealth)
	e.GET("/models", s.handleModels)
	e.GET("/v1/models", s.handleModels)
	chatGroup := e.Group("")
	chatGroup.Use(concurrencyLimiter(cfg.MaxConcurrentRequests))
	chatGroup.POST("/v1/chat/completions", s.handleChatCompletions)
	chatGroup.POST("/chat/completions", s.handleChatCompletions)

	s.echo = e
	s.httpServer = &http.Server{
		Addr:         fmt.Sprintf("%s:%d", cfg.Host, cfg.Port),
		Handler:      e,
		ReadTimeout:  0,
		WriteTimeout: 0,
	}
	return s
}

// Start starts the HTTP server.
func (s *ProxyServer) Start(
	host string,
	port int,
) error {
	return s.httpServer.ListenAndServe()
}

// Shutdown gracefully shuts down the server.
func (s *ProxyServer) Shutdown(ctx context.Context) error {
	return s.httpServer.Shutdown(ctx)
}

// ---------------------------------------------------------------------------
// Handlers
// ---------------------------------------------------------------------------

func (s *ProxyServer) handleHealth(c *echo.Context) error {
	return c.JSON(http.StatusOK, models.HealthResponse{Ok: true})
}

func (s *ProxyServer) handleModels(c *echo.Context) error {

	created := time.Now().Unix()
	modelIDs := make([]string, 0, len(models.ModelsList)+1)
	modelIDs = append(modelIDs, s.cfg.UpstreamModel)
	for _, m := range models.ModelsList {
		if m != s.cfg.UpstreamModel {
			modelIDs = append(modelIDs, m)
		}
	}

	data := make([]models.Model, 0, len(modelIDs))
	for _, id := range modelIDs {
		data = append(data, models.Model{
			ID:      id,
			Object:  "model",
			Created: created,
			OwnedBy: "deepseek",
		})
	}
	return c.JSON(http.StatusOK, models.ModelsResponse{Object: "list", Data: data})
}

func (s *ProxyServer) handleChatCompletions(c *echo.Context) (err error) {

	ctx := c.Request().Context()
	ctx, span := s.tracer.Start(ctx, "handleChatCompletions")
	defer span.End()

	// Catch panics so they are recorded on the span and finishTrace is called.
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("panic: %v", r)
			log.Printf("recovered panic in handleChatCompletions: %v", r)
			span.RecordError(fmt.Errorf("%v", r))
			span.SetStatus(codes.Error, fmt.Sprintf("%v", r))
			getTraceRequest(ctx).Finish("panic", map[string]any{"error": fmt.Sprintf("%v", r)})
		}
	}()

	reqPath := c.Path()

	tr := s.startTrace(c.Request(), reqPath)
	ctx = withTraceRequest(ctx, tr)

	token, err := s.checkAuthorization(ctx, c.Request())
	if err != nil {
		return writeError(ctx, c, err)
	}

	payload, err := s.readAndParseBody(ctx, c.Request())
	if err != nil {
		return writeError(ctx, c, err)
	}

	getTraceRequest(ctx).RecordCursorBody(payload)

	prepared, err := s.prepareUpstream(ctx, payload, token)
	if err != nil {
		return writeError(ctx, c, err)
	}

	upReq, upstreamBody, isStream, err := s.buildUpstreamRequest(ctx, c.Request(), prepared, token)
	if err != nil {
		return writeError(ctx, c, err)
	}

	otel_trace.SpanFromContext(ctx).SetAttributes(
		attribute.Bool("stream", isStream),
	)
	otel_trace.SpanFromContext(ctx).AddEvent("upstream.body", otel_trace.WithAttributes(attribute.Int("size", len(upstreamBody))))

	getTraceRequest(ctx).RecordUpstreamRequest(
		fmt.Sprintf("%s/chat/completions", s.cfg.UpstreamBaseURL),
		headerMap(upReq.Header),
		len(upstreamBody),
	)

	upResp, err := s.doUpstream(ctx, upReq, prepared.UpstreamModel)
	if err != nil {
		return writeError(ctx, c, err)
	}
	if upResp == nil {
		return writeError(ctx, c, &httpError{
			Code:    http.StatusBadGateway,
			Message: "Upstream returned nil response",
		})
	}
	defer upResp.Body.Close()

	if upResp.StatusCode >= 400 {
		s.handleUpstreamError(ctx, c.Response(), upResp)
		return nil
	}

	streamVal := isStream
	getTraceRequest(ctx).RecordUpstreamResponse(upResp.StatusCode, responseHeaders(upResp), nil, &streamVal)

	result := s.proxyResponse(ctx, c.Response(), upResp, prepared, isStream)
	s.finishTrace(ctx, result, upResp.StatusCode, isStream)

	span.SetAttributes(
		attribute.String("upstream_model", prepared.UpstreamModel),
	)
	if result.usage != nil {
		span.AddEvent("tokens.usage", otel_trace.WithAttributes(
			attribute.Int("prompt", result.usage.PromptTokens),
			attribute.Int("completion", result.usage.CompletionTokens),
			attribute.Int("total", result.usage.TotalTokens),
		))
	}

	return nil
}

// ---------------------------------------------------------------------------
// Middleware
// ---------------------------------------------------------------------------

// concurrencyLimiter returns a middleware that limits the number of concurrent
// in-flight requests. When the limit is reached, new requests receive 429.
func concurrencyLimiter(limit int) echo.MiddlewareFunc {
	sem := make(chan struct{}, limit)
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c *echo.Context) error {
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
				return next(c)
			default:
				log.Printf("rejected request path=%s status=429 reason=at_capacity", c.Path())
				return c.JSON(http.StatusTooManyRequests, models.ErrorResponse{
					Error: models.ErrorDetail{
						Message: "Too many concurrent requests. Try again later.",
					},
				})
			}
		}
	}
}

// ---------------------------------------------------------------------------
// Request pipeline steps
// ---------------------------------------------------------------------------

// startTrace begins tracing the request if a trace writer is configured.
func (s *ProxyServer) startTrace(r *http.Request, reqPath string) trace.TraceRequestInterface {
	if s.traceWriter == nil {
		return trace.NoopTraceRequest{}
	}
	return s.traceWriter.StartRequest(
		r.Method,
		reqPath,
		r.RemoteAddr,
		headerMap(r.Header),
	)
}

// checkAuthorization extracts and validates the Bearer token.
// Returns the token on success.
func (s *ProxyServer) checkAuthorization(
	ctx context.Context,
	r *http.Request,
) (string, error) {

	token := extractBearerToken(r.Header.Get("Authorization"))
	if token == "" {
		return "", &httpError{
			Code:    http.StatusUnauthorized,
			Message: "Missing Authorization bearer token",
		}
	}
	return token, nil
}

// readAndParseBody reads and JSON-decodes the request body in one pass
// with a size limit enforced via io.LimitReader.
func (s *ProxyServer) readAndParseBody(
	ctx context.Context,
	r *http.Request,
) (map[string]any, error) {

	ctx, span := s.tracer.Start(ctx, "readAndParseBody")
	defer span.End()

	limited := io.LimitReader(r.Body, s.cfg.MaxRequestBodyBytes+1)
	var payload map[string]any
	decoder := json.NewDecoder(limited)
	if err := decoder.Decode(&payload); err != nil {
		if decoder.InputOffset() > s.cfg.MaxRequestBodyBytes {
			return nil, &httpError{
				Code:    http.StatusRequestEntityTooLarge,
				Message: fmt.Sprintf("Request body too large; limit is %d bytes", s.cfg.MaxRequestBodyBytes),
			}
		}
		return nil, &httpError{
			Code:    http.StatusBadRequest,
			Message: fmt.Sprintf("Invalid JSON: %v", err),
		}
	}

	span.AddEvent("body.decoded", otel_trace.WithAttributes(
		attribute.Int64("bytes_read", decoder.InputOffset()),
	))

	return payload, nil
}

// prepareUpstream transforms the payload for the upstream and checks
// the missing-reasoning strategy. On strict rejection, returns a 409 error.
func (s *ProxyServer) prepareUpstream(
	ctx context.Context,
	payload map[string]any,
	token string,
) (*models.PreparedRequest, error) {
	ctx, span := s.tracer.Start(ctx, "prepareUpstream")
	defer span.End()

	prepared := transform.PrepareUpstreamRequest(ctx, payload, s.cfg, s.reasoningStore, token)

	rawMessages, _ := payload["messages"].([]any)
	span.AddEvent("prepare.result", otel_trace.WithAttributes(
		attribute.Int("messages.count", len(rawMessages)),
		attribute.Int("patched_count", prepared.PatchedReasoningMessages),
		attribute.Int("missing_count", prepared.MissingReasoningMessages),
		attribute.Int("recovered_count", prepared.RecoveredReasoningMessages),
		attribute.Int("recovery_iterations", len(prepared.RecoverySteps)),
		attribute.Int("store.lookups", prepared.StoreLookups),
	))

	if prepared.MissingReasoningMessages > 0 && s.cfg.MissingReasoningStrategy == "reject" {
		return nil, &httpError{
			Code: http.StatusConflict,
			Message: fmt.Sprintf(
				"deepseek-cursor-proxy is running in strict missing-reasoning mode and cannot automatically "+
					"recover this thinking-mode tool-call history because cached DeepSeek reasoning_content is "+
					"missing for %d assistant message(s). Restart without --missing-reasoning-strategy reject, "+
					"or pass --missing-reasoning-strategy recover, so the proxy can recover from partial chat "+
					"history automatically.", prepared.MissingReasoningMessages),
		}
	}

	return prepared, nil
}

// buildUpstreamRequest creates and configures the HTTP request for the upstream.
// Returns the request, the marshalled body bytes, and the stream flag.
func (s *ProxyServer) buildUpstreamRequest(
	ctx context.Context,
	r *http.Request,
	prepared *models.PreparedRequest,
	token string,
) (
	upReq *http.Request,
	upstreamBody []byte,
	isStream bool,
	err error,
) {
	ctx, span := s.tracer.Start(ctx, "buildUpstreamRequest")
	defer span.End()

	buf := new(bytes.Buffer)
	if err := json.NewEncoder(buf).Encode(prepared.Payload); err != nil {
		return nil, nil, false, &httpError{
			Code:    http.StatusInternalServerError,
			Message: "Internal error preparing request",
		}
	}
	upstreamBody = buf.Bytes()

	upstreamURL := fmt.Sprintf("%s/chat/completions", s.cfg.UpstreamBaseURL)
	isStream, _ = prepared.Payload["stream"].(bool)

	upReq, err = http.NewRequestWithContext(
		ctx,
		http.MethodPost,
		upstreamURL,
		buf,
	)
	if err != nil {
		return nil, nil, false, &httpError{
			Code:    http.StatusInternalServerError,
			Message: "Internal error creating upstream request",
		}
	}

	upReq.Header.Set("Authorization", token)
	upReq.Header.Set("Content-Type", "application/json")
	if isStream {
		upReq.Header.Set("Accept", "text/event-stream")
	} else {
		upReq.Header.Set("Accept", "application/json")
	}
	upReq.Header.Set("Accept-Encoding", "identity")
	upReq.Header.Set("User-Agent", serverVersion)
	if al := r.Header.Get("Accept-Language"); al != "" {
		upReq.Header.Set("Accept-Language", al)
	}

	return upReq, upstreamBody, isStream, nil
}

// doUpstream sends the request to the upstream API.
func (s *ProxyServer) doUpstream(
	ctx context.Context,
	upReq *http.Request,
	upstreamModel string,
) (*http.Response, error) {

	ctx, span := s.tracer.Start(ctx, "doUpstream")
	defer span.End()

	span.SetAttributes(
		attribute.String("upstream.model", upstreamModel),
	)

	upResp, err := s.upstreamRoundTrip(ctx, upReq)
	if err != nil {
		return nil, &httpError{
			Code:    http.StatusBadGateway,
			Message: fmt.Sprintf("Upstream request failed: %v", err),
		}
	}

	return upResp, nil
}

// upstreamRoundTrip executes the upstream HTTP request and returns the response.
func (s *ProxyServer) upstreamRoundTrip(
	ctx context.Context,
	upReq *http.Request,
) (*http.Response, error) {
	ctx, span := s.tracer.Start(ctx, "upstreamRoundTrip")
	defer span.End()

	if s.httpClient == nil {
		return nil, fmt.Errorf("http client is nil")
	}

	upResp, err := s.httpClient.Do(upReq)
	if err != nil {
		return nil, err
	}
	return upResp, nil
}

// handleUpstreamError reads and forwards an error response from the upstream.
func (s *ProxyServer) handleUpstreamError(
	ctx context.Context,
	w http.ResponseWriter,
	upResp *http.Response,
) {
	ctx, span := s.tracer.Start(ctx, "handleUpstreamError")
	defer span.End()

	if upResp == nil {
		log.Printf("handleUpstreamError: nil response")
		w.WriteHeader(http.StatusBadGateway)
		return
	}

	bodyBytes, _ := io.ReadAll(upResp.Body)
	getTraceRequest(ctx).RecordUpstreamResponse(upResp.StatusCode, responseHeaders(upResp), bodyBytes, nil)
	log.Printf("upstream error status=%d", upResp.StatusCode)
	w.Header().Set(echo.HeaderContentType, "application/json")
	w.WriteHeader(upResp.StatusCode)
	w.Write(bodyBytes)
}

// proxyResponse dispatches to streaming or regular response proxying.
func (s *ProxyServer) proxyResponse(
	ctx context.Context,
	w http.ResponseWriter,
	upResp *http.Response,
	prepared *models.PreparedRequest,
	stream bool,
) *proxyResponseResult {
	ctx, span := s.tracer.Start(ctx, "proxyResponse")
	defer span.End()

	if stream {
		return s.proxyStreamingResponse(ctx, w, upResp, prepared)
	}
	return s.proxyRegularResponse(ctx, w, upResp, prepared)
}

// finishTrace completes the trace with the final status and metadata.
func (s *ProxyServer) finishTrace(
	ctx context.Context,
	result *proxyResponseResult,
	statusCode int,
	stream bool,
) {
	tr := getTraceRequest(ctx)
	status := "completed"
	if result != nil && !result.sent {
		status = "client_disconnected"
	}
	extra := map[string]any{
		"http_status": statusCode,
		"stream":      stream,
	}
	if result != nil && result.usage != nil {
		extra["usage"] = result.usage
	}
	tr.Finish(status, extra)
}

// ---------------------------------------------------------------------------
// Response proxying
// ---------------------------------------------------------------------------

type proxyResponseResult struct {
	sent  bool
	usage *models.Usage
}

// sseLineResult holds the outcome of processing one SSE line.
type sseLineResult struct {
	processTime  time.Duration
	writeTime    time.Duration
	bytesWritten int64
	notice       string
	chunkUsage   *models.Usage
	writeErr     error
	fin          bool
}

// processSSELine rewrites one SSE line and writes it to the client.
func (s *ProxyServer) processSSELine(
	ctx context.Context,
	line string,
	w http.ResponseWriter,
	flusher http.Flusher,
	prepared *models.PreparedRequest,
	accumulator *streaming.StreamAccumulator,
	displayAdapter *streaming.CursorReasoningDisplayAdapter,
	contexts []models.ResponseContext,
	pendingNotice string,
) sseLineResult {
	beforeProcess := time.Now()
	rewritten, fin, notice, chunkUsage := s.rewriteSSELine(
		ctx,
		line,
		prepared.OriginalModel,
		accumulator,
		prepared.CacheNamespace,
		contexts,
		displayAdapter,
		pendingNotice,
	)
	processTime := time.Since(beforeProcess)

	beforeWrite := time.Now()
	n, writeErr := w.Write([]byte(rewritten))
	writeTime := time.Since(beforeWrite)
	if writeErr == nil {
		flusher.Flush()
	}

	return sseLineResult{
		processTime:  processTime,
		writeTime:    writeTime,
		bytesWritten: int64(n),
		notice:       notice,
		chunkUsage:   chunkUsage,
		writeErr:     writeErr,
		fin:          fin,
	}
}

func (s *ProxyServer) proxyRegularResponse(
	ctx context.Context,
	w http.ResponseWriter,
	upResp *http.Response,
	prepared *models.PreparedRequest,
) *proxyResponseResult {
	ctx, span := s.tracer.Start(ctx, "proxyRegularResponse")
	defer span.End()

	if upResp == nil || upResp.Body == nil {
		log.Printf("upstream response or body is nil")
		return &proxyResponseResult{sent: false}
	}

	bodyBytes, err := io.ReadAll(upResp.Body)
	if err != nil {
		log.Printf("failed to read upstream response: %v", err)
		return &proxyResponseResult{sent: false}
	}

	usage := usageFromBody(bodyBytes)

	rewritten, err := transform.RewriteResponseBody(
		ctx,
		bodyBytes,
		prepared.OriginalModel,
		s.reasoningStore,
		prepared.RecordResponseMessages,
		prepared.CacheNamespace,
		prepared.RecoveryNotice,
		prepared.RecordResponseScope,
		prepared.RecordResponseMessages,
		prepared.RecordResponseContexts,
		s.cfg.DisplayReasoning,
		s.cfg.CollapsibleReasoning,
	)
	if err != nil {
		log.Printf("failed to rewrite response: %v", err)
		rewritten = bodyBytes
	}

	w.Header().Set(echo.HeaderContentType, upResp.Header.Get("Content-Type"))
	w.WriteHeader(upResp.StatusCode)

	_, writeErr := w.Write(rewritten)
	return &proxyResponseResult{
		sent:  writeErr == nil,
		usage: usage,
	}
}

func (s *ProxyServer) proxyStreamingResponse(
	ctx context.Context,
	w http.ResponseWriter,
	upResp *http.Response,
	prepared *models.PreparedRequest,
) *proxyResponseResult {

	ctx, span := s.tracer.Start(ctx, "proxyStreamingResponse")
	defer span.End()

	w.Header().Set(echo.HeaderContentType, "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "close")
	w.WriteHeader(upResp.StatusCode)

	flusher, ok := w.(http.Flusher)
	if !ok {
		log.Printf("streaming not supported by ResponseWriter")
		return &proxyResponseResult{sent: false}
	}

	accumulator := streaming.NewStreamAccumulator()
	var displayAdapter *streaming.CursorReasoningDisplayAdapter
	if s.cfg.DisplayReasoning {
		displayAdapter = streaming.NewCursorReasoningDisplayAdapter(s.cfg.CollapsibleReasoning)
	}

	contexts := prepared.RecordResponseContexts
	pendingNotice := prepared.RecoveryNotice
	finalized := false

	if s.reasoningStore != nil {
		s.reasoningStore.BeginBatch()
		defer s.cleanupStreamingBatch(ctx, accumulator, contexts, prepared.CacheNamespace, &finalized)
	}

	if upResp.Body == nil {
		log.Printf("upstream response body is nil")
		return &proxyResponseResult{sent: false}
	}

	reader := bufio.NewReader(upResp.Body)
	return s.readUpstreamStream(ctx, w, reader, flusher, prepared, accumulator, displayAdapter, contexts, pendingNotice, &finalized)
}

// readUpstreamStream reads SSE lines from the upstream response body and streams
// rewritten chunks to the client. This is the long-running part of streaming.
func (s *ProxyServer) readUpstreamStream(
	ctx context.Context,
	w http.ResponseWriter,
	reader *bufio.Reader,
	flusher http.Flusher,
	prepared *models.PreparedRequest,
	accumulator *streaming.StreamAccumulator,
	displayAdapter *streaming.CursorReasoningDisplayAdapter,
	contexts []models.ResponseContext,
	pendingNotice string,
	finalized *bool,
) *proxyResponseResult {

	ctx, span := s.tracer.Start(ctx, "readUpstreamStream")
	defer span.End()

	var (
		usage        *models.Usage
		chunks       int
		bytesWritten int64
		tRead        time.Duration
		tProcess     time.Duration
		tWrite       time.Duration
	)

	for {
		beforeRead := time.Now()
		line, err := reader.ReadString('\n')
		tRead += time.Since(beforeRead)
		if err != nil {
			if err == io.EOF {
				break
			}
			log.Printf("upstream streaming read failed: %v", err)
			return &proxyResponseResult{sent: false, usage: usage}
		}

		result := s.processSSELine(ctx, line, w, flusher, prepared, accumulator, displayAdapter, contexts, pendingNotice)
		tProcess += result.processTime
		tWrite += result.writeTime
		if result.chunkUsage != nil {
			usage = result.chunkUsage
		}
		pendingNotice = result.notice
		chunks++
		bytesWritten += result.bytesWritten
		if result.writeErr != nil {
			return &proxyResponseResult{sent: false, usage: usage}
		}
		if result.fin {
			*finalized = true
			break
		}
	}

	span.AddEvent("stream.stats", otel_trace.WithAttributes(
		attribute.Int("chunks", chunks),
		attribute.Int64("bytes_written", bytesWritten),
		attribute.Int64("read_wait_ms", tRead.Milliseconds()),
		attribute.Int64("process_ms", tProcess.Milliseconds()),
		attribute.Int64("write_ms", tWrite.Milliseconds()),
	))

	return &proxyResponseResult{sent: true, usage: usage}
}

// cleanupStreamingBatch finalizes the streaming batch: stores pending reasoning
// contexts (if the stream was not cleanly finalized) and ends the batch.
func (s *ProxyServer) cleanupStreamingBatch(
	ctx context.Context,
	accumulator *streaming.StreamAccumulator,
	contexts []models.ResponseContext,
	cacheNamespace string,
	finalized *bool,
) {
	ctx, span := s.tracer.Start(ctx, "cleanupStreamingBatch")
	defer span.End()

	var tStoreReasoning time.Duration
	var tEndBatch time.Duration

	if !*finalized && s.reasoningStore != nil {
		beforeStore := time.Now()
		for _, rc := range contexts {
			accumulator.StoreReasoning(ctx, s.reasoningStore, rc.Scope, cacheNamespace, rc.Messages...)
		}
		tStoreReasoning = time.Since(beforeStore)
	}

	if s.reasoningStore != nil {
		beforeEndBatch := time.Now()
		if _, err := s.reasoningStore.EndBatch(ctx); err != nil {
			log.Printf("error ending streaming batch: %v", err)
		}
		tEndBatch = time.Since(beforeEndBatch)
	}

	span.SetAttributes(
		attribute.Int64("cleanup.store_ms", tStoreReasoning.Milliseconds()),
		attribute.Int64("cleanup.end_batch_ms", tEndBatch.Milliseconds()),
	)
}

func (s *ProxyServer) rewriteSSELine(
	ctx context.Context,
	line string,
	originalModel string,
	accumulator *streaming.StreamAccumulator,
	cacheNamespace string,
	contexts []models.ResponseContext,
	displayAdapter *streaming.CursorReasoningDisplayAdapter,
	recoveryNotice string,
) (string, bool, string, *models.Usage) {
	stripped := strings.TrimSpace(line)
	if !strings.HasPrefix(stripped, "data:") {
		return line, false, recoveryNotice, nil
	}

	data := strings.TrimSpace(stripped[5:])
	if data == "[DONE]" {
		var prefix string
		if displayAdapter == nil {
			if recoveryNotice != "" {
				chunk := transform.RecoveryNoticeChunk(originalModel)
				encoded, _ := transform.SSEEncode(chunk)
				prefix = string(encoded)
			}
			return prefix + "data: [DONE]\n\n", true, "", nil
		}

		closingChunk := displayAdapter.FlushChunk(originalModel)
		if closingChunk != nil {
			encoded, _ := transform.SSEEncode(closingChunk)
			prefix += string(encoded)
		}
		if recoveryNotice != "" {
			chunk := transform.RecoveryNoticeChunk(originalModel)
			encoded, _ := transform.SSEEncode(chunk)
			prefix += string(encoded)
		}
		return prefix + "data: [DONE]\n\n", true, "", nil
	}

	var chunk map[string]any
	if err := json.Unmarshal([]byte(data), &chunk); err != nil {
		return line, false, recoveryNotice, nil
	}

	if recoveryNotice != "" && transform.InjectRecoveryNotice(chunk, recoveryNotice) {
		recoveryNotice = ""
	}

	accumulator.IngestChunk(chunk)

	if s.reasoningStore != nil {
		for _, rc := range contexts {
			accumulator.StoreReadyReasoning(ctx, s.reasoningStore, rc.Scope, cacheNamespace, rc.Messages)
		}
	}

	var chunkUsage *models.Usage
	if usageRaw, ok := chunk["usage"].(map[string]any); ok {
		chunkUsage = usageFromMap(usageRaw)
	}

	if displayAdapter != nil {
		displayAdapter.RewriteChunk(chunk)
	}

	if model, ok := chunk["model"].(string); ok && model != "" {
		chunk["model"] = originalModel
	}

	encoded, err := json.Marshal(chunk)
	if err != nil {
		return line, false, recoveryNotice, chunkUsage
	}

	ending := "\n"
	if strings.HasSuffix(line, "\r\n") {
		ending = "\r\n"
	}
	return fmt.Sprintf("data: %s%s", string(encoded), ending), false, recoveryNotice, chunkUsage
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func extractBearerToken(authHeader string) string {
	parts := strings.SplitN(strings.TrimSpace(authHeader), " ", 2)
	if len(parts) != 2 || strings.ToLower(parts[0]) != "bearer" || strings.TrimSpace(parts[1]) == "" {
		return ""
	}
	return "Bearer " + strings.TrimSpace(parts[1])
}

func usageFromBody(body []byte) *models.Usage {
	var resp models.ChatCompletionResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil
	}
	return resp.Usage
}

func usageFromMap(m map[string]any) *models.Usage {
	usage := &models.Usage{}
	if v, ok := m["prompt_tokens"].(float64); ok {
		usage.PromptTokens = int(v)
	}
	if v, ok := m["completion_tokens"].(float64); ok {
		usage.CompletionTokens = int(v)
	}
	if v, ok := m["total_tokens"].(float64); ok {
		usage.TotalTokens = int(v)
	}
	if details, ok := m["completion_tokens_details"].(map[string]any); ok {
		cd := &models.CompletionTokensDetails{}
		if v, ok := details["reasoning_tokens"].(float64); ok {
			cd.ReasoningTokens = int(v)
		}
		usage.CompletionTokensDetails = cd
	}
	if v, ok := m["prompt_cache_hit_tokens"].(float64); ok {
		usage.PromptCacheHitTokens = int(v)
	}
	if v, ok := m["prompt_cache_miss_tokens"].(float64); ok {
		usage.PromptCacheMissTokens = int(v)
	}
	return usage
}

func headerMap(h http.Header) map[string]string {
	result := make(map[string]string, len(h))
	for k, v := range h {
		if len(v) > 0 {
			result[k] = v[0]
		}
	}
	return result
}

func responseHeaders(resp *http.Response) map[string]string {
	if resp == nil {
		return nil
	}
	result := make(map[string]string, len(resp.Header))
	for k, v := range resp.Header {
		if len(v) > 0 {
			result[k] = v[0]
		}
	}
	return result

}
