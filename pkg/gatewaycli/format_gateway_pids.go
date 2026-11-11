package gatewaycli

import (
	"strconv"
	"strings"
)

// Comma joined positive pids, the first limit of them and an ellipsis when more exist, every pid for a negative limit
func FormatGatewayPids(pids []int, limit int) string {
	shown := pids
	if limit >= 0 && len(pids) > limit {
		shown = pids[:limit]
	}
	rendered := []string{}
	for _, pid := range shown {
		if pid > 0 {
			rendered = append(rendered, strconv.Itoa(pid))
		}
	}
	if limit >= 0 && len(pids) > limit {
		rendered = append(rendered, "...")
	}
	return strings.Join(rendered, ", ")
}
