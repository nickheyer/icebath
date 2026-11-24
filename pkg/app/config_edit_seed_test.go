package app

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/plugins"
	"github.com/nickheyer/icebath/pkg/selfupdate"
	"github.com/nickheyer/icebath/pkg/settings"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Every chat platform resolves every display setting it may override exactly as with no config at all
func assertKeepsPlatformDisplayDefaults(t *testing.T, ctx context.Context) {
	t.Helper()
	plugins.DiscoverPlugins(ctx, false)
	platforms := settings.CorePlatforms()
	for _, e := range gateway.Registry.AllEntries(ctx) {
		if e.DisplayTier != "" {
			platforms = append(platforms, e.Name)
		}
	}
	slices.Sort(platforms)
	for _, p := range platforms {
		chat := settings.WithPlatform(ctx, p)
		for _, e := range settings.All() {
			if !e.Info().Platforms {
				continue
			}
			got, err := settings.Resolve(chat, e.Info().Key)
			if err != nil {
				t.Fatal(err)
			}
			want, _ := settings.Resolve(settings.DefaultsOnly(chat), e.Info().Key)
			if !reflect.DeepEqual(got.Value, want.Value) {
				t.Fatalf("%s %s resolves %v want %v", p, e.Info().Key, got.Value, want.Value)
			}
		}
	}
}

// Runs one doctor check, a panicking one failing the case with its message
func runDoctorCheck(t *testing.T, fn func() (int, error)) (fixed int, err error) {
	t.Helper()
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("%v", r)
		}
	}()
	return fn()
}

func TestEverySeederKeepsEveryPlatformDisplayDefault(t *testing.T) {
	seeders := []struct {
		name string
		seed func(t *testing.T, ctx context.Context, tmp, h string)
	}{
		{"config-edit-template", func(t *testing.T, ctx context.Context, _, h string) {
			t.Setenv("EDITOR", "true")
			swapSeam(t, &config.FacadeStdout, io.Writer(io.Discard))
			if err := config.EditConfig(ctx, h); err != nil {
				t.Fatal(err)
			}
		}},
		{"config-edit-no-template", func(t *testing.T, ctx context.Context, tmp, h string) {
			t.Setenv("EDITOR", "true")
			if _, err := config.SeedConfigFile(ctx, h, home.ConfigPath(h), filepath.Join(tmp, "no-checkout", "cli-config.yaml.example")); err != nil {
				t.Fatal(err)
			}
			if err := config.EditConfig(ctx, h); err != nil {
				t.Fatal(err)
			}
		}},
		{"blank-slate", func(t *testing.T, ctx context.Context, _, h string) {
			cfg, err := config.LoadContext(ctx, h, nil)
			if err != nil {
				t.Fatal(err)
			}
			if err := tools.BlankSlateMinimizeConfig(cfg); err != nil {
				t.Fatal(err)
			}
			if err := config.SaveConfig(ctx, h, cfg, config.SaveOptions{}); err != nil {
				t.Fatal(err)
			}
		}},
		{"doctor-fix", func(t *testing.T, ctx context.Context, tmp, h string) {
			root := filepath.Join(tmp, "checkout")
			if err := os.MkdirAll(root, 0o755); err != nil {
				t.Fatal(err)
			}
			template, err := os.ReadFile(filepath.Join(config.GetProjectRoot(), "cli-config.yaml.example"))
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(root, "cli-config.yaml.example"), template, 0o644); err != nil {
				t.Fatal(err)
			}
			fixed, err := runDoctorCheck(t, func() (int, error) { return selfupdate.DoctorCheckConfigFile(ctx, h, root, true, io.Discard) })
			if err != nil || fixed != 1 {
				t.Fatalf("doctor fix %d %v", fixed, err)
			}
			if runtime.GOOS != "windows" {
				if info, err := os.Stat(home.ConfigPath(h)); err != nil || info.Mode().Perm() != 0o600 {
					t.Fatalf("config.yaml mode %v %v", info, err)
				}
			}
		}},
	}
	for _, s := range seeders {
		t.Run(s.name, func(t *testing.T) {
			tmp := t.TempDir()
			h := filepath.Join(tmp, "home")
			if err := os.MkdirAll(h, 0o700); err != nil {
				t.Fatal(err)
			}
			t.Setenv("ICEBATH_HOME", h)
			ctx := home.WithOverride(context.Background(), h)
			s.seed(t, ctx, tmp, h)
			configPath := home.ConfigPath(h)
			if _, err := os.Stat(configPath); err != nil {
				t.Fatalf("seeder left no config.yaml: %v", err)
			}
			assertKeepsPlatformDisplayDefaults(t, ctx)
		})
	}
}

// Replaces a seam for the test, restoring it at cleanup
func swapSeam[T any](t *testing.T, target *T, value T) {
	t.Helper()
	prev := *target
	*target = value
	t.Cleanup(func() { *target = prev })
}
