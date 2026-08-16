package management

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

const (
	openCodeAPIBaseURL        = "https://opencode.ai/zen/go/v1"
	openCodeModelsMaxBody     = 2 << 20
	openCodeModelsHTTPTimeout = 15 * time.Second
)

type openCodeModel struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	OwnedBy string `json:"owned_by"`
}

type openCodeModelList struct {
	Data []openCodeModel `json:"data"`
}

func (h *Handler) getOpenCodeAuthFileModels(c *gin.Context, auth *coreauth.Auth) {
	apiKey := ""
	if auth != nil && auth.Metadata != nil {
		apiKey, _ = auth.Metadata["api_key"].(string)
	}

	client := &http.Client{
		Timeout:   openCodeModelsHTTPTimeout,
		Transport: h.apiCallTransport(auth),
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	models, errFetch := fetchOpenCodeModels(
		c.Request.Context(),
		client,
		openCodeAPIBaseURL,
		strings.TrimSpace(apiKey),
	)
	if errFetch != nil {
		var upstreamErr *openCodeUpstreamError
		if errors.As(errFetch, &upstreamErr) && upstreamErr != nil {
			status := http.StatusBadGateway
			if upstreamErr.StatusCode == http.StatusUnauthorized || upstreamErr.StatusCode == http.StatusForbidden {
				status = http.StatusForbidden
			}
			c.JSON(status, gin.H{"error": upstreamErr.Message})
			return
		}
		c.JSON(http.StatusBadGateway, gin.H{"error": errFetch.Error()})
		return
	}

	result := make([]gin.H, 0, len(models))
	registeredModels := make([]*registry.ModelInfo, 0, len(models))
	for _, model := range models {
		entry := gin.H{
			"id":   model.ID,
			"type": "opencode",
		}
		if model.OwnedBy != "" {
			entry["owned_by"] = model.OwnedBy
		}
		result = append(result, entry)
		registeredModels = append(registeredModels, &registry.ModelInfo{
			ID:      model.ID,
			Object:  "model",
			OwnedBy: model.OwnedBy,
			Type:    "opencode",
		})
	}
	if auth != nil && auth.ID != "" && !auth.Disabled && strings.TrimSpace(apiKey) != "" && len(registeredModels) > 0 {
		registry.GetGlobalRegistry().RegisterClient(auth.ID, "opencode", registeredModels)
	}
	c.JSON(http.StatusOK, gin.H{"models": result})
}

func fetchOpenCodeModels(ctx context.Context, client *http.Client, baseURL, apiKey string) ([]openCodeModel, error) {
	if client == nil {
		return nil, errors.New("http client is required")
	}
	targetURL := strings.TrimRight(strings.TrimSpace(baseURL), "/") + "/models"
	req, errRequest := http.NewRequestWithContext(ctx, http.MethodGet, targetURL, nil)
	if errRequest != nil {
		return nil, fmt.Errorf("create OpenCode models request: %w", errRequest)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "CLIProxyAPI-Management")
	if apiKey = strings.TrimSpace(apiKey); apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}

	resp, errDo := client.Do(req)
	if errDo != nil {
		return nil, fmt.Errorf("request OpenCode models: %w", errDo)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return nil, &openCodeUpstreamError{
			StatusCode: resp.StatusCode,
			Message:    fmt.Sprintf("OpenCode models returned HTTP %d", resp.StatusCode),
		}
	}

	body, errRead := io.ReadAll(io.LimitReader(resp.Body, openCodeModelsMaxBody+1))
	if errRead != nil {
		return nil, fmt.Errorf("read OpenCode models: %w", errRead)
	}
	if len(body) > openCodeModelsMaxBody {
		return nil, errors.New("OpenCode models response exceeds the size limit")
	}
	var payload openCodeModelList
	if errDecode := json.Unmarshal(body, &payload); errDecode != nil {
		return nil, fmt.Errorf("decode OpenCode models: %w", errDecode)
	}

	seen := make(map[string]struct{}, len(payload.Data))
	models := make([]openCodeModel, 0, len(payload.Data))
	for _, model := range payload.Data {
		model.ID = strings.TrimSpace(model.ID)
		model.OwnedBy = strings.TrimSpace(model.OwnedBy)
		if model.ID == "" {
			continue
		}
		if _, exists := seen[model.ID]; exists {
			continue
		}
		seen[model.ID] = struct{}{}
		models = append(models, model)
	}
	return models, nil
}
