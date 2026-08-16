package management

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestFetchOpenCodeModelsUsesAPIKeyAndParsesModels(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/models" {
			t.Fatalf("path = %q", r.URL.Path)
		}
		if authorization := r.Header.Get("Authorization"); authorization != "Bearer test-key" {
			t.Fatalf("authorization = %q", authorization)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"model-a","owned_by":"opencode"},{"id":"model-a"},{"id":"  "},{"id":"model-b"}]}`))
	}))
	defer server.Close()

	models, errFetch := fetchOpenCodeModels(context.Background(), server.Client(), server.URL, "test-key")
	if errFetch != nil {
		t.Fatalf("fetchOpenCodeModels() error = %v", errFetch)
	}
	if len(models) != 2 || models[0].ID != "model-a" || models[1].ID != "model-b" {
		t.Fatalf("models = %#v", models)
	}
	if models[0].OwnedBy != "opencode" {
		t.Fatalf("owned_by = %q", models[0].OwnedBy)
	}
}

func TestFetchOpenCodeModelsMapsUpstreamError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer server.Close()

	_, errFetch := fetchOpenCodeModels(context.Background(), server.Client(), server.URL, "bad-key")
	upstreamErr, ok := errFetch.(*openCodeUpstreamError)
	if !ok || upstreamErr.StatusCode != http.StatusUnauthorized {
		t.Fatalf("error = %#v", errFetch)
	}
}
