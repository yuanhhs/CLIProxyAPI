package management

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/redisqueue"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/usage"
)

type usageQueueRecord []byte

func (r usageQueueRecord) MarshalJSON() ([]byte, error) {
	if json.Valid(r) {
		return append([]byte(nil), r...), nil
	}
	return json.Marshal(string(r))
}

// GetUsageQueue pops queued usage records from the usage queue.
func (h *Handler) GetUsageQueue(c *gin.Context) {
	if h == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "handler unavailable"})
		return
	}
	count, err := parseUsageQueueCount(c.Query("count"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	items := redisqueue.PopOldest(count)
	records := make([]usageQueueRecord, 0, len(items))
	for _, item := range items {
		records = append(records, usageQueueRecord(append([]byte(nil), item...)))
	}
	c.JSON(http.StatusOK, records)
}

func parseUsageQueueCount(value string) (int, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 1, nil
	}
	count, err := strconv.Atoi(value)
	if err != nil || count <= 0 {
		return 0, errors.New("count must be a positive integer")
	}
	return count, nil
}

type usageExportPayload struct {
	Version    int                      `json:"version"`
	ExportedAt time.Time                `json:"exported_at"`
	Usage      usage.StatisticsSnapshot `json:"usage"`
}

type usageImportPayload struct {
	Version int                      `json:"version"`
	Usage   usage.StatisticsSnapshot `json:"usage"`
}

// GetUsage returns the official usage response shape backed by SQL persistence.
func (h *Handler) GetUsage(c *gin.Context) {
	store := h.getUsageStore()
	if store == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "usage database unavailable"})
		return
	}
	revision, err := store.Revision(c.Request.Context())
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	snapshot, err := store.Snapshot(c.Request.Context())
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"usage":           snapshot,
		"failed_requests": snapshot.FailureCount,
		"revision":        revision,
	})
}

// GetUsageRevision returns a cheap change marker for live usage dashboards.
func (h *Handler) GetUsageRevision(c *gin.Context) {
	store := h.getUsageStore()
	if store == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "usage database unavailable"})
		return
	}
	revision, err := store.Revision(c.Request.Context())
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, revision)
}

func (h *Handler) ExportUsage(c *gin.Context) {
	store := h.getUsageStore()
	if store == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "usage database unavailable"})
		return
	}
	snapshot, err := store.Snapshot(c.Request.Context())
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, usageExportPayload{Version: 1, ExportedAt: time.Now().UTC(), Usage: snapshot})
}

func (h *Handler) ImportUsage(c *gin.Context) {
	store := h.getUsageStore()
	if store == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "usage database unavailable"})
		return
	}
	data, err := c.GetRawData()
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "failed to read request body"})
		return
	}
	var payload usageImportPayload
	if err = json.Unmarshal(data, &payload); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid json"})
		return
	}
	if payload.Version != 0 && payload.Version != 1 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "unsupported version"})
		return
	}
	result, err := store.MergeSnapshot(c.Request.Context(), payload.Usage)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	snapshot, err := store.Snapshot(c.Request.Context())
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"added":           result.Added,
		"skipped":         result.Skipped,
		"total_requests":  snapshot.TotalRequests,
		"failed_requests": snapshot.FailureCount,
	})
}

func (h *Handler) getUsageStore() *usage.Store {
	if h == nil {
		return nil
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.usageStore
}
