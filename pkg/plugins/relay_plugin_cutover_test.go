package plugins

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/tools"
)

// legacy_relay_plugin_keys keeps only the removed Relay identities of list, tuple and set values, sorted once each
func TestLegacyRelayPluginKeysIn(t *testing.T) {
	cases := []struct {
		in   any
		want []string
	}{
		{[]any{"observability/nemo_relay", "disk-cleanup", "nemo_relay", 7, "nemo_relay"}, []string{"nemo_relay", "observability/nemo_relay"}},
		{[]string{"disk-cleanup"}, []string{}},
		{map[string]bool{"nemo_relay": true}, []string{"nemo_relay"}},
		{map[string]struct{}{"observability/nemo_relay": {}}, []string{"observability/nemo_relay"}},
		{map[any]bool{"nemo_relay": true, 3: true}, []string{"nemo_relay"}},
		{"nemo_relay", []string{}},
		{nil, []string{}},
		{map[string]any{"nemo_relay": true}, []string{}},
	}
	for _, c := range cases {
		if got := LegacyRelayPluginKeysIn(c.in); !reflect.DeepEqual(got, c.want) {
			t.Fatalf("%v got %v", c.in, got)
		}
	}
}

// Home holding a user plugin under observability/nemo_relay whose manifest name is name
func removedRelayHome(t *testing.T, name, enabled string) string {
	t.Helper()
	root := t.TempDir()
	h := filepath.Join(root, "icebath-home")
	dir := filepath.Join(h, "plugins", "observability", "nemo_relay")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(dir, "plugin.yaml"), "name: "+name+"\nversion: \"1.0\"\n")
	writeFile(t, filepath.Join(dir, "__init__.py"), "def register(ctx):\n    raise RuntimeError('must not load')\n")
	if enabled != "" {
		writeFile(t, filepath.Join(h, "config.yaml"), "plugins:\n  enabled:\n  - "+enabled+"\n")
	}
	t.Setenv("HOME", filepath.Join(root, "os-home"))
	t.Setenv("ICEBATH_HOME", h)
	t.Setenv("ICEBATH_BUNDLED_PLUGINS", filepath.Join(root, "bundled-plugins"))
	return h
}

// Enabling a removed Relay identity exits 1 before resolving the name or saving config
func TestEnableRejectsRemovedRelayPluginWithoutDiscovery(t *testing.T) {
	for _, name := range []string{"nemo_relay", "observability/nemo_relay"} {
		t.Run(name, func(t *testing.T) {
			h := filepath.Join(t.TempDir(), "missing-home")
			t.Setenv("ICEBATH_HOME", h)
			var out bytes.Buffer
			err := CmdEnable(homeCtx(h), name, nil, &out)
			var exit *CommandExit
			if !errors.As(err, &exit) || exit.Code != 1 {
				t.Fatalf("err %v", err)
			}
			if _, statErr := os.Stat(h); !os.IsNotExist(statErr) {
				t.Fatalf("home touched %v", statErr)
			}
			if text := out.String(); !strings.Contains(text, name) || !strings.Contains(text, tools.RelayPluginsConfigEnv) {
				t.Fatalf("output %q", text)
			}
		})
	}
}

// An alias resolving to the removed Relay key exits 1 naming the key without saving config
func TestEnableRejectsAliasResolvingToRemovedRelayPlugin(t *testing.T) {
	h := removedRelayHome(t, "relay-copy", "")
	var out bytes.Buffer
	err := CmdEnable(homeCtx(h), "relay-copy", nil, &out)
	var exit *CommandExit
	if !errors.As(err, &exit) || exit.Code != 1 {
		t.Fatalf("err %v output %q", err, out.String())
	}
	if _, statErr := os.Stat(filepath.Join(h, "config.yaml")); !os.IsNotExist(statErr) {
		t.Fatalf("config saved %v", statErr)
	}
	if text := out.String(); !strings.Contains(text, "observability/nemo_relay") || !strings.Contains(text, tools.RelayPluginsConfigEnv) {
		t.Fatalf("output %q", text)
	}
}

// Discovery records an enabled removed Relay identity as a disabled gated record without loading it
func TestRemovedRelayPluginIdentityCannotBeReloaded(t *testing.T) {
	h := removedRelayHome(t, "nemo_relay", "observability/nemo_relay")
	m := NewManager(h)
	m.DiscoverAndLoad(homeCtx(h), false)
	m.mu.RLock()
	state := m.plugins["observability/nemo_relay"]
	host := m.pluginHostInstance
	m.mu.RUnlock()
	if state == nil || state.Enabled || !strings.Contains(state.Error, tools.RelayPluginsConfigEnv) {
		t.Fatalf("state %+v", state)
	}
	if host != nil {
		t.Fatal("removed Relay plugin reached the loader")
	}
}
