package gateway

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/config/roundtrip"
	"github.com/nickheyer/icebath/pkg/home"
)

func TestP6AllSectionsNullAtOnceThroughGatewayLoaderAndMigration(t *testing.T) {
	h := t.TempDir()
	t.Setenv("ICEBATH_HOME", h)
	t.Setenv(config.EnvManagedDir, filepath.Join(t.TempDir(), "no-managed"))
	config.InvalidateManagedCache()
	prevOut, prevNotice := config.FacadeStdout, config.FacadeNoticeOut
	config.FacadeStdout, config.FacadeNoticeOut = io.Discard, io.Discard
	t.Cleanup(func() { config.FacadeStdout, config.FacadeNoticeOut = prevOut, prevNotice; config.ResetCache() })
	ctx := context.Background()
	defaults := config.Defaults()
	latest := config.LatestConfigVersion()
	var topLevel []string
	var b strings.Builder
	b.WriteString("# every section null\n")
	for _, k := range config.MappingKeys(defaults) {
		if k != "_config_version" {
			topLevel = append(topLevel, k)
			b.WriteString(k + ": null\n")
		}
	}
	fmt.Fprintf(&b, "_config_version: %d\n", latest-1)
	cfg := home.ConfigPath(h)
	if err := os.WriteFile(cfg, []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	config.ResetCache()
	if _, err := LoadGatewayConfig(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := config.LoadUserEffective(cfg, config.EnvRefLookup); err != nil {
		t.Fatal(err)
	}
	effective, err := config.Load(h, config.EnvRefLookup)
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range topLevel {
		if _, isMap := defaults[k].(map[string]any); isMap {
			if _, ok := effective[k].(map[string]any); !ok {
				t.Fatalf("load_config()[%q] = %#v", k, effective[k])
			}
		}
	}
	if _, err := config.MigrateConfig(ctx, h, false, true); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(cfg)
	if err != nil {
		t.Fatal(err)
	}
	after := string(raw)
	got, err := config.SafeLoad(after)
	if err != nil {
		t.Fatal(err)
	}
	if v := roundtrip.Get(got, []string{"_config_version"}); !roundtrip.Equal(v, latest) {
		t.Fatalf("stamp %#v:\n%s", v, after)
	}
	if !strings.Contains(after, "# every section null") {
		t.Fatalf("comment dropped:\n%s", after)
	}
	if err := roundtrip.NoDuplicateTopLevel(after, "all-null migrate"); err != nil {
		t.Fatal(err)
	}
	config.ResetCache()
	if _, err := LoadGatewayConfig(ctx); err != nil {
		t.Fatal(err)
	}
}
