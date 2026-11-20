package gateway

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/home"
)

func TestColdFrontedPlatformsIsReadOnly(t *testing.T) {
	for _, tc := range []string{"native", "url-only", "disabled", "managed", "managed-only", "scoped"} {
		t.Run(tc, func(t *testing.T) {
			tmp := t.TempDir()
			primary, scoped, managed := filepath.Join(tmp, "primary"), filepath.Join(tmp, "secondary"), filepath.Join(tmp, "managed")
			for _, d := range []string{primary, scoped, managed} {
				if err := os.MkdirAll(d, 0o755); err != nil {
					t.Fatal(err)
				}
			}
			write := func(path, text string) {
				if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			enabled := "true"
			if tc == "disabled" {
				enabled = "false"
			}
			if tc != "native" && tc != "url-only" && tc != "managed-only" {
				write(filepath.Join(primary, "config.yaml"), "platforms:\n  relay:\n    enabled: "+enabled+"\n")
			}
			if tc == "managed" || tc == "managed-only" {
				write(filepath.Join(managed, "config.yaml"), "platforms:\n  relay:\n    enabled: false\n")
			}
			if tc == "scoped" {
				write(filepath.Join(scoped, "config.yaml"), "platforms:\n  relay:\n    enabled: false\n")
			}
			for _, root := range []string{primary, scoped} {
				write(filepath.Join(root, ".env"), "GATEWAY_RELAY_SECRET=dotenv-only-test-secret\n")
			}
			for _, kv := range os.Environ() {
				if k, _, _ := strings.Cut(kv, "="); strings.HasPrefix(k, "GATEWAY_RELAY_") {
					t.Setenv(k, "")
					os.Unsetenv(k)
				}
			}
			t.Setenv("HOME", tmp)
			t.Setenv("USERPROFILE", tmp)
			t.Setenv("ICEBATH_HOME", primary)
			t.Setenv("ICEBATH_MANAGED_DIR", managed)
			if tc != "native" {
				t.Setenv("GATEWAY_RELAY_URL", "wss://connector.example/relay")
				t.Setenv("GATEWAY_RELAY_PLATFORMS", "slack")
				t.Setenv("GATEWAY_RELAY_SECRET", "inherited-test-secret")
			}
			ctx := context.Background()
			if tc == "scoped" {
				ctx = home.WithOverride(ctx, scoped)
			}
			before := os.Environ()
			fronted := RelayFrontedPlatforms(ctx)
			after := os.Environ()
			slices.Sort(before)
			slices.Sort(after)
			if !slices.Equal(before, after) {
				t.Fatal("reading the fronted platforms changed the environment")
			}
			var names []string
			for p := range fronted {
				names = append(names, p)
			}
			want := []string(nil)
			if tc == "url-only" {
				want = []string{"slack"}
			}
			if !slices.Equal(names, want) {
				t.Fatalf("fronted %v want %v", names, want)
			}
		})
	}
}
