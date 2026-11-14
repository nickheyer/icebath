package pm

import (
	"bytes"
	"context"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestUpdateInstallsTheExtrasConfiguredFeaturesNeed(t *testing.T) {
	tmp := isolateHome(t)
	root := filepath.Join(tmp, "project")
	peWrite(t, filepath.Join(root, "pyproject.toml"), "[project]\nname = \"x\"\nversion = \"0\"\n"+
		"[project.optional-dependencies]\ndiscord = []\ngoogle-chat = []\nmcp = []\n")
	peSwap(t, &ConfiguredFeaturesMissingDeps, func(context.Context) []ConfiguredFeatureGap {
		return []ConfiguredFeatureGap{{"Discord", "discord hint", "discord"}, {"Google Chat", "chat hint", "google_chat"},
			{"IRC", "irc hint", "irc"}, {"MCP servers", "mcp hint", "mcp"}}
	})
	var calls []SyncVenvOptions
	peSwap(t, &clientSyncVenv, func(_ context.Context, opts SyncVenvOptions) error {
		calls = append(calls, opts)
		return nil
	})
	var out bytes.Buffer
	if err := InstallConfiguredFeaturesMissingDeps(withSyncOutput(context.Background(), &out), root); err != nil {
		t.Fatal(err)
	}
	extras := []string{"discord", "google-chat", "mcp"}
	want := []SyncVenvOptions{{Extras: extras, Explicit: true, ProjectRoot: root, EvictIncompatiblePlugins: true, Automatic: extras}}
	if !reflect.DeepEqual(calls, want) {
		t.Fatalf("calls %+v", calls)
	}
	text := out.String()
	if !strings.Contains(text, "IRC: irc hint") || strings.Contains(text, "Discord") || strings.Contains(text, "MCP servers") {
		t.Fatalf("output %q", text)
	}
}
