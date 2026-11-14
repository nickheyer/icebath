package gateway

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/profiles"
	"github.com/nickheyer/icebath/pkg/provider"
)

func TestCronProfileHomesServeEveryLiveProfile(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	defaultHome := filepath.Join(tmp, ".icebath")
	t.Setenv("ICEBATH_HOME", defaultHome)
	for _, name := range []string{"worker", "guest", "gone"} {
		dir := filepath.Join(defaultHome, "profiles", name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte("{}\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := home.MarkProfileDeleted(filepath.Join(defaultHome, "profiles", "gone")); err != nil {
		t.Fatal(err)
	}
	multiplex := true
	cfg := DefaultGatewayConfig(context.Background())
	cfg.MultiplexProfiles = &multiplex
	homes, err := multiplexProfileHomes(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, served := range homes {
		names = append(names, served.Name)
	}
	if !reflect.DeepEqual(names, []string{"default", "guest", "worker"}) {
		t.Fatalf("served %v", names)
	}
}

func TestMultiplexProfileHomesPrefersTheBootReservation(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	reserved := []profiles.ServedProfile{{Name: "default", Home: "/h"}, {Name: "coder", Home: "/h/profiles/coder"}}
	cfg := DefaultGatewayConfig(context.Background())
	cfg.runtimeProfileHomes = &reserved
	got, err := multiplexProfileHomes(context.Background(), cfg)
	if err != nil || !reflect.DeepEqual(got, reserved) {
		t.Fatalf("got %v %v", got, err)
	}
	got[0].Name = "mutated"
	if reserved[0].Name != "default" {
		t.Fatal("caller mutation reached the reservation")
	}
	empty := []profiles.ServedProfile{}
	cfg.runtimeProfileHomes = &empty
	if got, err := multiplexProfileHomes(context.Background(), cfg); err != nil || got == nil || len(got) != 0 {
		t.Fatalf("empty reservation fell through to the directory read: %v %v", got, err)
	}
}

// Writes config.yaml text into a fresh home bound as ICEBATH_HOME
func gatewayConfigHome(t *testing.T, text string) string {
	t.Helper()
	h := t.TempDir()
	t.Setenv("ICEBATH_HOME", h)
	if err := os.WriteFile(filepath.Join(h, "config.yaml"), []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
	return h
}

func TestLoadGatewayConfigReadsTheActiveHomeAndAnExplicitPath(t *testing.T) {
	gatewayConfigHome(t, "model:\n  default: gpt-home\n")
	cfg := loadGatewayConfig(context.Background(), "")
	if m, _ := cfg["model"].(map[string]any); m == nil || m["default"] != "gpt-home" {
		t.Fatalf("active home config %v", cfg)
	}
	other := t.TempDir()
	if err := os.WriteFile(filepath.Join(other, "config.yaml"), []byte("model:\n  default: gpt-other\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg = loadGatewayConfig(context.Background(), filepath.Join(other, "config.yaml"))
	if m, _ := cfg["model"].(map[string]any); m == nil || m["default"] != "gpt-other" {
		t.Fatalf("explicit path config %v", cfg)
	}
	override := t.TempDir()
	if err := os.WriteFile(filepath.Join(override, "config.yaml"), []byte("model:\n  default: gpt-override\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg = loadGatewayConfig(home.WithOverride(context.Background(), override), "")
	if m, _ := cfg["model"].(map[string]any); m == nil || m["default"] != "gpt-override" {
		t.Fatalf("override home config %v", cfg)
	}
}

func TestLoadGatewayConfigIsEmptyOnFailure(t *testing.T) {
	h := gatewayConfigHome(t, "")
	dir := filepath.Join(h, "config.yaml.d")
	if err := os.MkdirAll(filepath.Join(dir, "config.yaml"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := loadGatewayConfig(context.Background(), filepath.Join(dir, "config.yaml"))
	if cfg == nil || len(cfg) != 0 {
		t.Fatalf("unreadable config %v", cfg)
	}
}

func TestLoadGatewayConfigExpandsRefsThroughTheSecretScope(t *testing.T) {
	h := gatewayConfigHome(t, "model:\n  default: ${GW_MODEL_REF}\n")
	t.Setenv("GW_MODEL_REF", "process-model")
	cfg := loadGatewayConfig(context.Background(), "")
	if m, _ := cfg["model"].(map[string]any); m == nil || m["default"] != "process-model" {
		t.Fatalf("unscoped expansion %v", cfg)
	}
	scoped := provider.WithSecretScope(context.Background(), map[string]string{"GW_MODEL_REF": "scoped-model"}, h)
	cfg = loadGatewayConfig(scoped, "")
	if m, _ := cfg["model"].(map[string]any); m == nil || m["default"] != "scoped-model" {
		t.Fatalf("scoped expansion %v", cfg)
	}
}

func TestResolveGatewayModel(t *testing.T) {
	gatewayConfigHome(t, "model:\n  default: from-home\n")
	ctx := context.Background()
	cases := []struct {
		name string
		cfg  map[string]any
		want string
	}{
		{"nil_config_loads_the_gateway_config", nil, "from-home"},
		{"string_model", map[string]any{"model": "plain"}, "plain"},
		{"default_key", map[string]any{"model": map[string]any{"default": "d", "model": "m"}}, "d"},
		{"model_key_when_default_empty", map[string]any{"model": map[string]any{"default": "", "model": "m"}}, "m"},
		{"truthy_non_string_default_wins", map[string]any{"model": map[string]any{"default": 5, "model": "m"}}, ""},
		{"missing_model", map[string]any{}, ""},
		{"non_string_model", map[string]any{"model": []any{"x"}}, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := resolveGatewayModel(ctx, c.cfg); got != c.want {
				t.Fatalf("got %q want %q", got, c.want)
			}
		})
	}
}
