package httpclient

import "strings"

// Name token every outbound request carries first in its User-Agent
const Product = "icebath"

// Product token with its version and optional parenthesised comments such as the surface and OS
func UserAgent(version string, comments ...string) string {
	agent := Product
	if v := strings.TrimSpace(version); v != "" {
		agent += "/" + v
	}
	var kept []string
	for _, c := range comments {
		if c = strings.TrimSpace(c); c != "" {
			kept = append(kept, c)
		}
	}
	if len(kept) > 0 {
		agent += " (" + strings.Join(kept, "; ") + ")"
	}
	return agent
}
