package provider

// Rows of DeepSeek's flash models, which see images though the models.dev registry has not indexed them yet
func flashMetadata() map[string]map[string]any {
	rows := map[string]map[string]any{}
	for _, id := range []string{"deepseek-flash", "deepseek-v4-flash", "deepseek-v4.1-flash", "deepseek-v4-flash-vision-exp"} {
		rows[id] = map[string]any{
			"limit":      map[string]any{"context": float64(1_000_000), "output": float64(384_000)},
			"modalities": map[string]any{"input": []any{"text", "image"}, "output": []any{"text"}},
			"tool_call":  true,
			"reasoning":  true,
			"family":     "deepseek-flash",
		}
	}
	return rows
}
