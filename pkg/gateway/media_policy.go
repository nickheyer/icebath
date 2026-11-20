package gateway

import (
	"context"
	"os"
	"strings"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

var mediaFlagEnvs = [2][2]string{
	{"strict", "ICEBATH_MEDIA_DELIVERY_STRICT"},
	{"trust_recent_files", "ICEBATH_MEDIA_TRUST_RECENT_FILES"},
}

const (
	mediaAllowDirsEnv          = "ICEBATH_MEDIA_ALLOW_DIRS"
	mediaTrustRecentSecondsEnv = "ICEBATH_MEDIA_TRUST_RECENT_SECONDS"
)

var mediaTruthy = map[string]bool{"1": true, "true": true, "yes": true, "on": true}

// Gateway section of the routed profile config under a home override, ok false without an override
func routedGatewayCfg(ctx context.Context) (map[string]any, bool) {
	if home.Override(ctx) == "" {
		return nil, false
	}
	tree, err := config.LoadReadonly(ctx, home.Dir(ctx), nil)
	if err != nil {
		return map[string]any{}, true
	}
	if m, ok := tree["gateway"].(map[string]any); ok {
		return m, true
	}
	return map[string]any{}, true
}

// Environment value or its default when unset
func envOr(name, def string) string {
	if v, ok := os.LookupEnv(name); ok {
		return v
	}
	return def
}

// Whether strict media delivery is on for the routed profile or the process env
func MediaDeliveryStrict(ctx context.Context) bool {
	if cfg, ok := routedGatewayCfg(ctx); ok {
		return pyrepr.Truthy(cfg["strict"])
	}
	return mediaTruthy[pystr.Lower(pystr.Strip(envOr(mediaFlagEnvs[0][1], "0")))]
}

// Operator media allowlist joined with the path list separator
func MediaDeliveryAllowDirs(ctx context.Context) string {
	if cfg, ok := routedGatewayCfg(ctx); ok {
		return allowDirsStr(cfg["media_delivery_allow_dirs"])
	}
	return os.Getenv(mediaAllowDirsEnv)
}

// Whether freshly produced files are trusted for delivery, on unless switched off
func MediaDeliveryTrustRecent(ctx context.Context) bool {
	if cfg, ok := routedGatewayCfg(ctx); ok {
		v, present := cfg["trust_recent_files"]
		return !present || pyrepr.Truthy(v)
	}
	switch pystr.Lower(pystr.Strip(envOr(mediaFlagEnvs[1][1], "1"))) {
	case "0", "false", "no", "off", "":
		return false
	}
	return true
}

// Raw recency window text, empty for the validator default
func MediaDeliveryTrustRecentSeconds(ctx context.Context) string {
	if cfg, ok := routedGatewayCfg(ctx); ok {
		raw := cfg["trust_recent_files_seconds"]
		if raw == nil {
			return ""
		}
		return pyrepr.Str(raw)
	}
	return os.Getenv(mediaTrustRecentSecondsEnv)
}

// Gateway section of the given config, loading the home config when none is given
func loadMediaGatewayCfg(ctx context.Context, cfg map[string]any) map[string]any {
	if cfg == nil {
		tree, err := config.Load(home.Dir(ctx), nil)
		if err != nil {
			return map[string]any{}
		}
		cfg = tree
	}
	if m, ok := cfg["gateway"].(map[string]any); ok {
		return m
	}
	return map[string]any{}
}

// Sets the env only when it is unset or empty and the value is non empty
func setEnvDefault(name, value string) {
	if value != "" && os.Getenv(name) == "" {
		os.Setenv(name, value)
	}
}

// Allowlist value as one separator joined string, lists dropping falsy entries
func allowDirsStr(v any) string {
	switch t := v.(type) {
	case []any:
		parts := make([]string, 0, len(t))
		for _, p := range t {
			if pyrepr.Truthy(p) {
				parts = append(parts, pyrepr.Str(p))
			}
		}
		return strings.Join(parts, string(os.PathListSeparator))
	case []string:
		parts := make([]string, 0, len(t))
		for _, p := range t {
			if p != "" {
				parts = append(parts, p)
			}
		}
		return strings.Join(parts, string(os.PathListSeparator))
	case string:
		return t
	}
	return ""
}

// Bridges the gateway media policy config into the env, env wins and a home override skips it
func ApplyMediaPolicyEnv(ctx context.Context, cfg map[string]any) {
	if home.Override(ctx) != "" {
		return
	}
	gatewayCfg := loadMediaGatewayCfg(ctx, cfg)
	if len(gatewayCfg) == 0 {
		return
	}
	for _, kv := range mediaFlagEnvs {
		if flag, ok := gatewayCfg[kv[0]]; ok && flag != nil {
			v := "0"
			if pyrepr.Truthy(flag) {
				v = "1"
			}
			setEnvDefault(kv[1], v)
		}
	}
	if allow := gatewayCfg["media_delivery_allow_dirs"]; pyrepr.Truthy(allow) {
		setEnvDefault(mediaAllowDirsEnv, allowDirsStr(allow))
	}
}
