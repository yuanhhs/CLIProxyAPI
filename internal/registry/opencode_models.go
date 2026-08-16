package registry

// openCodeBuiltinModelIDs mirrors the OpenCode Go model catalog. The runtime
// refreshes an auth's registration with the upstream /models response whenever
// that credential's models are queried through the management API.
var openCodeBuiltinModelIDs = []string{
	"minimax-m3",
	"minimax-m2.7",
	"minimax-m2.5",
	"kimi-k3",
	"kimi-k2.7-code",
	"kimi-k2.6",
	"kimi-k2.5",
	"glm-5.2",
	"glm-5.3",
	"glm-5.1",
	"glm-5",
	"deepseek-v4-pro",
	"deepseek-v4-flash",
	"qwen3.7-max",
	"qwen3.8-max",
	"qwen3.7-plus",
	"qwen3.6-plus",
	"qwen3.5-plus",
	"mimo-v2-pro",
	"mimo-v2-omni",
	"mimo-v2.5-pro",
	"mimo-v2.5",
	"hy3",
	"hy3-preview",
	"gpt-5.6-luna",
	"grok-4.5",
}

// GetOpenCodeModels returns the locally known OpenCode Go models. This keeps
// OpenCode routable before the first successful upstream model query.
func GetOpenCodeModels() []*ModelInfo {
	models := make([]*ModelInfo, 0, len(openCodeBuiltinModelIDs))
	for _, id := range openCodeBuiltinModelIDs {
		models = append(models, &ModelInfo{
			ID:      id,
			Object:  "model",
			OwnedBy: "opencode",
			Type:    "opencode",
		})
	}
	return models
}
