package monitoring

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/google/uuid"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

func asConfigMap(v any) (map[string]any, bool) {
	switch m := v.(type) {
	case map[string]any:
		return m, m != nil
	case config.Tree:
		return m, m != nil
	}
	return nil, false
}

// Stable install id of the monitoring section, a fresh uuid minted and persisted to config.yaml when empty
func EnsureInstallID(ctx context.Context, cfg map[string]any) string {
	if mon, ok := asConfigMap(cfg["monitoring"]); ok {
		if existing, isStr := mon["install_id"].(string); isStr && pystr.Strip(existing) != "" {
			return existing
		}
	}
	minted := uuid.NewString()
	if err := persistInstallID(ctx, minted); err != nil {
		slog.Debug(fmt.Sprintf("install_id persist failed; using ephemeral id: %v", err))
	}
	if cfg != nil {
		if _, ok := cfg["monitoring"]; !ok {
			cfg["monitoring"] = map[string]any{}
		}
		if mon, ok := asConfigMap(cfg["monitoring"]); ok {
			mon["install_id"] = minted
		}
	}
	return minted
}

// Writes the minted id into the loaded config unless another process already stored one
func persistInstallID(ctx context.Context, minted string) error {
	h := home.Dir(ctx)
	fresh, err := config.Load(h, nil)
	if err != nil {
		return err
	}
	if _, ok := fresh["monitoring"]; !ok {
		fresh["monitoring"] = map[string]any{}
	}
	slot, ok := asConfigMap(fresh["monitoring"])
	if !ok || pystr.Strip(pyrepr.Str(orEmpty(slot["install_id"]))) != "" {
		return nil
	}
	slot["install_id"] = minted
	return config.Save(h, fresh)
}

// Python value or empty string, the falsy default of str(x or "")
func orEmpty(v any) any {
	if pyrepr.Truthy(v) {
		return v
	}
	return ""
}
