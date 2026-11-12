package selfupdate

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

const legacyAgentBrowserEnv = "AGENT_BROWSER_ENGINE=lightpanda\nAGENT_BROWSER_PROFILE=/probe/chrome-profile\nAGENT_BROWSER_HEADED=true\n"

// Real ICEBATH_HOME holding the legacy .env and the config text, no config.yaml when nil
func legacyAgentBrowserHome(t *testing.T, text *string) string {
	t.Helper()
	for _, name := range []string{"AGENT_BROWSER_EXECUTABLE_PATH", "AGENT_BROWSER_PROFILE", "AGENT_BROWSER_ARGS", "AGENT_BROWSER_HEADED", "AGENT_BROWSER_ENGINE", "CAMOFOX_URL"} {
		t.Setenv(name, "")
		os.Unsetenv(name)
	}
	h := filepath.Join(t.TempDir(), "home")
	if err := os.Mkdir(h, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(h, ".env"), []byte(legacyAgentBrowserEnv), 0o644); err != nil {
		t.Fatal(err)
	}
	if text != nil {
		if err := os.WriteFile(filepath.Join(h, "config.yaml"), []byte(*text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("ICEBATH_HOME", h)
	config.ResetCache()
	t.Cleanup(config.ResetCache)
	return h
}

// Browser engine, headed mode and profile directory the active home resolves right now
func agentBrowserRuntime(t *testing.T, h string) map[string]any {
	t.Helper()
	config.ResetCache()
	cfg, err := config.Load(h, config.EnvRefLookup)
	if err != nil {
		t.Fatal(err)
	}
	b := config.AsMap(cfg["browser"])
	return map[string]any{"engine": b["engine"], "headed": b["headed"], "user_data_dir": b["user_data_dir"]}
}

func agentBrowserStamp(t *testing.T, h string) any {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(h, "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	doc, err := config.SafeLoad(string(raw))
	if err != nil {
		t.Fatal(err)
	}
	return config.AsMap(doc)["_config_version"]
}

func TestBootKeepsTheLegacyBrowserSettings(t *testing.T) {
	unstamped, v51 := "{}\n", "_config_version: 51\n"
	for name, text := range map[string]*string{"missing": nil, "unstamped": &unstamped, "v51": &v51} {
		t.Run(name, func(t *testing.T) {
			h := legacyAgentBrowserHome(t, text)
			res, err := StepMigrateConfig(context.Background())
			if err != nil || !pyrepr.Truthy(res["ok"]) {
				t.Fatalf("boot %v %v", res, err)
			}
			kept := map[string]any{"engine": "lightpanda", "headed": true, "user_data_dir": "/probe/chrome-profile"}
			if got := agentBrowserRuntime(t, h); !reflect.DeepEqual(got, kept) {
				t.Fatalf("runtime %v", got)
			}
			if got := agentBrowserStamp(t, h); !pyrepr.Equal(got, config.LatestConfigVersion()) {
				t.Fatalf("stamp %v", got)
			}
		})
	}
}

func TestBootLeavesAHomeWithoutLegacySettingsAsItWas(t *testing.T) {
	unstamped := "{}\n"
	for name, text := range map[string]*string{"missing": nil, "unstamped": &unstamped} {
		t.Run(name, func(t *testing.T) {
			h := legacyAgentBrowserHome(t, text)
			if err := os.WriteFile(filepath.Join(h, ".env"), nil, 0o644); err != nil {
				t.Fatal(err)
			}
			res, err := StepMigrateConfig(context.Background())
			if err != nil || !pyrepr.Truthy(res["ok"]) {
				t.Fatalf("boot %v %v", res, err)
			}
			got, err := os.ReadFile(filepath.Join(h, "config.yaml"))
			if text == nil {
				if !os.IsNotExist(err) {
					t.Fatalf("config.yaml written %q %v", got, err)
				}
				return
			}
			if err != nil || string(got) != *text {
				t.Fatalf("config.yaml %q %v", got, err)
			}
		})
	}
}
