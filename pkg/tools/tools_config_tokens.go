package tools

import (
	"context"
	"sync"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/tokenizers/tiktoken"
)

type toolTokenKey struct {
	scope      string
	generation int64
}

var (
	toolTokenCacheMu sync.Mutex
	toolTokenCache   = map[toolTokenKey]map[string]int{}
	// Seam over the cl100k_base encoding load
	toolTokenEncoding = tiktoken.CL100kBase
)

// cl100k_base tokens of each registered tool's function schema, cached per home and registry generation, empty without the encoding
func estimateToolTokens(ctx context.Context) (map[string]int, error) {
	key := toolTokenKey{home.Key(home.Dir(ctx)), Default.Generation()}
	toolTokenCacheMu.Lock()
	defer toolTokenCacheMu.Unlock()
	if counts, ok := toolTokenCache[key]; ok {
		return counts, nil
	}
	enc, err := toolTokenEncoding()
	if err != nil {
		toolsConfigLogger().Debug("tiktoken unavailable; skipping tool token estimation")
		toolTokenCache[key] = map[string]int{}
		return toolTokenCache[key], nil
	}
	counts := map[string]int{}
	for _, name := range Default.AllToolNames(ctx, "") {
		schema := Default.Schema(ctx, "", name)
		if len(schema) == 0 {
			continue
		}
		wire := pyjson.NewObject()
		wire.Set("type", "function")
		wire.Set("function", schema)
		text, err := pyjson.Dumps(wire)
		if err != nil {
			return nil, err
		}
		counts[name] = len(enc.EncodeOrdinary(text))
	}
	toolTokenCache[key] = counts
	return counts, nil
}
