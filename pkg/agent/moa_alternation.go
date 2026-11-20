package agent

import (
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/provider"
)

// Route and model identity of an aggregator destination, base_url preferred over the provider slug
func destinationKey(runtime map[string]any) [2]string {
	raw := runtime["base_url"]
	if !pyTruthy(raw) {
		raw = runtime["provider"]
	}
	route := ""
	if pyTruthy(raw) {
		route = pyrepr.Str(raw)
	}
	route = strings.TrimRight(pystr.Strip(route), "/")
	model := ""
	if pyTruthy(runtime["model"]) {
		model = pyrepr.Str(runtime["model"])
	}
	return [2]string{route, pystr.Strip(model)}
}

// Rows with adjacent user turns folded into one, the input itself and false when nothing merged
func mergeSameRoleMessages(messages []map[string]any) ([]map[string]any, bool) {
	merged := make([]map[string]any, 0, len(messages))
	changed := false
	for _, message := range messages {
		if len(merged) > 0 {
			prev := merged[len(merged)-1]
			if prev["role"] == "user" && message["role"] == "user" {
				if content, ok := mergeUserContent(contentOrEmpty(prev), contentOrEmpty(message)); ok {
					row := make(map[string]any, len(prev))
					for k, v := range prev {
						row[k] = v
					}
					row["content"] = content
					merged[len(merged)-1] = row
					changed = true
					continue
				}
			}
		}
		merged = append(merged, message)
	}
	if !changed {
		return messages, false
	}
	return merged, true
}

// Message content with the empty string standing in for an absent key
func contentOrEmpty(m map[string]any) any {
	if v, ok := m["content"]; ok {
		return v
	}
	return ""
}

// True when the aggregator destination rejected the request for adjacent same role messages
func isRoleAlternationRejection(err error, runtime map[string]any) (out bool) {
	defer func() {
		if recover() != nil {
			out = false
		}
	}()
	str := func(k string) string {
		if !pyTruthy(runtime[k]) {
			return ""
		}
		return pyrepr.Str(runtime[k])
	}
	classified := provider.ClassifyError(provider.ClassifyInput{Err: err, Provider: str("provider"), Model: str("model"), BaseURL: str("base_url")})
	return classified != nil && classified.Reason == provider.ReasonRoleAlternation
}
