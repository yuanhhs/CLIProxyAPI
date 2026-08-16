package management

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

const (
	openCodeBaseURL     = "https://opencode.ai"
	openCodeMaxPageSize = 8 << 20
)

var (
	openCodeRefAssignPattern = regexp.MustCompile(`\$R\[\d+\]\s*=\s*`)
	openCodeNewDatePattern   = regexp.MustCompile(`new Date\("([^"]+)"\)`)
)

type openCodeQuotaRequest struct {
	Name string `json:"name"`
}

type openCodeUsageLimit struct {
	Status       string `json:"status"`
	ResetInSec   int    `json:"resetInSec"`
	UsagePercent int    `json:"usagePercent"`
}

type openCodeQuotaData struct {
	RollingUsage openCodeUsageLimit `json:"rollingUsage"`
	WeeklyUsage  openCodeUsageLimit `json:"weeklyUsage"`
	MonthlyUsage openCodeUsageLimit `json:"monthlyUsage"`
}

type openCodeQuotaWindow struct {
	UsedPercent      int    `json:"used_percent"`
	RemainingPercent int    `json:"remaining_percent"`
	Status           string `json:"status"`
	ResetInSeconds   int    `json:"reset_in_seconds"`
}

type openCodeQuotaResponse struct {
	Rolling   openCodeQuotaWindow `json:"rolling"`
	Weekly    openCodeQuotaWindow `json:"weekly"`
	Monthly   openCodeQuotaWindow `json:"monthly"`
	FetchedAt time.Time           `json:"fetched_at"`
}

type openCodeUpstreamError struct {
	StatusCode int
	Message    string
}

func (e *openCodeUpstreamError) Error() string { return e.Message }

// GetOpenCodeQuota fetches an OpenCode workspace quota using a saved auth file.
func (h *Handler) GetOpenCodeQuota(c *gin.Context) {
	if c == nil || c.Request == nil {
		return
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 1<<20)
	var request openCodeQuotaRequest
	if errBind := c.ShouldBindJSON(&request); errBind != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}
	request.Name = strings.TrimSpace(request.Name)
	if request.Name == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "auth file name is required"})
		return
	}
	if request.Name != filepath.Base(request.Name) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid auth file name"})
		return
	}

	cookie, workspaceID, found := h.openCodeCredential(request.Name)
	if !found {
		c.JSON(http.StatusNotFound, gin.H{"error": "OpenCode auth file not found"})
		return
	}
	if cookie == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "OpenCode auth file is missing cookie"})
		return
	}
	if workspaceID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "OpenCode auth file is missing workspace_id"})
		return
	}

	client := &http.Client{CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	quota, errFetch := fetchOpenCodeQuota(c.Request.Context(), client, openCodeBaseURL, cookie, workspaceID)
	if errFetch != nil {
		var upstreamErr *openCodeUpstreamError
		if errors.As(errFetch, &upstreamErr) && upstreamErr != nil {
			status := http.StatusBadGateway
			if upstreamErr.StatusCode == http.StatusUnauthorized || upstreamErr.StatusCode == http.StatusForbidden || upstreamErr.StatusCode == http.StatusFound || upstreamErr.StatusCode == http.StatusMovedPermanently {
				status = http.StatusForbidden
			}
			c.JSON(status, gin.H{"error": upstreamErr.Message})
			return
		}
		c.JSON(http.StatusBadGateway, gin.H{"error": errFetch.Error()})
		return
	}
	c.JSON(http.StatusOK, quota)
}

func (h *Handler) openCodeCredential(name string) (cookie, workspaceID string, found bool) {
	if h == nil || h.authManager == nil {
		return "", "", false
	}
	name = strings.TrimSpace(name)
	for _, auth := range h.authManager.List() {
		if auth == nil || !strings.EqualFold(strings.TrimSpace(auth.Provider), "opencode") {
			continue
		}
		if !openCodeAuthNameMatches(auth.FileName, auth.ID, auth.Attributes, name) {
			continue
		}
		if auth.Metadata == nil {
			return "", "", true
		}
		cookie, _ = auth.Metadata["cookie"].(string)
		workspaceID, _ = auth.Metadata["workspace_id"].(string)
		return strings.TrimSpace(cookie), strings.TrimSpace(workspaceID), true
	}
	return "", "", false
}

func openCodeAuthNameMatches(fileName, id string, attributes map[string]string, name string) bool {
	for _, candidate := range []string{
		fileName,
		filepath.Base(strings.TrimSpace(id)),
		filepath.Base(strings.TrimSpace(attributes["path"])),
		filepath.Base(strings.TrimSpace(attributes["source"])),
	} {
		if candidate != "." && strings.EqualFold(strings.TrimSpace(candidate), name) {
			return true
		}
	}
	return false
}

func fetchOpenCodeQuota(ctx context.Context, client *http.Client, baseURL, cookie, workspaceID string) (*openCodeQuotaResponse, error) {
	if client == nil {
		return nil, errors.New("http client is required")
	}
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	targetURL := baseURL + "/workspace/" + url.PathEscape(strings.TrimSpace(workspaceID)) + "/go"
	req, errRequest := http.NewRequestWithContext(ctx, http.MethodGet, targetURL, nil)
	if errRequest != nil {
		return nil, fmt.Errorf("create OpenCode quota request: %w", errRequest)
	}
	req.Header.Set("Cookie", cookie)
	req.Header.Set("Accept", "text/html")
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36")

	resp, errDo := client.Do(req)
	if errDo != nil {
		return nil, fmt.Errorf("request OpenCode quota: %w", errDo)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return nil, &openCodeUpstreamError{StatusCode: resp.StatusCode, Message: "OpenCode cookie is expired or invalid"}
	}
	if resp.StatusCode == http.StatusMovedPermanently || resp.StatusCode == http.StatusFound {
		return nil, &openCodeUpstreamError{StatusCode: resp.StatusCode, Message: "OpenCode redirected to login; the cookie is expired or invalid"}
	}
	if resp.StatusCode != http.StatusOK {
		return nil, &openCodeUpstreamError{StatusCode: resp.StatusCode, Message: fmt.Sprintf("OpenCode returned HTTP %d", resp.StatusCode)}
	}

	body, errRead := io.ReadAll(io.LimitReader(resp.Body, openCodeMaxPageSize+1))
	if errRead != nil {
		return nil, fmt.Errorf("read OpenCode quota page: %w", errRead)
	}
	if len(body) > openCodeMaxPageSize {
		return nil, errors.New("OpenCode quota page exceeds the size limit")
	}
	if !strings.Contains(string(body), "$R") {
		return nil, errors.New("OpenCode quota page format changed or the cookie is invalid")
	}
	return parseOpenCodeQuota(string(body))
}

func parseOpenCodeQuota(html string) (*openCodeQuotaResponse, error) {
	scripts := extractOpenCodeInlineScripts(html)
	for _, script := range scripts {
		marker := strings.Index(script, "rollingUsage")
		if marker < 0 {
			continue
		}
		objectStart := marker
		for objectStart > 0 && script[objectStart] != '{' {
			objectStart--
		}
		if objectStart <= 0 {
			continue
		}
		block, errBlock := findOpenCodeBalancedObject(script, objectStart)
		if errBlock != nil {
			continue
		}
		var data openCodeQuotaData
		if errJSON := json.Unmarshal([]byte(normalizeOpenCodeJS(block)), &data); errJSON != nil {
			return nil, fmt.Errorf("parse OpenCode quota data: %w", errJSON)
		}
		return &openCodeQuotaResponse{
			Rolling:   makeOpenCodeQuotaWindow(data.RollingUsage),
			Weekly:    makeOpenCodeQuotaWindow(data.WeeklyUsage),
			Monthly:   makeOpenCodeQuotaWindow(data.MonthlyUsage),
			FetchedAt: time.Now().UTC(),
		}, nil
	}
	return nil, fmt.Errorf("OpenCode quota data not found in %d script(s)", len(scripts))
}

func makeOpenCodeQuotaWindow(limit openCodeUsageLimit) openCodeQuotaWindow {
	used := limit.UsagePercent
	if used < 0 {
		used = 0
	}
	if used > 100 {
		used = 100
	}
	return openCodeQuotaWindow{
		UsedPercent:      used,
		RemainingPercent: 100 - used,
		Status:           limit.Status,
		ResetInSeconds:   limit.ResetInSec,
	}
}

func normalizeOpenCodeJS(input string) string {
	normalized := openCodeRefAssignPattern.ReplaceAllString(input, "")
	normalized = openCodeNewDatePattern.ReplaceAllString(normalized, `"$1"`)
	normalized = strings.ReplaceAll(normalized, "!0", "true")
	normalized = strings.ReplaceAll(normalized, "!1", "false")
	return quoteOpenCodeBareKeys(normalized)
}

func quoteOpenCodeBareKeys(input string) string {
	var output strings.Builder
	output.Grow(len(input))
	for i := 0; i < len(input); {
		if input[i] == '"' {
			output.WriteByte(input[i])
			i++
			for i < len(input) && input[i] != '"' {
				if input[i] == '\\' && i+1 < len(input) {
					output.WriteByte(input[i])
					i++
				}
				output.WriteByte(input[i])
				i++
			}
			if i < len(input) {
				output.WriteByte(input[i])
				i++
			}
			continue
		}
		if isOpenCodeIdentifierStart(input[i]) && (i == 0 || !isOpenCodeIdentifierChar(input[i-1])) {
			end := i + 1
			for end < len(input) && isOpenCodeIdentifierChar(input[end]) {
				end++
			}
			word := input[i:end]
			colon := end
			for colon < len(input) && input[colon] == ' ' {
				colon++
			}
			if colon < len(input) && input[colon] == ':' && word != "true" && word != "false" && word != "null" {
				output.WriteByte('"')
				output.WriteString(word)
				output.WriteByte('"')
				i = end
				continue
			}
		}
		output.WriteByte(input[i])
		i++
	}
	return output.String()
}

func isOpenCodeIdentifierStart(value byte) bool {
	return value >= 'a' && value <= 'z' || value >= 'A' && value <= 'Z' || value == '_'
}

func isOpenCodeIdentifierChar(value byte) bool {
	return isOpenCodeIdentifierStart(value) || value >= '0' && value <= '9'
}

func findOpenCodeBalancedObject(input string, start int) (string, error) {
	if start >= len(input) || input[start] != '{' {
		return "", errors.New("OpenCode quota object start not found")
	}
	depth := 0
	inString := false
	for i := start; i < len(input); i++ {
		char := input[i]
		if inString {
			if char == '\\' {
				i++
				continue
			}
			if char == '"' {
				inString = false
			}
			continue
		}
		if char == '"' {
			inString = true
			continue
		}
		switch char {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return input[start : i+1], nil
			}
		}
	}
	return "", errors.New("OpenCode quota object is incomplete")
}

func extractOpenCodeInlineScripts(html string) []string {
	lowerHTML := strings.ToLower(html)
	searchFrom := 0
	var scripts []string
	for {
		tagStart := strings.Index(lowerHTML[searchFrom:], "<script")
		if tagStart < 0 {
			break
		}
		tagStart += searchFrom
		tagEnd := strings.Index(html[tagStart:], ">")
		if tagEnd < 0 {
			break
		}
		tagEnd += tagStart
		if strings.Contains(strings.ToLower(html[tagStart:tagEnd+1]), "src=") {
			searchFrom = tagEnd + 1
			continue
		}
		closeTag := strings.Index(lowerHTML[tagEnd+1:], "</script>")
		if closeTag < 0 {
			break
		}
		closeTag += tagEnd + 1
		script := html[tagEnd+1 : closeTag]
		if strings.Contains(script, "$R") {
			scripts = append(scripts, script)
		}
		searchFrom = closeTag + len("</script>")
	}
	return scripts
}
