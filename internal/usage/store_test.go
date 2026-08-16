package usage

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	coreusage "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
)

func TestSQLiteOfficialUsageSnapshotPersistsAllTokenTypes(t *testing.T) {
	t.Setenv("USAGE_DB_DRIVER", "sqlite")
	t.Setenv("USAGE_DB_DSN", t.TempDir()+"/sql.db")
	store, err := Open(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	err = store.Record(context.Background(), coreusage.Record{
		Provider:        "codex",
		Model:           "gpt-5",
		APIKey:          "client-key",
		Source:          "user@example.com",
		AuthIndex:       "auth-1",
		ReasoningEffort: "high",
		RequestedAt:     time.Now().UTC(),
		Latency:         1250 * time.Millisecond,
		Detail: coreusage.Detail{
			InputTokens:         40,
			OutputTokens:        20,
			ReasoningTokens:     10,
			CachedTokens:        30,
			CacheReadTokens:     30,
			CacheCreationTokens: 12,
			TotalTokens:         70,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := store.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.TotalRequests != 1 || snapshot.TotalTokens != 70 || snapshot.SuccessCount != 1 {
		t.Fatalf("unexpected summary: %+v", snapshot)
	}
	details := snapshot.APIs["client-key"].Models["gpt-5"].Details
	if len(details) != 1 {
		t.Fatalf("details = %d, want 1", len(details))
	}
	if details[0].Source != "user@example.com" || details[0].AuthIndex != "auth-1" {
		t.Fatalf("unexpected account detail: %+v", details[0])
	}
	if details[0].Tokens.CachedTokens != 30 || details[0].Tokens.ReasoningTokens != 10 {
		t.Fatalf("cached/reasoning tokens were not persisted: %+v", details[0].Tokens)
	}
	if details[0].Tokens.CacheReadTokens != 30 || details[0].Tokens.CacheCreationTokens != 12 {
		t.Fatalf("cache read/creation tokens were not persisted: %+v", details[0].Tokens)
	}
	if details[0].Thinking == nil || details[0].Thinking.Intensity != "high" {
		t.Fatalf("reasoning effort was not persisted: %+v", details[0].Thinking)
	}

	if err = store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = Open(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	snapshot, err = store.Snapshot(context.Background())
	if err != nil || snapshot.TotalRequests != 1 {
		t.Fatalf("snapshot after reopen = %+v, err=%v", snapshot, err)
	}
	details = snapshot.APIs["client-key"].Models["gpt-5"].Details
	if len(details) != 1 || details[0].Thinking == nil || details[0].Thinking.Intensity != "high" {
		t.Fatalf("reasoning effort after reopen = %+v", details)
	}
}

func TestSQLiteUsageSnapshotCacheTracksNewRecords(t *testing.T) {
	t.Setenv("USAGE_DB_DRIVER", "sqlite")
	t.Setenv("USAGE_DB_DSN", t.TempDir()+"/sql.db")
	store, err := Open(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	record := coreusage.Record{
		Provider:    "codex",
		Model:       "gpt-5",
		APIKey:      "client-key",
		RequestedAt: time.Date(2026, 8, 16, 1, 2, 3, 0, time.UTC),
		Detail:      coreusage.Detail{InputTokens: 4, OutputTokens: 2, TotalTokens: 6},
	}
	if err = store.Record(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	first, err := store.Snapshot(context.Background())
	if err != nil || first.TotalRequests != 1 {
		t.Fatalf("first cached snapshot = %+v, err=%v", first, err)
	}

	record.RequestedAt = record.RequestedAt.Add(time.Minute)
	record.Detail.TotalTokens = 9
	if err = store.Record(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	second, err := store.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if second.TotalRequests != 2 || second.TotalTokens != 15 {
		t.Fatalf("updated cached snapshot = %+v", second)
	}
}

func TestSQLiteSnapshotSinceLimitsHistory(t *testing.T) {
	t.Setenv("USAGE_DB_DRIVER", "sqlite")
	t.Setenv("USAGE_DB_DSN", t.TempDir()+"/sql.db")
	store, err := Open(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	for index, requestedAt := range []time.Time{
		time.Date(2026, 8, 1, 1, 0, 0, 0, time.UTC),
		time.Date(2026, 8, 16, 1, 0, 0, 0, time.UTC),
	} {
		if err = store.Record(context.Background(), coreusage.Record{
			Provider:    "codex",
			Model:       "gpt-5",
			APIKey:      "client-key",
			RequestedAt: requestedAt,
			Detail:      coreusage.Detail{InputTokens: int64(index + 1), TotalTokens: int64(index + 1)},
		}); err != nil {
			t.Fatal(err)
		}
	}

	snapshot, err := store.SnapshotSince(
		context.Background(),
		time.Date(2026, 8, 15, 0, 0, 0, 0, time.UTC),
	)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.TotalRequests != 1 || snapshot.TotalTokens != 2 {
		t.Fatalf("range-limited snapshot = %+v", snapshot)
	}
}

func TestMergeSnapshotSkipsDuplicateDetails(t *testing.T) {
	t.Setenv("USAGE_DB_DRIVER", "sqlite")
	t.Setenv("USAGE_DB_DSN", t.TempDir()+"/sql.db")
	store, err := Open(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	detail := RequestDetail{
		Timestamp: time.Date(2026, 8, 15, 1, 2, 3, 0, time.UTC),
		Source:    "source",
		AuthIndex: "index",
		Tokens: TokenStats{
			InputTokens:         5,
			CachedTokens:        3,
			CacheReadTokens:     3,
			CacheCreationTokens: 2,
			TotalTokens:         5,
		},
		Thinking: &ThinkingStats{Intensity: "xhigh"},
	}
	snapshot := StatisticsSnapshot{APIs: map[string]APISnapshot{
		"key": {Models: map[string]ModelSnapshot{"model": {Details: []RequestDetail{detail}}}},
	}}
	first, err := store.MergeSnapshot(context.Background(), snapshot)
	if err != nil || first.Added != 1 || first.Skipped != 0 {
		t.Fatalf("first merge = %+v, err=%v", first, err)
	}
	second, err := store.MergeSnapshot(context.Background(), snapshot)
	if err != nil || second.Added != 0 || second.Skipped != 1 {
		t.Fatalf("second merge = %+v, err=%v", second, err)
	}
	persisted, err := store.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	persistedDetails := persisted.APIs["key"].Models["model"].Details
	if len(persistedDetails) != 1 || persistedDetails[0].Thinking == nil || persistedDetails[0].Thinking.Intensity != "xhigh" {
		t.Fatalf("merged reasoning effort = %+v", persistedDetails)
	}
}

func TestSQLiteExistingUsageSchemaAddsReasoningEffort(t *testing.T) {
	databasePath := filepath.Join(t.TempDir(), "sql.db")
	db, err := sql.Open("sqlite", databasePath)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`CREATE TABLE usage_events (
id INTEGER PRIMARY KEY AUTOINCREMENT,
event_key TEXT NOT NULL UNIQUE,
api_key TEXT NOT NULL,
model TEXT NOT NULL,
timestamp TEXT NOT NULL,
latency_ms BIGINT NOT NULL DEFAULT 0,
source TEXT NOT NULL DEFAULT '',
auth_index TEXT NOT NULL DEFAULT '',
input_tokens BIGINT NOT NULL DEFAULT 0,
output_tokens BIGINT NOT NULL DEFAULT 0,
reasoning_tokens BIGINT NOT NULL DEFAULT 0,
cached_tokens BIGINT NOT NULL DEFAULT 0,
total_tokens BIGINT NOT NULL DEFAULT 0,
failed BOOLEAN NOT NULL DEFAULT FALSE
)`)
	if err != nil {
		t.Fatal(err)
	}
	if err = db.Close(); err != nil {
		t.Fatal(err)
	}

	t.Setenv("USAGE_DB_DRIVER", "sqlite")
	t.Setenv("USAGE_DB_DSN", databasePath)
	store, err := Open(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err = store.Record(context.Background(), coreusage.Record{
		Model:           "gpt-5.4",
		APIKey:          "client-key",
		ReasoningEffort: "xhigh",
		RequestedAt:     time.Now().UTC(),
		Detail:          coreusage.Detail{InputTokens: 1, TotalTokens: 1},
	}); err != nil {
		t.Fatal(err)
	}
	snapshot, err := store.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	details := snapshot.APIs["client-key"].Models["gpt-5.4"].Details
	if len(details) != 1 || details[0].Thinking == nil || details[0].Thinking.Intensity != "xhigh" {
		t.Fatalf("migrated reasoning effort = %+v", details)
	}
}

func TestSQLiteExistingUsageSchemaAddsCacheBreakdownWithLegacyFallback(t *testing.T) {
	databasePath := filepath.Join(t.TempDir(), "sql.db")
	db, err := sql.Open("sqlite", databasePath)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`CREATE TABLE usage_events (
id INTEGER PRIMARY KEY AUTOINCREMENT,
event_key TEXT NOT NULL UNIQUE,
api_key TEXT NOT NULL,
model TEXT NOT NULL,
timestamp TEXT NOT NULL,
latency_ms BIGINT NOT NULL DEFAULT 0,
source TEXT NOT NULL DEFAULT '',
auth_index TEXT NOT NULL DEFAULT '',
reasoning_effort TEXT NOT NULL DEFAULT '',
input_tokens BIGINT NOT NULL DEFAULT 0,
output_tokens BIGINT NOT NULL DEFAULT 0,
reasoning_tokens BIGINT NOT NULL DEFAULT 0,
cached_tokens BIGINT NOT NULL DEFAULT 0,
total_tokens BIGINT NOT NULL DEFAULT 0,
failed BOOLEAN NOT NULL DEFAULT FALSE
)`)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`INSERT INTO usage_events
(event_key,api_key,model,timestamp,input_tokens,cached_tokens,total_tokens)
VALUES ('legacy-cache','client-key','gpt-5','2026-08-15T01:02:03Z',40,30,40)`)
	if err != nil {
		t.Fatal(err)
	}
	if err = db.Close(); err != nil {
		t.Fatal(err)
	}

	t.Setenv("USAGE_DB_DRIVER", "sqlite")
	t.Setenv("USAGE_DB_DSN", databasePath)
	store, err := Open(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	snapshot, err := store.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	details := snapshot.APIs["client-key"].Models["gpt-5"].Details
	if len(details) != 1 || details[0].Tokens.CacheReadTokens != 30 || details[0].Tokens.CacheCreationTokens != 0 {
		t.Fatalf("legacy cache fallback = %+v", details)
	}
}

func TestSQLiteUsageSnapshotInfersAutomaticThinkingFromReasoningTokens(t *testing.T) {
	t.Setenv("USAGE_DB_DRIVER", "sqlite")
	t.Setenv("USAGE_DB_DSN", t.TempDir()+"/sql.db")
	store, err := Open(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	_, err = store.db.Exec(`INSERT INTO usage_events
(event_key,api_key,model,timestamp,reasoning_effort,reasoning_tokens,total_tokens)
VALUES ('historical-auto','client-key','deepseek-v4-flash','2026-08-15T07:40:09Z','',107,107)`)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := store.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	details := snapshot.APIs["client-key"].Models["deepseek-v4-flash"].Details
	if len(details) != 1 || details[0].Thinking == nil || details[0].Thinking.Intensity != "auto" {
		t.Fatalf("inferred thinking intensity = %+v", details)
	}
}

func TestLegacyMigrationUsesCanonicalDedupKey(t *testing.T) {
	databasePath := filepath.Join(t.TempDir(), "sql.db")
	db, err := sql.Open("sqlite", databasePath)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`CREATE TABLE usage_records (
id INTEGER PRIMARY KEY,
provider TEXT NOT NULL,
model TEXT NOT NULL,
status_code INTEGER NOT NULL,
input_tokens INTEGER NOT NULL,
output_tokens INTEGER NOT NULL,
total_tokens INTEGER NOT NULL,
duration_ms INTEGER NOT NULL,
account_name TEXT NOT NULL,
auth_index TEXT NOT NULL,
created_at TEXT NOT NULL
)`)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`INSERT INTO usage_records
(id,provider,model,status_code,input_tokens,output_tokens,total_tokens,duration_ms,account_name,auth_index,created_at)
VALUES (1,'client-key','gpt-5',200,40,20,60,1250,'user@example.com','auth-1','2026-08-15T01:02:03Z')`)
	if err != nil {
		t.Fatal(err)
	}
	if err = db.Close(); err != nil {
		t.Fatal(err)
	}

	t.Setenv("USAGE_DB_DRIVER", "sqlite")
	t.Setenv("USAGE_DB_DSN", databasePath)
	store, err := Open(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	snapshot, err := store.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	result, err := store.MergeSnapshot(context.Background(), snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if result.Added != 0 || result.Skipped != 1 {
		t.Fatalf("merge after migration = %+v, want one skipped event", result)
	}
}
