package tuiserver

import (
	"context"
	"sort"
	"strings"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/tuigateway"
)

// Sorted key value pairs of the compression and context keys that must apply on the next turn
func tuiCompressionConfigSignature(ctx context.Context, cfg map[string]any) []any {
	keys := gateway.ExtractCacheBustingConfig(ctx, cfg)
	picked := map[string]any{}
	for k, v := range keys {
		if strings.HasPrefix(k, "compression.") || k == "model.context_length" {
			picked[k] = v
		}
	}
	compression := map[string]any{}
	if cfg != nil {
		if m, ok := cfg["compression"].(map[string]any); ok {
			compression = m
		}
	}
	for _, k := range []string{"idle_compact_after_seconds", "tail_mode"} {
		picked["compression."+k] = compression[k]
	}
	names := make([]string, 0, len(picked))
	for k := range picked {
		names = append(names, k)
	}
	sort.Strings(names)
	out := make([]any, 0, len(names))
	for _, k := range names {
		out = append(out, []any{k, picked[k]})
	}
	return out
}

// Adopts compression and context length edits at turn start by updating the live compressor in place
func syncAgentCompressionWithConfig(ctx context.Context, sid string, session *tuigateway.Session) {
	a := sessionAgent(session)
	if a == nil {
		return
	}
	cfg := loadCfg(ctx)
	if cfg == nil {
		cfg = map[string]any{}
	}
	signature := tuiCompressionConfigSignature(ctx, cfg)
	seen := session.Get("config_compression_seen")
	session.Set("config_compression_seen", signature)
	if seen != nil && pyrepr.Equal(signature, seen) {
		return
	}
	var failure any
	func() {
		defer func() {
			if r := recover(); r != nil {
				failure = r
			}
		}()
		if err := a.ApplyLiveCompressionConfig(ctx, cfg); err != nil {
			failure = err
		}
	}()
	if failure != nil {
		logger().Warn("Could not apply live compression config for " + sid + ": " + failureText(failure))
	}
}
