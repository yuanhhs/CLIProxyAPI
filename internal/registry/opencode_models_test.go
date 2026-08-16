package registry

import "testing"

func TestGetOpenCodeModelsReturnsUniqueModels(t *testing.T) {
	models := GetOpenCodeModels()
	if len(models) != 26 {
		t.Fatalf("model count = %d, want 26", len(models))
	}

	seen := make(map[string]struct{}, len(models))
	for _, model := range models {
		if model == nil || model.ID == "" {
			t.Fatal("expected every OpenCode model to have an ID")
		}
		if _, exists := seen[model.ID]; exists {
			t.Fatalf("duplicate OpenCode model ID %q", model.ID)
		}
		seen[model.ID] = struct{}{}
	}
}
