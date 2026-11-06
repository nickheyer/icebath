package provider

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/nickheyer/icebath/pkg/observability"
)

func TestObservabilityHooksReadLocalCustomProviders(t *testing.T) {
	if observability.IsLocalEndpointHook == nil || observability.NamedCustomProviderHook == nil {
		t.Fatal("hooks not installed")
	}
	if !observability.IsLocalEndpointHook("http://127.0.0.1:11434/v1") || observability.IsLocalEndpointHook("https://api.openai.com/v1") {
		t.Fatal("local endpoint hook")
	}
	h := t.TempDir()
	t.Setenv("ICEBATH_HOME", h)
	body := "custom_providers:\n  - name: mylocal\n    base_url: http://localhost:8080/v1\n  - name: remote\n    base_url: https://llm.example.com/v1\n"
	if err := os.WriteFile(filepath.Join(h, "config.yaml"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	entry, _ := observability.NamedCustomProviderHook(ctx, "custom:mylocal").(map[string]any)
	if entry == nil || entry["base_url"] != "http://localhost:8080/v1" {
		t.Fatalf("named entry %v", entry)
	}
	if got := observability.NamedCustomProviderHook(ctx, "missing"); got != nil {
		t.Fatalf("missing entry %v", got)
	}
	for slot, want := range map[string]string{"custom:mylocal": "yes", "mylocal": "yes", "remote": "no", "missing": "no"} {
		got, err := observability.LocalModelProviderUsed(ctx, map[string]any{"model": map[string]any{"provider": slot}})
		if err != nil || got != want {
			t.Fatalf("%s: got %q %v want %q", slot, got, err, want)
		}
	}
}
