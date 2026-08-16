package management

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/redisqueue"
	internalusage "github.com/router-for-me/CLIProxyAPI/v7/internal/usage"
)

func TestGetUsageQueuePopsRequestedRecords(t *testing.T) {
	gin.SetMode(gin.TestMode)
	withManagementUsageQueue(t, func() {
		redisqueue.Enqueue([]byte(`{"id":1}`))
		redisqueue.Enqueue([]byte(`{"id":2}`))
		redisqueue.Enqueue([]byte(`{"id":3}`))

		rec := httptest.NewRecorder()
		ginCtx, _ := gin.CreateTestContext(rec)
		ginCtx.Request = httptest.NewRequest(http.MethodGet, "/v0/management/usage-queue?count=2", nil)

		h := &Handler{}
		h.GetUsageQueue(ginCtx)

		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d body=%s", rec.Code, http.StatusOK, rec.Body.String())
		}

		var payload []json.RawMessage
		if errUnmarshal := json.Unmarshal(rec.Body.Bytes(), &payload); errUnmarshal != nil {
			t.Fatalf("unmarshal response: %v", errUnmarshal)
		}
		if len(payload) != 2 {
			t.Fatalf("response records = %d, want 2", len(payload))
		}
		requireRecordID(t, payload[0], 1)
		requireRecordID(t, payload[1], 2)

		remaining := redisqueue.PopOldest(10)
		if len(remaining) != 1 || string(remaining[0]) != `{"id":3}` {
			t.Fatalf("remaining queue = %q, want third item only", remaining)
		}
	})
}

func TestGetUsageQueueInvalidCountDoesNotPop(t *testing.T) {
	gin.SetMode(gin.TestMode)
	withManagementUsageQueue(t, func() {
		redisqueue.Enqueue([]byte(`{"id":1}`))

		rec := httptest.NewRecorder()
		ginCtx, _ := gin.CreateTestContext(rec)
		ginCtx.Request = httptest.NewRequest(http.MethodGet, "/v0/management/usage-queue?count=0", nil)

		h := &Handler{}
		h.GetUsageQueue(ginCtx)

		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want %d body=%s", rec.Code, http.StatusBadRequest, rec.Body.String())
		}

		remaining := redisqueue.PopOldest(10)
		if len(remaining) != 1 || string(remaining[0]) != `{"id":1}` {
			t.Fatalf("remaining queue = %q, want original item", remaining)
		}
	})
}

func TestGetUsageRevision(t *testing.T) {
	gin.SetMode(gin.TestMode)
	t.Setenv("USAGE_DB_DRIVER", "sqlite")
	t.Setenv("USAGE_DB_DSN", t.TempDir()+"/sql.db")
	store, err := internalusage.Open(t.Context(), "")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	rec := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(rec)
	ginCtx.Request = httptest.NewRequest(http.MethodGet, "/v0/management/usage/revision", nil)

	h := &Handler{usageStore: store}
	h.GetUsageRevision(ginCtx)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	var revision internalusage.Revision
	if errUnmarshal := json.Unmarshal(rec.Body.Bytes(), &revision); errUnmarshal != nil {
		t.Fatalf("unmarshal response: %v", errUnmarshal)
	}
	if revision.LatestID != 0 || revision.TotalRows != 0 {
		t.Fatalf("revision = %+v, want empty", revision)
	}
}

func TestParseUsageRange(t *testing.T) {
	now := time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		value      string
		wantSince  time.Time
		wantRanged bool
		wantError  bool
	}{
		{value: "", wantRanged: false},
		{value: "all", wantRanged: false},
		{value: "24h", wantSince: now.Add(-24 * time.Hour), wantRanged: true},
		{value: "7d", wantSince: now.Add(-7 * 24 * time.Hour), wantRanged: true},
		{value: "30d", wantSince: now.Add(-30 * 24 * time.Hour), wantRanged: true},
		{value: "invalid", wantError: true},
	}

	for _, test := range tests {
		t.Run(test.value, func(t *testing.T) {
			since, ranged, err := parseUsageRange(test.value, now)
			if (err != nil) != test.wantError {
				t.Fatalf("error = %v, wantError=%v", err, test.wantError)
			}
			if ranged != test.wantRanged || !since.Equal(test.wantSince) {
				t.Fatalf("since=%v ranged=%v, want since=%v ranged=%v", since, ranged, test.wantSince, test.wantRanged)
			}
		})
	}
}

func withManagementUsageQueue(t *testing.T, fn func()) {
	t.Helper()

	prevQueueEnabled := redisqueue.Enabled()
	redisqueue.SetEnabled(false)
	redisqueue.SetEnabled(true)

	defer func() {
		redisqueue.SetEnabled(false)
		redisqueue.SetEnabled(prevQueueEnabled)
	}()

	fn()
}

func requireRecordID(t *testing.T, raw json.RawMessage, want int) {
	t.Helper()

	var payload struct {
		ID int `json:"id"`
	}
	if errUnmarshal := json.Unmarshal(raw, &payload); errUnmarshal != nil {
		t.Fatalf("unmarshal record: %v", errUnmarshal)
	}
	if payload.ID != want {
		t.Fatalf("record id = %d, want %d", payload.ID, want)
	}
}
