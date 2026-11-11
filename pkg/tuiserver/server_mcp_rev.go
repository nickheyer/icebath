package tuiserver

import (
	"context"
	"crypto/sha1"
	"encoding/hex"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

// Short hash of the mcp, mcp_servers and tools sections, empty when the config cannot be read
func computeMCPRev(ctx context.Context) (rev string) {
	defer func() {
		if recover() != nil {
			rev = ""
		}
	}()
	cfg := loadCfg(ctx)
	src := map[string]any{"mcp": cfg["mcp"], "mcp_servers": cfg["mcp_servers"], "tools": cfg["tools"]}
	encoded, err := pyjson.Dumps(src, pyjson.SortKeys(true), pyjson.Default(func(v any) (any, error) { return pyrepr.Str(v), nil }))
	if err != nil {
		return ""
	}
	sum := sha1.Sum([]byte(encoded))
	return hex.EncodeToString(sum[:])[:12]
}
