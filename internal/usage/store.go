package usage

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	internallogging "github.com/router-for-me/CLIProxyAPI/v7/internal/logging"
	coreusage "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
	_ "modernc.org/sqlite"
)

type RequestDetail struct {
	Timestamp time.Time      `json:"timestamp"`
	LatencyMs int64          `json:"latency_ms"`
	Source    string         `json:"source"`
	AuthIndex string         `json:"auth_index"`
	Tokens    TokenStats     `json:"tokens"`
	Thinking  *ThinkingStats `json:"thinking,omitempty"`
	Failed    bool           `json:"failed"`
}

type ThinkingStats struct {
	Intensity string `json:"intensity,omitempty"`
}

type TokenStats struct {
	InputTokens         int64 `json:"input_tokens"`
	OutputTokens        int64 `json:"output_tokens"`
	ReasoningTokens     int64 `json:"reasoning_tokens"`
	CachedTokens        int64 `json:"cached_tokens"`
	CacheReadTokens     int64 `json:"cache_read_tokens"`
	CacheCreationTokens int64 `json:"cache_creation_tokens"`
	TotalTokens         int64 `json:"total_tokens"`
}

type StatisticsSnapshot struct {
	TotalRequests int64 `json:"total_requests"`
	SuccessCount  int64 `json:"success_count"`
	FailureCount  int64 `json:"failure_count"`
	TotalTokens   int64 `json:"total_tokens"`

	APIs map[string]APISnapshot `json:"apis"`

	RequestsByDay  map[string]int64 `json:"requests_by_day"`
	RequestsByHour map[string]int64 `json:"requests_by_hour"`
	TokensByDay    map[string]int64 `json:"tokens_by_day"`
	TokensByHour   map[string]int64 `json:"tokens_by_hour"`
}

type Revision struct {
	LatestID  int64 `json:"latest_id"`
	TotalRows int64 `json:"total_rows"`
}

type APISnapshot struct {
	TotalRequests int64                    `json:"total_requests"`
	TotalTokens   int64                    `json:"total_tokens"`
	Models        map[string]ModelSnapshot `json:"models"`
}

type ModelSnapshot struct {
	TotalRequests int64           `json:"total_requests"`
	TotalTokens   int64           `json:"total_tokens"`
	Details       []RequestDetail `json:"details"`
}

type MergeResult struct {
	Added   int64 `json:"added"`
	Skipped int64 `json:"skipped"`
}

type usageEvent struct {
	APIKey   string
	Model    string
	Detail   RequestDetail
	EventKey string
}

type Store struct {
	db      *sql.DB
	dialect string

	// The management page polls frequently, while the usage table can contain
	// a large history. Keep the aggregated snapshot in memory and only rebuild
	// it after a cold start or an import. Normal request writes update the
	// cached snapshot incrementally.
	snapshotMu    sync.RWMutex
	snapshot      StatisticsSnapshot
	snapshotReady bool
	revision      Revision
	revisionReady bool
}

func Open(ctx context.Context, configPath string) (*Store, error) {
	driver := strings.ToLower(strings.TrimSpace(os.Getenv("USAGE_DB_DRIVER")))
	dsn := strings.TrimSpace(os.Getenv("USAGE_DB_DSN"))
	if driver == "" {
		driver = "sqlite"
	}
	if driver == "postgres" || driver == "postgresql" {
		if dsn == "" {
			dsn = strings.TrimSpace(os.Getenv("DATABASE_URL"))
		}
		if dsn == "" {
			return nil, errors.New("USAGE_DB_DSN is required for PostgreSQL usage storage")
		}
		db, err := sql.Open("pgx", dsn)
		if err != nil {
			return nil, fmt.Errorf("open PostgreSQL usage database: %w", err)
		}
		store := &Store{db: db, dialect: "postgres"}
		if err = store.initialize(ctx); err != nil {
			_ = db.Close()
			return nil, err
		}
		return store, nil
	}

	if dsn == "" {
		dsn = "sql.db"
	}
	if !filepath.IsAbs(dsn) && configPath != "" {
		dsn = filepath.Join(filepath.Dir(configPath), dsn)
	}
	if dir := filepath.Dir(dsn); dir != "." && dir != "" {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, fmt.Errorf("create usage database directory: %w", err)
		}
	}
	db, err := sql.Open("sqlite", dsn+"?_busy_timeout=5000&_journal_mode=WAL")
	if err != nil {
		return nil, fmt.Errorf("open SQLite usage database: %w", err)
	}
	store := &Store{db: db, dialect: "sqlite"}
	if err = store.initialize(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	return store, nil
}

func (s *Store) initialize(ctx context.Context) error {
	if s == nil || s.db == nil {
		return errors.New("usage database is not initialized")
	}
	if err := s.db.PingContext(ctx); err != nil {
		return fmt.Errorf("ping usage database: %w", err)
	}
	idType := "INTEGER PRIMARY KEY AUTOINCREMENT"
	timestampType := "TEXT NOT NULL"
	if s.dialect == "postgres" {
		idType = "BIGSERIAL PRIMARY KEY"
		timestampType = "TIMESTAMPTZ NOT NULL"
	}
	statement := fmt.Sprintf(`CREATE TABLE IF NOT EXISTS usage_events (
id %s,
event_key TEXT NOT NULL UNIQUE,
api_key TEXT NOT NULL,
model TEXT NOT NULL,
timestamp %s,
latency_ms BIGINT NOT NULL DEFAULT 0,
source TEXT NOT NULL DEFAULT '',
auth_index TEXT NOT NULL DEFAULT '',
reasoning_effort TEXT NOT NULL DEFAULT '',
input_tokens BIGINT NOT NULL DEFAULT 0,
output_tokens BIGINT NOT NULL DEFAULT 0,
reasoning_tokens BIGINT NOT NULL DEFAULT 0,
cached_tokens BIGINT NOT NULL DEFAULT 0,
cache_read_tokens BIGINT NOT NULL DEFAULT 0,
cache_creation_tokens BIGINT NOT NULL DEFAULT 0,
total_tokens BIGINT NOT NULL DEFAULT 0,
failed BOOLEAN NOT NULL DEFAULT FALSE
)`, idType, timestampType)
	if _, err := s.db.ExecContext(ctx, statement); err != nil {
		return fmt.Errorf("initialize official usage schema: %w", err)
	}
	if err := s.ensureReasoningEffortColumn(ctx); err != nil {
		return fmt.Errorf("migrate usage reasoning effort: %w", err)
	}
	if err := s.ensureCacheTokenColumns(ctx); err != nil {
		return fmt.Errorf("migrate usage cache token columns: %w", err)
	}
	for _, index := range []string{
		"CREATE INDEX IF NOT EXISTS idx_usage_events_timestamp ON usage_events(timestamp)",
		"CREATE INDEX IF NOT EXISTS idx_usage_events_api_model ON usage_events(api_key, model)",
		"CREATE INDEX IF NOT EXISTS idx_usage_events_auth_timestamp ON usage_events(auth_index, timestamp)",
	} {
		if _, err := s.db.ExecContext(ctx, index); err != nil {
			return fmt.Errorf("initialize usage index: %w", err)
		}
	}
	if err := s.migrateLegacyRecords(ctx); err != nil {
		return fmt.Errorf("migrate legacy usage records: %w", err)
	}
	if s.dialect == "sqlite" {
		// Apply this after the legacy migration, which reads and writes concurrently.
		// At runtime a single connection makes writes immediately visible to snapshots.
		s.db.SetMaxOpenConns(1)
		s.db.SetMaxIdleConns(1)
	} else {
		s.db.SetMaxOpenConns(8)
		s.db.SetMaxIdleConns(4)
	}
	return nil
}

func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

// Revision returns a lightweight marker that changes whenever usage rows change.
func (s *Store) Revision(ctx context.Context) (Revision, error) {
	if s == nil || s.db == nil {
		return Revision{}, errors.New("usage database is not initialized")
	}
	// PostgreSQL deployments may share the database across processes, so do
	// not serve an in-process revision cache there.
	if s.dialect == "postgres" {
		return s.queryRevision(ctx)
	}
	s.snapshotMu.RLock()
	if s.revisionReady {
		revision := s.revision
		s.snapshotMu.RUnlock()
		return revision, nil
	}
	s.snapshotMu.RUnlock()

	s.snapshotMu.Lock()
	defer s.snapshotMu.Unlock()
	if s.revisionReady {
		return s.revision, nil
	}
	revision, err := s.queryRevision(ctx)
	if err != nil {
		return Revision{}, err
	}
	s.revision = revision
	s.revisionReady = true
	return revision, nil
}

func (s *Store) queryRevision(ctx context.Context) (Revision, error) {
	var revision Revision
	if err := s.db.QueryRowContext(
		ctx,
		`SELECT COALESCE(MAX(id), 0), COUNT(*) FROM usage_events`,
	).Scan(&revision.LatestID, &revision.TotalRows); err != nil {
		return Revision{}, fmt.Errorf("query usage revision: %w", err)
	}
	return revision, nil
}

func (s *Store) Record(ctx context.Context, record coreusage.Record) error {
	if s == nil || s.db == nil {
		return errors.New("usage database is not initialized")
	}
	timestamp := record.RequestedAt
	if timestamp.IsZero() {
		timestamp = time.Now()
	}
	apiKey := strings.TrimSpace(record.APIKey)
	if apiKey == "" {
		apiKey = resolveAPIIdentifier(ctx, record)
	}
	model := strings.TrimSpace(record.Model)
	if model == "" {
		model = "unknown"
	}
	failed := record.Failed
	if !failed {
		failed = !resolveSuccess(ctx)
	}
	detail := RequestDetail{
		Timestamp: timestamp,
		LatencyMs: normaliseLatency(record.Latency),
		Source:    record.Source,
		AuthIndex: record.AuthIndex,
		Tokens:    normaliseDetail(record.Detail),
		Thinking:  resolveThinking(record.ReasoningEffort, record.Detail.ReasoningTokens),
		Failed:    failed,
	}
	event := usageEvent{APIKey: apiKey, Model: model, Detail: detail}
	event.EventKey = dedupKey(apiKey, model, detail)

	// Serialize the write with a possible cold-cache rebuild. Otherwise a
	// rebuild could observe the inserted row before Record updates the cache and
	// aggregate the same event twice.
	s.snapshotMu.Lock()
	defer s.snapshotMu.Unlock()
	added, err := s.insertEvent(ctx, s.db, event)
	if err != nil || !added {
		return err
	}
	if s.snapshotReady {
		aggregateEvent(&s.snapshot, event)
	}
	s.revisionReady = false
	return nil
}

func (s *Store) Snapshot(ctx context.Context) (StatisticsSnapshot, error) {
	if s == nil || s.db == nil {
		return emptySnapshot(), errors.New("usage database is not initialized")
	}
	if s.dialect == "postgres" {
		return s.loadSnapshot(ctx)
	}

	s.snapshotMu.RLock()
	if s.snapshotReady {
		snapshot := cloneSnapshot(s.snapshot)
		s.snapshotMu.RUnlock()
		return snapshot, nil
	}
	s.snapshotMu.RUnlock()

	// Serialize the first rebuild so concurrent management requests do not all
	// scan the same table at once.
	s.snapshotMu.Lock()
	defer s.snapshotMu.Unlock()
	if s.snapshotReady {
		return cloneSnapshot(s.snapshot), nil
	}

	result, err := s.loadSnapshot(ctx)
	if err != nil {
		return emptySnapshot(), err
	}
	s.snapshot = result
	s.snapshotReady = true
	return cloneSnapshot(result), nil
}

func (s *Store) loadSnapshot(ctx context.Context) (StatisticsSnapshot, error) {
	return s.querySnapshot(ctx, nil)
}

// SnapshotSince returns a range-limited aggregate without materializing the
// complete historical dataset. The timestamp index keeps dashboard refreshes
// proportional to the selected window instead of total database size.
func (s *Store) SnapshotSince(ctx context.Context, since time.Time) (StatisticsSnapshot, error) {
	if s == nil || s.db == nil {
		return emptySnapshot(), errors.New("usage database is not initialized")
	}
	since = since.UTC()
	return s.querySnapshot(ctx, &since)
}

func (s *Store) querySnapshot(ctx context.Context, since *time.Time) (StatisticsSnapshot, error) {
	result := emptySnapshot()
	query := `SELECT api_key,model,timestamp,latency_ms,source,auth_index,reasoning_effort,input_tokens,output_tokens,reasoning_tokens,cached_tokens,cache_read_tokens,cache_creation_tokens,total_tokens,failed FROM usage_events`
	args := make([]any, 0, 1)
	if since != nil {
		query += ` WHERE timestamp >= ?`
		value := any(*since)
		if s.dialect == "sqlite" {
			value = since.Format(time.RFC3339Nano)
		}
		args = append(args, value)
	}
	query += ` ORDER BY timestamp ASC,id ASC`
	rows, err := s.db.QueryContext(ctx, s.bind(query), args...)
	if err != nil {
		return result, err
	}
	defer rows.Close()
	for rows.Next() {
		var event usageEvent
		var timestampRaw any
		var reasoningEffort string
		if err = rows.Scan(
			&event.APIKey,
			&event.Model,
			&timestampRaw,
			&event.Detail.LatencyMs,
			&event.Detail.Source,
			&event.Detail.AuthIndex,
			&reasoningEffort,
			&event.Detail.Tokens.InputTokens,
			&event.Detail.Tokens.OutputTokens,
			&event.Detail.Tokens.ReasoningTokens,
			&event.Detail.Tokens.CachedTokens,
			&event.Detail.Tokens.CacheReadTokens,
			&event.Detail.Tokens.CacheCreationTokens,
			&event.Detail.Tokens.TotalTokens,
			&event.Detail.Failed,
		); err != nil {
			return result, err
		}
		event.Detail.Timestamp = parseCreatedAt(timestampRaw)
		event.Detail.Thinking = resolveThinking(reasoningEffort, event.Detail.Tokens.ReasoningTokens)
		aggregateEvent(&result, event)
	}
	if err = rows.Err(); err != nil {
		return result, err
	}
	return result, nil
}

// cloneSnapshot prevents callers and JSON encoding from racing with an
// incremental update to the cached aggregate.
func cloneSnapshot(source StatisticsSnapshot) StatisticsSnapshot {
	clone := emptySnapshot()
	clone.TotalRequests = source.TotalRequests
	clone.SuccessCount = source.SuccessCount
	clone.FailureCount = source.FailureCount
	clone.TotalTokens = source.TotalTokens
	for key, value := range source.RequestsByDay {
		clone.RequestsByDay[key] = value
	}
	for key, value := range source.RequestsByHour {
		clone.RequestsByHour[key] = value
	}
	for key, value := range source.TokensByDay {
		clone.TokensByDay[key] = value
	}
	for key, value := range source.TokensByHour {
		clone.TokensByHour[key] = value
	}
	for apiName, apiSource := range source.APIs {
		apiClone := APISnapshot{
			TotalRequests: apiSource.TotalRequests,
			TotalTokens:   apiSource.TotalTokens,
			Models:        make(map[string]ModelSnapshot, len(apiSource.Models)),
		}
		for modelName, modelSource := range apiSource.Models {
			details := make([]RequestDetail, len(modelSource.Details))
			copy(details, modelSource.Details)
			for index := range details {
				if modelSource.Details[index].Thinking != nil {
					thinking := *modelSource.Details[index].Thinking
					details[index].Thinking = &thinking
				}
			}
			apiClone.Models[modelName] = ModelSnapshot{
				TotalRequests: modelSource.TotalRequests,
				TotalTokens:   modelSource.TotalTokens,
				Details:       details,
			}
		}
		clone.APIs[apiName] = apiClone
	}
	return clone
}

func (s *Store) MergeSnapshot(ctx context.Context, snapshot StatisticsSnapshot) (MergeResult, error) {
	result := MergeResult{}
	if s == nil || s.db == nil {
		return result, errors.New("usage database is not initialized")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return result, err
	}
	defer func() { _ = tx.Rollback() }()
	for apiName, apiSnapshot := range snapshot.APIs {
		apiName = strings.TrimSpace(apiName)
		if apiName == "" {
			continue
		}
		for modelName, modelSnapshot := range apiSnapshot.Models {
			modelName = strings.TrimSpace(modelName)
			if modelName == "" {
				modelName = "unknown"
			}
			for _, detail := range modelSnapshot.Details {
				detail.Tokens = normaliseTokenStats(detail.Tokens)
				detail.Thinking = resolveThinking(thinkingIntensity(detail.Thinking), detail.Tokens.ReasoningTokens)
				if detail.LatencyMs < 0 {
					detail.LatencyMs = 0
				}
				if detail.Timestamp.IsZero() {
					detail.Timestamp = time.Now()
				}
				event := usageEvent{APIKey: apiName, Model: modelName, Detail: detail}
				event.EventKey = dedupKey(apiName, modelName, detail)
				added, errInsert := s.insertEvent(ctx, tx, event)
				if errInsert != nil {
					return result, errInsert
				}
				if added {
					result.Added++
				} else {
					result.Skipped++
				}
			}
		}
	}
	if err = tx.Commit(); err != nil {
		return result, err
	}
	// Imports can add many records at once, so invalidate instead of trying to
	// replay the whole transaction into the in-memory aggregate.
	if result.Added > 0 {
		s.snapshotMu.Lock()
		s.snapshotReady = false
		s.revisionReady = false
		s.snapshotMu.Unlock()
	}
	return result, nil
}

type contextExecer interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}

func (s *Store) insertEvent(ctx context.Context, execer contextExecer, event usageEvent) (bool, error) {
	query := s.bind(`INSERT INTO usage_events
(event_key,api_key,model,timestamp,latency_ms,source,auth_index,reasoning_effort,input_tokens,output_tokens,reasoning_tokens,cached_tokens,cache_read_tokens,cache_creation_tokens,total_tokens,failed)
VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(event_key) DO NOTHING`)
	timestamp := any(event.Detail.Timestamp.UTC())
	if s.dialect == "sqlite" {
		timestamp = event.Detail.Timestamp.UTC().Format(time.RFC3339Nano)
	}
	result, err := execer.ExecContext(
		ctx,
		query,
		event.EventKey,
		event.APIKey,
		event.Model,
		timestamp,
		event.Detail.LatencyMs,
		event.Detail.Source,
		event.Detail.AuthIndex,
		thinkingIntensity(event.Detail.Thinking),
		event.Detail.Tokens.InputTokens,
		event.Detail.Tokens.OutputTokens,
		event.Detail.Tokens.ReasoningTokens,
		event.Detail.Tokens.CachedTokens,
		event.Detail.Tokens.CacheReadTokens,
		event.Detail.Tokens.CacheCreationTokens,
		event.Detail.Tokens.TotalTokens,
		event.Detail.Failed,
	)
	if err != nil {
		return false, err
	}
	rows, err := result.RowsAffected()
	return rows > 0, err
}

func emptySnapshot() StatisticsSnapshot {
	return StatisticsSnapshot{
		APIs:           make(map[string]APISnapshot),
		RequestsByDay:  make(map[string]int64),
		RequestsByHour: make(map[string]int64),
		TokensByDay:    make(map[string]int64),
		TokensByHour:   make(map[string]int64),
	}
}

func aggregateEvent(snapshot *StatisticsSnapshot, event usageEvent) {
	if snapshot == nil {
		return
	}
	detail := event.Detail
	detail.Tokens = normaliseTokenStats(detail.Tokens)
	totalTokens := detail.Tokens.TotalTokens
	snapshot.TotalRequests++
	snapshot.TotalTokens += totalTokens
	if detail.Failed {
		snapshot.FailureCount++
	} else {
		snapshot.SuccessCount++
	}
	apiSnapshot := snapshot.APIs[event.APIKey]
	if apiSnapshot.Models == nil {
		apiSnapshot.Models = make(map[string]ModelSnapshot)
	}
	apiSnapshot.TotalRequests++
	apiSnapshot.TotalTokens += totalTokens
	modelSnapshot := apiSnapshot.Models[event.Model]
	modelSnapshot.TotalRequests++
	modelSnapshot.TotalTokens += totalTokens
	modelSnapshot.Details = append(modelSnapshot.Details, detail)
	apiSnapshot.Models[event.Model] = modelSnapshot
	snapshot.APIs[event.APIKey] = apiSnapshot
	dayKey := detail.Timestamp.Format("2006-01-02")
	hourKey := formatHour(detail.Timestamp.Hour())
	snapshot.RequestsByDay[dayKey]++
	snapshot.RequestsByHour[hourKey]++
	snapshot.TokensByDay[dayKey] += totalTokens
	snapshot.TokensByHour[hourKey] += totalTokens
}

func normaliseDetail(detail coreusage.Detail) TokenStats {
	return normaliseTokenStats(TokenStats{
		InputTokens:         detail.InputTokens,
		OutputTokens:        detail.OutputTokens,
		ReasoningTokens:     detail.ReasoningTokens,
		CachedTokens:        detail.CachedTokens,
		CacheReadTokens:     detail.CacheReadTokens,
		CacheCreationTokens: detail.CacheCreationTokens,
		TotalTokens:         detail.TotalTokens,
	})
}

func normaliseTokenStats(tokens TokenStats) TokenStats {
	if tokens.CacheReadTokens == 0 && tokens.CacheCreationTokens == 0 && tokens.CachedTokens > 0 {
		tokens.CacheReadTokens = tokens.CachedTokens
	}
	if tokens.CachedTokens == 0 {
		if tokens.CacheReadTokens > 0 {
			tokens.CachedTokens = tokens.CacheReadTokens
		} else if tokens.CacheCreationTokens > 0 {
			tokens.CachedTokens = tokens.CacheCreationTokens
		}
	}
	if tokens.TotalTokens == 0 {
		tokens.TotalTokens = tokens.InputTokens + tokens.OutputTokens + tokens.ReasoningTokens
	}
	if tokens.TotalTokens == 0 {
		tokens.TotalTokens = tokens.InputTokens + tokens.OutputTokens + tokens.ReasoningTokens + tokens.CachedTokens
	}
	return tokens
}

func normaliseThinking(thinking *ThinkingStats) *ThinkingStats {
	if thinking == nil {
		return nil
	}
	intensity := strings.TrimSpace(thinking.Intensity)
	if intensity == "" {
		return nil
	}
	return &ThinkingStats{Intensity: intensity}
}

func thinkingIntensity(thinking *ThinkingStats) string {
	normalised := normaliseThinking(thinking)
	if normalised == nil {
		return ""
	}
	return normalised.Intensity
}

func resolveThinking(reasoningEffort string, reasoningTokens int64) *ThinkingStats {
	thinking := normaliseThinking(&ThinkingStats{Intensity: reasoningEffort})
	if thinking != nil {
		return thinking
	}
	if reasoningTokens > 0 {
		return &ThinkingStats{Intensity: "auto"}
	}
	return nil
}

func normaliseLatency(latency time.Duration) int64 {
	if latency <= 0 {
		return 0
	}
	return latency.Milliseconds()
}

func resolveAPIIdentifier(ctx context.Context, record coreusage.Record) string {
	if ctx != nil {
		if endpoint := strings.TrimSpace(internallogging.GetEndpoint(ctx)); endpoint != "" {
			return endpoint
		}
	}
	if record.Provider != "" {
		return record.Provider
	}
	return "unknown"
}

func resolveSuccess(ctx context.Context) bool {
	status := internallogging.GetResponseStatus(ctx)
	return status == 0 || status < 400
}

func dedupKey(apiName, modelName string, detail RequestDetail) string {
	tokens := normaliseTokenStats(detail.Tokens)
	raw := fmt.Sprintf(
		"%s|%s|%s|%s|%s|%t|%d|%d|%d|%d|%d",
		apiName,
		modelName,
		detail.Timestamp.UTC().Format(time.RFC3339Nano),
		detail.Source,
		detail.AuthIndex,
		detail.Failed,
		tokens.InputTokens,
		tokens.OutputTokens,
		tokens.ReasoningTokens,
		tokens.CachedTokens,
		tokens.TotalTokens,
	)
	if intensity := thinkingIntensity(detail.Thinking); intensity != "" {
		raw += "|thinking:" + intensity
	}
	if tokens.CacheCreationTokens > 0 {
		raw += fmt.Sprintf("|cache-creation:%d", tokens.CacheCreationTokens)
	}
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

func formatHour(hour int) string {
	if hour < 0 {
		hour = 0
	}
	return fmt.Sprintf("%02d", hour%24)
}

func parseCreatedAt(value any) time.Time {
	switch item := value.(type) {
	case time.Time:
		return item.UTC()
	case []byte:
		return parseCreatedAtString(string(item))
	case string:
		return parseCreatedAtString(item)
	default:
		return time.Time{}
	}
}

func parseCreatedAtString(value string) time.Time {
	for _, layout := range []string{time.RFC3339Nano, "2006-01-02 15:04:05.999999999-07:00", "2006-01-02 15:04:05.999999999"} {
		if parsed, err := time.Parse(layout, strings.TrimSpace(value)); err == nil {
			return parsed.UTC()
		}
	}
	return time.Time{}
}

func (s *Store) bind(query string) string {
	if s.dialect != "postgres" {
		return query
	}
	var builder strings.Builder
	index := 1
	for _, character := range query {
		if character == '?' {
			builder.WriteString("$")
			builder.WriteString(strconv.Itoa(index))
			index++
		} else {
			builder.WriteRune(character)
		}
	}
	return builder.String()
}

func (s *Store) ensureReasoningEffortColumn(ctx context.Context) error {
	if s.dialect == "postgres" {
		_, err := s.db.ExecContext(ctx, `ALTER TABLE usage_events ADD COLUMN IF NOT EXISTS reasoning_effort TEXT NOT NULL DEFAULT ''`)
		return err
	}
	var count int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM pragma_table_info('usage_events') WHERE name = 'reasoning_effort'`).Scan(&count); err != nil {
		return err
	}
	if count > 0 {
		return nil
	}
	_, err := s.db.ExecContext(ctx, `ALTER TABLE usage_events ADD COLUMN reasoning_effort TEXT NOT NULL DEFAULT ''`)
	return err
}

func (s *Store) ensureCacheTokenColumns(ctx context.Context) error {
	columns := []struct {
		name string
		ddl  string
	}{
		{name: "cache_read_tokens", ddl: "BIGINT NOT NULL DEFAULT 0"},
		{name: "cache_creation_tokens", ddl: "BIGINT NOT NULL DEFAULT 0"},
	}
	for _, column := range columns {
		if s.dialect == "postgres" {
			if _, err := s.db.ExecContext(ctx, fmt.Sprintf("ALTER TABLE usage_events ADD COLUMN IF NOT EXISTS %s %s", column.name, column.ddl)); err != nil {
				return err
			}
			continue
		}
		var count int
		query := fmt.Sprintf("SELECT COUNT(*) FROM pragma_table_info('usage_events') WHERE name = '%s'", column.name)
		if err := s.db.QueryRowContext(ctx, query).Scan(&count); err != nil {
			return err
		}
		if count > 0 {
			continue
		}
		if _, err := s.db.ExecContext(ctx, fmt.Sprintf("ALTER TABLE usage_events ADD COLUMN %s %s", column.name, column.ddl)); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) migrateLegacyRecords(ctx context.Context) error {
	exists, err := s.legacyTableExists(ctx)
	if err != nil || !exists {
		return err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id,provider,model,status_code,input_tokens,output_tokens,total_tokens,duration_ms,account_name,auth_index,created_at FROM usage_records ORDER BY id ASC`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id, statusCode, inputTokens, outputTokens, totalTokens, durationMS int64
		var apiKey, model, source, authIndex string
		var timestampRaw any
		if err = rows.Scan(&id, &apiKey, &model, &statusCode, &inputTokens, &outputTokens, &totalTokens, &durationMS, &source, &authIndex, &timestampRaw); err != nil {
			return err
		}
		event := usageEvent{
			APIKey: apiKey,
			Model:  model,
			Detail: RequestDetail{
				Timestamp: parseCreatedAt(timestampRaw),
				LatencyMs: durationMS,
				Source:    source,
				AuthIndex: authIndex,
				Tokens: TokenStats{
					InputTokens:  inputTokens,
					OutputTokens: outputTokens,
					TotalTokens:  totalTokens,
				},
				Failed: statusCode < 200 || statusCode >= 300,
			},
		}
		legacyEventKey := fmt.Sprintf("legacy-sql-v1:%d", id)
		event.EventKey = dedupKey(event.APIKey, event.Model, event.Detail)
		// Older builds keyed migrated rows by their source table ID. Normalize
		// those rows so exporting and importing the same snapshot remains idempotent.
		deleteDuplicate := s.bind(`DELETE FROM usage_events WHERE event_key = ? AND EXISTS (SELECT 1 FROM usage_events WHERE event_key = ?)`)
		if _, err = s.db.ExecContext(ctx, deleteDuplicate, legacyEventKey, event.EventKey); err != nil {
			return err
		}
		updateLegacy := s.bind(`UPDATE usage_events SET event_key = ? WHERE event_key = ?`)
		if _, err = s.db.ExecContext(ctx, updateLegacy, event.EventKey, legacyEventKey); err != nil {
			return err
		}
		if _, err = s.insertEvent(ctx, s.db, event); err != nil {
			return err
		}
	}
	return rows.Err()
}

func (s *Store) legacyTableExists(ctx context.Context) (bool, error) {
	if s.dialect == "postgres" {
		var name sql.NullString
		if err := s.db.QueryRowContext(ctx, "SELECT to_regclass('usage_records')").Scan(&name); err != nil {
			return false, err
		}
		return name.Valid && name.String != "", nil
	}
	var name string
	err := s.db.QueryRowContext(ctx, "SELECT name FROM sqlite_master WHERE type='table' AND name='usage_records'").Scan(&name)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil && name != "", err
}
