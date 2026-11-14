package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/config"

	"github.com/nickheyer/icebath/pkg/tuiserver"
)

// Launch home under a fake user home with the scout profile folder, both empty
func inheritLaunchHomes(t *testing.T) (string, string) {
	t.Helper()
	tmp := t.TempDir()
	h := filepath.Join(tmp, ".icebath")
	profile := filepath.Join(h, "profiles", "scout")
	if err := os.MkdirAll(profile, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", tmp)
	t.Setenv("ICEBATH_HOME", h)
	installProcessWiring()
	return h, profile
}

// Writes one config file the way atomic_config_write does
func inheritLaunchWrite(t *testing.T, h, dir string, data map[string]any) {
	t.Helper()
	if err := config.AtomicConfigWrite(context.Background(), h, filepath.Join(dir, "config.yaml"), data, ""); err != nil {
		t.Fatal(err)
	}
}

// Raw user config of a home folder
func inheritLaunchRead(t *testing.T, dir string) map[string]any {
	t.Helper()
	raw, err := config.ReadUser(dir)
	if err != nil {
		t.Fatal(err)
	}
	return inheritLaunchPlain(t, map[string]any(raw)).(map[string]any)
}

func TestInheritLaunchModelCarriesACustomProviderGateway(t *testing.T) {
	for _, row := range []struct{ schema, provider string }{{"providers", "custom:my-gateway"}, {"custom_providers", "custom:my-gateway"}} {
		t.Run(row.schema, func(t *testing.T) {
			h, profile := inheritLaunchHomes(t)
			t.Setenv("FAKE_GW_TOKEN", "gw-FAKE-111")
			endpoint := map[string]any{"name": "my-gateway", "base_url": "http://127.0.0.1:11434/v1", "api_key": "${FAKE_GW_TOKEN}"}
			unrelated := map[string]any{"name": "unrelated", "base_url": "http://127.0.0.1:11435/v1"}
			retained := map[string]any{"name": "retained", "base_url": "http://127.0.0.1:11436/v1", "api_key": "${RETAINED_TOKEN}"}
			stale := map[string]any{"name": "my-gateway", "base_url": "http://127.0.0.1:11437/v1"}
			var catalog, retainedCatalog, expected any
			if row.schema == "providers" {
				catalog = map[string]any{"my-gateway": endpoint, "unrelated": unrelated}
				retainedCatalog = map[string]any{"stale": stale, "retained": retained}
				expected = map[string]any{"my-gateway": endpoint, "retained": retained}
			} else {
				catalog = []any{endpoint, unrelated}
				retainedCatalog = []any{stale, retained}
				expected = []any{endpoint, retained}
			}
			inheritLaunchWrite(t, h, h, map[string]any{"model": map[string]any{"provider": row.provider, "default": "my-finetune"},
				row.schema: catalog, "_config_version": config.Version})
			inheritLaunchWrite(t, h, profile, map[string]any{"tts": map[string]any{"provider": "edge"}, row.schema: retainedCatalog,
				"_config_version": config.Version})
			inherited, err := tuiserver.InheritLaunchModel(context.Background(), profile)
			if err != nil || !inherited {
				t.Fatalf("inherited %v %v", inherited, err)
			}
			cfg := inheritLaunchRead(t, profile)
			model, _ := cfg["model"].(map[string]any)
			if model["provider"] != row.provider || model["default"] != "my-finetune" {
				t.Fatalf("model %v", model)
			}
			if !reflect.DeepEqual(cfg[row.schema], inheritLaunchPlain(t, expected)) {
				t.Fatalf("%s %v", row.schema, cfg[row.schema])
			}
			var copied map[string]any
			if row.schema == "providers" {
				copied = cfg["providers"].(map[string]any)["my-gateway"].(map[string]any)
			} else {
				copied = cfg["custom_providers"].([]any)[0].(map[string]any)
			}
			if copied["api_key"] != "${FAKE_GW_TOKEN}" {
				t.Fatalf("copied key %v", copied["api_key"])
			}
		})
	}
}

func TestInheritLaunchModelPinsABareCustomEndpoint(t *testing.T) {
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/models") {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("content-type", "application/json")
		w.Write([]byte(`{"object": "list", "data": [{"id": "llama3", "object": "model"}]}`))
	}))
	defer endpoint.Close()
	for _, written := range []string{endpoint.URL + "/v1", "${LAB_URL}"} {
		t.Run(strings.NewReplacer("$", "", "{", "", "}", "").Replace(written), func(t *testing.T) {
			h, profile := inheritLaunchHomes(t)
			for _, name := range []string{"CUSTOM_BASE_URL", "OPENAI_BASE_URL"} {
				t.Setenv(name, "")
				os.Unsetenv(name)
			}
			t.Setenv("LAB_URL", endpoint.URL+"/v1")
			inheritLaunchWrite(t, h, h, map[string]any{"model": map[string]any{"provider": "custom", "default": "llama3", "base_url": written},
				"_config_version": config.Version})
			inheritLaunchWrite(t, h, profile, map[string]any{"tts": map[string]any{"provider": "edge"}, "_config_version": config.Version})
			inherited, err := tuiserver.InheritLaunchModel(context.Background(), profile)
			if err != nil || !inherited {
				t.Fatalf("inherited %v %v", inherited, err)
			}
			model, _ := inheritLaunchRead(t, profile)["model"].(map[string]any)
			if model["provider"] != "custom" || model["default"] != "llama3" || model["base_url"] != written {
				t.Fatalf("model %v", model)
			}
		})
	}
}

func TestInheritLaunchModelPreservesExplicitProfileSelection(t *testing.T) {
	h, profile := inheritLaunchHomes(t)
	inheritLaunchWrite(t, h, h, map[string]any{"model": map[string]any{"provider": "nous", "default": "launch-model"}})
	inheritLaunchWrite(t, h, profile, map[string]any{"model": map[string]any{"provider": "openrouter", "default": "profile-model"}})
	before := inheritLaunchRead(t, profile)
	inherited, err := tuiserver.InheritLaunchModel(context.Background(), profile)
	if err != nil || inherited {
		t.Fatalf("inherited %v %v", inherited, err)
	}
	if after := inheritLaunchRead(t, profile); !reflect.DeepEqual(after, before) {
		t.Fatalf("profile config changed %v", after)
	}
}

// JSON round trip of a value, the plain maps and lists a YAML load gives
func inheritLaunchPlain(t *testing.T, value any) any {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	var out any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return out
}
