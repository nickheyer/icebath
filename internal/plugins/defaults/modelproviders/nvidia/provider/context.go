package provider

import (
	"net/url"
	"strings"
)

// Window NVIDIA's own endpoint gives a model it serves larger than elsewhere: DeepSeek V4 Pro at 256K, on the canonical https address only
func contextLength(model, base string) int {
	parsed, err := url.Parse(strings.TrimRight(strings.TrimSpace(base), "/"))
	if err != nil || parsed.Scheme != "https" || parsed.Port() != "" && parsed.Port() != "443" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return 0
	}
	if strings.ToLower(parsed.Hostname()) == "integrate.api.nvidia.com" && strings.TrimRight(parsed.Path, "/") == "/v1" &&
		strings.EqualFold(strings.TrimSpace(model), "deepseek-ai/deepseek-v4-pro") {
		return 262_144
	}
	return 0
}
