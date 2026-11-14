package tuiserver

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
)

func TestStatErrorAfterACachedAbsentConfigIsAFailedRead(t *testing.T) {
	h := t.TempDir()
	t.Setenv("ICEBATH_HOME", h)
	dropCfgCache()
	t.Cleanup(dropCfgCache)
	ctx := context.Background()
	cfg, err := loadCfgRaw(ctx)
	if err != nil || len(cfg) != 0 {
		t.Fatalf("absent config read %v, %v", cfg, err)
	}
	if err := os.WriteFile(filepath.Join(h, "config.yaml"), []byte("display:\n  skin: mono\nmodel:\n  default: keep-me\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(config.SwapConfigVersion(func(_ context.Context, configPath string) ([]any, error) {
		return nil, &os.PathError{Op: "stat", Path: configPath, Err: os.ErrPermission}
	}))
	if cfg, err := loadCfgRaw(ctx); err == nil {
		t.Fatalf("a stat error after a cached absent config read as %v", cfg)
	}
}
