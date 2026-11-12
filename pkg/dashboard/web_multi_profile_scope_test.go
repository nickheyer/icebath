package dashboard

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/provider"
)

const (
	multiProfileAVal = "a-only-secret-0001"
	multiProfileBVal = "b-only-secret-0002"
)

// Dashboard client over a launch home and profile b, each with its own dotenv and the same probe config
func twoHomesClient(t *testing.T) (*testClient, string, string) {
	t.Helper()
	t.Setenv("A_ONLY_TOKEN", multiProfileAVal)
	t.Setenv("TELEGRAM_BOT_TOKEN", "")
	os.Unsetenv("TELEGRAM_BOT_TOKEN")
	c := newTestClient(t, dashboardTestApp(t))
	root := home.Root()
	b := filepath.Join(root, "profiles", "b")
	if err := os.MkdirAll(b, 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(path, text string) {
		if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(root, ".env"), "A_ONLY_TOKEN="+multiProfileAVal+"\n")
	write(filepath.Join(b, ".env"), "B_ONLY_TOKEN="+multiProfileBVal+"\nTELEGRAM_BOT_TOKEN=b-telegram-token\n")
	for _, h := range []string{root, b} {
		write(filepath.Join(h, "config.yaml"), "model:\n  default: openai/gpt-4o-mini\n  api_key: ${A_ONLY_TOKEN}\n"+
			"custom_probe:\n  a_ref: ${A_ONLY_TOKEN}\n  b_ref: ${B_ONLY_TOKEN}\n")
	}
	provider.ResetLaunchEnvSnapshotForTests()
	t.Cleanup(provider.ResetLaunchEnvSnapshotForTests)
	config.ResetCache()
	t.Cleanup(config.ResetCache)
	return c, root, b
}

// The custom_probe object of a GET /api/config response
func customProbe(t *testing.T, r *testResponse) *pyjson.Object {
	t.Helper()
	probe, ok := appObject(t, r).Get("custom_probe")
	if !ok {
		t.Fatalf("no custom_probe in %s", r.Body)
	}
	return probe.(*pyjson.Object)
}

func TestGetConfigForNamedProfileExpandsOnlyItsOwnSecrets(t *testing.T) {
	c, _, _ := twoHomesClient(t)
	probe := customProbe(t, c.request("GET", "/api/config?profile=b", nil, sessionHeaders()...))
	if v, _ := probe.Get("b_ref"); v != multiProfileBVal {
		t.Fatalf("b_ref = %#v", v)
	}
	if v, _ := probe.Get("a_ref"); v != "${A_ONLY_TOKEN}" {
		t.Fatalf("a_ref = %#v", v)
	}
	if !provider.IsMultiplexActive(context.Background()) {
		t.Fatal("hosting a second profile did not flip the process to multiplex")
	}
	if os.Getenv("A_ONLY_TOKEN") != multiProfileAVal {
		t.Fatalf("A_ONLY_TOKEN = %q", os.Getenv("A_ONLY_TOKEN"))
	}
	if _, ok := os.LookupEnv("B_ONLY_TOKEN"); ok {
		t.Fatal("B_ONLY_TOKEN leaked into the process environment")
	}
	probeA := customProbe(t, c.request("GET", "/api/config", nil, sessionHeaders()...))
	if v, _ := probeA.Get("a_ref"); v != multiProfileAVal {
		t.Fatalf("launch a_ref = %#v", v)
	}
	if v, _ := probeA.Get("b_ref"); v != "${B_ONLY_TOKEN}" {
		t.Fatalf("launch b_ref = %#v", v)
	}
}
