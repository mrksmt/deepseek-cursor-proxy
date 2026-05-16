package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/sqlitedialect"
	"github.com/uptrace/bun/driver/sqliteshim"
	"github.com/uptrace/bun/extra/bunotel"

	"github.com/mrksmt/deepseek-cursor-proxy/internal/models"
	"github.com/mrksmt/deepseek-cursor-proxy/internal/otel_ctx"
)

// ReasoningStore provides a SQLite-backed cache for reasoning_content.
type ReasoningStore struct {
	db            *bun.DB
	maxAge        time.Duration
	maxRows       int
	putCount      atomic.Int64
	pruneInterval int64

	mu       sync.Mutex
	closed   bool
	batchBuf []batchItem
	batching bool
}

type batchItem struct {
	key         string
	reasoning   string
	messageJSON string
	createdAt   time.Time
}

// NewReasoningStore creates a new ReasoningStore backed by SQLite via bunDB.
func NewReasoningStore(
	ctx context.Context,
	dbPath string,
	maxAgeSeconds int,
	maxRows int,
) (*ReasoningStore, error) {

	// Ensure parent directory exists
	if dbPath != ":memory:" {
		dir := filepath.Dir(dbPath)
		if err := os.MkdirAll(dir, 0700); err != nil {
			return nil, fmt.Errorf("cannot create store directory %s: %w", dir, err)
		}
	}

	sqliteDB, err := sql.Open(sqliteshim.ShimName, dbPath)
	if err != nil {
		return nil, fmt.Errorf("cannot open SQLite database: %w", err)
	}

	// Configure connection
	sqliteDB.SetMaxOpenConns(1)
	sqliteDB.SetMaxIdleConns(1)
	sqliteDB.SetConnMaxLifetime(0)

	db := bun.NewDB(sqliteDB, sqlitedialect.New())
	db.AddQueryHook(bunotel.NewQueryHook(
		bunotel.WithDBName("reasoning_cache"),
	))

	// Enable WAL mode for concurrent reads
	if _, err := db.ExecContext(ctx, "PRAGMA journal_mode=WAL"); err != nil {
		return nil, fmt.Errorf("cannot enable WAL mode: %w", err)
	}
	if _, err := db.ExecContext(ctx, "PRAGMA busy_timeout=5000"); err != nil {
		return nil, fmt.Errorf("cannot set busy timeout: %w", err)
	}

	// Create table
	if _, err := db.NewCreateTable().
		Model((*models.ReasoningCacheEntry)(nil)).
		IfNotExists().
		Exec(ctx); err != nil {
		return nil, fmt.Errorf("cannot create reasoning_cache table: %w", err)
	}

	// Create index
	if _, err := db.ExecContext(ctx,
		"CREATE INDEX IF NOT EXISTS idx_reasoning_cache_created_at ON reasoning_cache(created_at)",
	); err != nil {
		return nil, fmt.Errorf("cannot create index: %w", err)
	}

	store := &ReasoningStore{
		db:            db,
		maxAge:        time.Duration(maxAgeSeconds) * time.Second,
		maxRows:       maxRows,
		pruneInterval: 100,
	}

	// Initial pruning
	if err := store.Prune(ctx); err != nil {
		// Non-fatal
		_ = err
	}

	return store, nil
}

// Close closes the database connection.
func (s *ReasoningStore) Close(ctx context.Context) error {
	ctx, span := otel_ctx.Tracer(ctx).Start(ctx, "store.Close")
	defer span.End()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	_ = s.pruneLocked(context.Background())
	return s.db.Close()
}

// BeginBatch starts buffering put() calls for the current streaming request.
// Only one batch can be active at a time (SQLite is single-connection).
func (s *ReasoningStore) BeginBatch() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.batchBuf = make([]batchItem, 0, 32)
	s.batching = true
}

// EndBatch flushes buffered puts and returns the number of rows written.
func (s *ReasoningStore) EndBatch(ctx context.Context) (int, error) {

	ctx, span := otel_ctx.Tracer(ctx).Start(ctx, "store.EndBatch")
	defer span.End()

	s.mu.Lock()
	items := s.batchBuf
	s.batchBuf = nil
	s.batching = false
	s.mu.Unlock()

	if len(items) == 0 {
		return 0, nil
	}
	now := time.Now()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("cannot begin batch transaction: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	for _, item := range items {
		if _, err := tx.NewInsert().
			Model(&models.ReasoningCacheEntry{
				Key:         item.key,
				Reasoning:   item.reasoning,
				MessageJSON: item.messageJSON,
				CreatedAt:   now,
			}).
			On("CONFLICT(key) DO UPDATE SET reasoning = excluded.reasoning, message_json = excluded.message_json, created_at = excluded.created_at").
			Exec(ctx); err != nil {
			return 0, fmt.Errorf("cannot insert batch item: %w", err)
		}
	}

	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("cannot commit batch transaction: %w", err)
	}

	putTotal := s.putCount.Add(int64(len(items)))
	if putTotal%s.pruneInterval == 0 {
		_ = s.Prune(ctx)
	}

	return len(items), nil
}

// Put stores a reasoning cache entry.
func (s *ReasoningStore) Put(
	ctx context.Context,
	key,
	reasoning string,
	message map[string]any,
) error {

	ctx, span := otel_ctx.Tracer(ctx).Start(ctx, "store.Put")
	defer span.End()
	if reasoning == "" {
		return nil
	}
	msgJSON, err := json.Marshal(message)
	if err != nil {
		return fmt.Errorf("cannot marshal message: %w", err)
	}

	s.mu.Lock()
	if s.batching {
		s.batchBuf = append(s.batchBuf, batchItem{
			key:         key,
			reasoning:   reasoning,
			messageJSON: string(msgJSON),
			createdAt:   time.Now(),
		})
		s.mu.Unlock()
		return nil
	}
	s.mu.Unlock()

	return s.putDirect(ctx, key, reasoning, string(msgJSON))
}

// Get retrieves reasoning_content by key.
func (s *ReasoningStore) Get(
	ctx context.Context,
	key string,
) (string, error) {

	ctx, span := otel_ctx.Tracer(ctx).Start(ctx, "store.Get")
	defer span.End()
	entry := new(models.ReasoningCacheEntry)
	err := s.db.NewSelect().
		Model(entry).
		Where("key = ?", key).
		Limit(1).
		Scan(ctx)
	if err != nil {
		if err == sql.ErrNoRows {
			return "", nil
		}
		return "", fmt.Errorf("cannot get reasoning key %s: %w", key, err)
	}
	return entry.Reasoning, nil
}

// StoreAssistantMessage stores reasoning for an assistant message under multiple keys.
func (s *ReasoningStore) StoreAssistantMessage(
	ctx context.Context,
	message map[string]any,
	scope, cacheNamespace string,
	priorMessages []models.Message,
) (int, error) {

	ctx, span := otel_ctx.Tracer(ctx).Start(ctx, "store.StoreAssistantMessage")
	defer span.End()
	if message["role"] != "assistant" {
		return 0, nil
	}
	reasoning, ok := message["reasoning_content"].(string)
	if !ok || reasoning == "" {
		return 0, nil
	}

	keys := scopedReasoningKeys(message, scope)
	if cacheNamespace != "" && priorMessages != nil {
		keys = append(keys, portableReasoningKeys(message, cacheNamespace, priorMessages)...)
	}

	// Deduplicate
	seen := make(map[string]struct{})
	unique := make([]string, 0, len(keys))
	for _, k := range keys {
		if _, ok := seen[k]; !ok {
			seen[k] = struct{}{}
			unique = append(unique, k)
		}
	}

	for _, k := range unique {
		if err := s.Put(ctx, k, reasoning, message); err != nil {
			return 0, err
		}
	}
	return len(unique), nil
}

// LookupForMessage searches for reasoning_content using multiple lookup keys.
func (s *ReasoningStore) LookupForMessage(
	ctx context.Context,
	message map[string]any,
	scope, cacheNamespace string,
	priorMessages []models.Message,
) (string, error) {

	ctx, span := otel_ctx.Tracer(ctx).Start(ctx, "store.LookupForMessage")
	defer span.End()
	keys := scopedReasoningKeys(message, scope)
	if cacheNamespace != "" && len(priorMessages) > 0 {
		keys = append(keys, portableReasoningKeys(message, cacheNamespace, priorMessages)...)
	}

	for _, key := range keys {
		reasoning, err := s.Get(ctx, key)
		if err != nil {
			return "", err
		}
		if reasoning != "" {
			return reasoning, nil
		}
	}
	return "", nil
}

// BackfillPortableAliases writes portable cache keys for a message.
func (s *ReasoningStore) BackfillPortableAliases(
	ctx context.Context,
	message map[string]any,
	reasoning, cacheNamespace string,
	priorMessages []models.Message,
) (int, error) {

	ctx, span := otel_ctx.Tracer(ctx).Start(ctx, "store.BackfillPortableAliases")
	defer span.End()
	if reasoning == "" {
		return 0, nil
	}
	keys := portableReasoningKeys(message, cacheNamespace, priorMessages)
	if len(keys) == 0 {
		return 0, nil
	}

	msgWithReasoning := make(map[string]any)
	maps.Copy(msgWithReasoning, message)
	msgWithReasoning["reasoning_content"] = reasoning

	deduped := make([]string, 0, len(keys))
	seen := make(map[string]struct{})
	for _, k := range keys {
		if _, ok := seen[k]; !ok {
			seen[k] = struct{}{}
			deduped = append(deduped, k)
		}
	}

	for _, k := range deduped {
		if err := s.Put(ctx, k, reasoning, msgWithReasoning); err != nil {
			return 0, err
		}
	}
	return len(deduped), nil
}

// Clear removes all entries from the cache and returns the count.
func (s *ReasoningStore) Clear(ctx context.Context) (int, error) {

	ctx, span := otel_ctx.Tracer(ctx).Start(ctx, "store.Clear")
	defer span.End()
	var count int
	err := s.db.NewSelect().
		Model((*models.ReasoningCacheEntry)(nil)).
		ColumnExpr("COUNT(*)").
		Scan(ctx, &count)
	if err != nil {
		return 0, fmt.Errorf("cannot count cache entries: %w", err)
	}

	_, err = s.db.NewDelete().
		Model((*models.ReasoningCacheEntry)(nil)).
		Where("1=1").
		Exec(ctx)
	if err != nil {
		return 0, fmt.Errorf("cannot clear cache: %w", err)
	}
	return count, nil
}

// Prune removes expired entries and enforces row limits.
func (s *ReasoningStore) Prune(ctx context.Context) error {

	ctx, span := otel_ctx.Tracer(ctx).Start(ctx, "store.Prune")
	defer span.End()
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.pruneLocked(ctx)
}

func (s *ReasoningStore) pruneLocked(ctx context.Context) error {

	ctx, span := otel_ctx.Tracer(ctx).Start(ctx, "store.pruneLocked")
	defer span.End()

	// Age-based pruning
	if s.maxAge > 0 {
		cutoff := time.Now().Add(-s.maxAge)
		_, err := s.db.NewDelete().
			Model((*models.ReasoningCacheEntry)(nil)).
			Where("created_at < ?", cutoff).
			Exec(ctx)
		if err != nil {
			return fmt.Errorf("cannot prune by age: %w", err)
		}
	}

	// Row count pruning
	if s.maxRows > 0 {
		_, err := s.db.NewRaw(`
			DELETE FROM reasoning_cache
			WHERE key NOT IN (
				SELECT key FROM reasoning_cache
				ORDER BY created_at DESC
				LIMIT ?
			)
		`, s.maxRows).Exec(ctx)
		if err != nil {
			return fmt.Errorf("cannot prune by row count: %w", err)
		}
	}

	return nil
}

func (s *ReasoningStore) putDirect(
	ctx context.Context,
	key, reasoning,
	messageJSON string,
) error {
	
	ctx, span := otel_ctx.Tracer(ctx).Start(ctx, "store.putDirect")
	defer span.End()

	_, err := s.db.NewInsert().
		Model(&models.ReasoningCacheEntry{
			Key:         key,
			Reasoning:   reasoning,
			MessageJSON: messageJSON,
			CreatedAt:   time.Now().UTC(),
		}).
		On("CONFLICT(key) DO UPDATE SET reasoning = excluded.reasoning, message_json = excluded.message_json, created_at = excluded.created_at").
		Exec(ctx)
	if err != nil {
		return fmt.Errorf("cannot put reasoning key: %w", err)
	}

	putTotal := s.putCount.Add(1)
	if putTotal%s.pruneInterval == 0 {
		_ = s.pruneLocked(ctx)
	}
	return nil
}

func sha256Hex(data string) string {
	h := sha256.Sum256([]byte(data))
	return fmt.Sprintf("%x", h)
}

func normalizeToolCall(tc map[string]any) map[string]any {
	function, _ := tc["function"].(map[string]any)
	if function == nil {
		function = make(map[string]any)
	}
	arguments, _ := function["arguments"].(string)
	if arguments == "" {
		if args, ok := function["arguments"]; ok {
			if b, err := json.Marshal(args); err == nil {
				arguments = string(b)
			}
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

func toolCallSignature(tc map[string]any) string {
	normalized := normalizeToolCall(tc)
	delete(normalized, "id")
	canonical, _ := json.Marshal(normalized)
	return sha256Hex(string(canonical))
}

func toolCallIDs(msg map[string]any) []string {
	tcs, _ := msg["tool_calls"].([]any)
	ids := make([]string, 0)
	for _, tc := range tcs {
		if tcMap, ok := tc.(map[string]any); ok {
			if id, ok := tcMap["id"].(string); ok && id != "" {
				ids = append(ids, id)
			}
		}
	}
	return ids
}

func toolCallNames(msg map[string]any) []string {
	tcs, _ := msg["tool_calls"].([]any)
	names := make([]string, 0)
	for _, tc := range tcs {
		if tcMap, ok := tc.(map[string]any); ok {
			if fn, ok := tcMap["function"].(map[string]any); ok {
				if name, ok := fn["name"].(string); ok && name != "" {
					names = append(names, name)
				}
			}
		}
	}
	return names
}

func messageSignature(msg map[string]any) string {
	tcs, _ := msg["tool_calls"].([]any)
	normalizedTCs := make([]map[string]any, 0)
	for _, tc := range tcs {
		if tcMap, ok := tc.(map[string]any); ok {
			normalizedTCs = append(normalizedTCs, normalizeToolCall(tcMap))
		}
	}

	content, _ := msg["content"].(string)
	payload := map[string]any{
		"content":    content,
		"tool_calls": normalizedTCs,
	}
	canonical, _ := json.Marshal(payload)
	return sha256Hex(string(canonical))
}

func scopedReasoningKeys(msg map[string]any, scope string) []string {
	keys := []string{fmt.Sprintf("scope:%s:signature:%s", scope, messageSignature(msg))}

	for _, id := range toolCallIDs(msg) {
		keys = append(keys, fmt.Sprintf("scope:%s:tool_call:%s", scope, id))
	}

	tcs, _ := msg["tool_calls"].([]any)
	for _, tc := range tcs {
		if tcMap, ok := tc.(map[string]any); ok {
			keys = append(keys, fmt.Sprintf("scope:%s:tool_call_signature:%s", scope, toolCallSignature(tcMap)))
		}
	}

	for _, name := range toolCallNames(msg) {
		keys = append(keys, fmt.Sprintf("scope:%s:tool_name:%s", scope, name))
	}

	return keys
}

func turnContextSignature(priorMsgs []models.Message) string {
	lastUserIdx := -1
	for i := len(priorMsgs) - 1; i >= 0; i-- {
		if priorMsgs[i].Role == "user" {
			lastUserIdx = i
			break
		}
	}
	startIdx := 0
	if lastUserIdx != -1 {
		startIdx = lastUserIdx
		for startIdx > 0 && priorMsgs[startIdx-1].Role == "user" {
			startIdx--
		}
	}

	contextMsgs := make([]map[string]any, 0)
	for _, msg := range priorMsgs[startIdx:] {
		if msg.Role == "system" {
			continue
		}
		contextMsgs = append(contextMsgs, map[string]any{
			"role":    msg.Role,
			"content": msg.Content,
			"name":    msg.Name,
		})
	}

	canonical, _ := json.Marshal(contextMsgs)
	return sha256Hex(string(canonical))
}

func portableReasoningKeys(msg map[string]any, cacheNamespace string, priorMsgs []models.Message) []string {
	if cacheNamespace == "" || len(priorMsgs) == 0 {
		return nil
	}

	turnSig := turnContextSignature(priorMsgs)
	keys := []string{
		fmt.Sprintf("namespace:%s:turn:%s:signature:%s", cacheNamespace, turnSig, messageSignature(msg)),
	}

	for _, id := range toolCallIDs(msg) {
		keys = append(keys, fmt.Sprintf("namespace:%s:turn:%s:tool_call:%s", cacheNamespace, turnSig, id))
	}

	tcs, _ := msg["tool_calls"].([]any)
	for _, tc := range tcs {
		if tcMap, ok := tc.(map[string]any); ok {
			keys = append(keys, fmt.Sprintf("namespace:%s:turn:%s:tool_call_signature:%s", cacheNamespace, turnSig, toolCallSignature(tcMap)))
		}
	}

	for _, name := range toolCallNames(msg) {
		keys = append(keys, fmt.Sprintf("namespace:%s:turn:%s:tool_name:%s", cacheNamespace, turnSig, name))
	}

	return keys
}

// ComputeReasoningCacheNamespace computes the cache namespace from config parameters.
func ComputeReasoningCacheNamespace(baseURL, model, thinking string, reasoningEffort string, authorization string) string {
	authHash := ""
	if authorization != "" {
		authHash = sha256Hex(authorization)
	}
	modelFamily := model
	if model == "deepseek-v4-pro" || model == "deepseek-v4-flash" {
		modelFamily = "deepseek-v4"
	}
	payload := map[string]any{
		"base_url":           baseURL,
		"model":              modelFamily,
		"thinking":           thinking,
		"reasoning_effort":   reasoningEffort,
		"authorization_hash": authHash,
	}
	canonical, _ := json.Marshal(payload)
	return sha256Hex(string(canonical))
}
