package plugins

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/pm"
)

// Git origin of one named plugin at a pin importing plugin_proof_dep at that pin, and its HEAD sha
func pubOriginNamed(t *testing.T, root, name, pin string) (string, string) {
	t.Helper()
	repo := filepath.Join(root, name+"-origin")
	c6Git(t, root, "init", "-qb", "main", repo)
	vtWrite(t, filepath.Join(repo, "plugin.yaml"), "name: "+name+"\npython_dependencies:\n- plugin-proof-dep=="+pin+"\nversion: '"+pin+"'\n")
	vtWrite(t, filepath.Join(repo, "__init__.py"), "import plugin_proof_dep as plugin_proof_dep\nVERSION = "+pyrepr.StrRepr(pin)+
		"\ndef register(ctx):\n    return (VERSION, plugin_proof_dep.__version__)\n")
	return repo, c6Commit(t, repo, "fixture "+pin)
}

// A dependency conflict ticked from the dashboard or the composite picker is refused and nothing is written
func TestUiConflictIsReportedWithoutChangingSelection(t *testing.T) {
	for _, surface := range []string{"dashboard", "composite"} {
		t.Run(surface, func(t *testing.T) {
			root, h, core := pubDepsWorld(t)
			ctx := home.WithOverride(context.Background(), h)
			no := false
			origin, sha := pubOrigin(t, root, "1.0")
			if err := CmdInstall(ctx, InstallCommandOptions{Identifier: "file://" + filepath.ToSlash(origin), Ref: sha, Enable: &no, AllowRemoved: true}, io.Discard); err != nil {
				t.Fatal(err)
			}
			if err := CmdEnable(ctx, "plugin-worker-proof", &no, io.Discard); err != nil {
				t.Fatal(err)
			}
			origin, sha = pubOriginNamed(t, root, "conflicting-ui-plugin", "2.0")
			if err := CmdInstall(ctx, InstallCommandOptions{Identifier: "file://" + filepath.ToSlash(origin), Ref: sha, Enable: &no, AllowRemoved: true}, io.Discard); err != nil {
				t.Fatal(err)
			}
			watched := []string{filepath.Join(h, "config.yaml"), pm.RuntimeFactsPath(ctx)}
			before := map[string][]byte{}
			for _, path := range watched {
				data, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				before[path] = data
			}
			if surface == "dashboard" {
				result, err := DashboardSetAgentPluginEnabled(ctx, "conflicting-ui-plugin", true)
				if err != nil || result["ok"] != false || !strings.Contains(pyrepr.Str(result["error"]), "plugin-proof-dep") {
					t.Fatalf("dashboard result %v %v", result, err)
				}
			} else {
				_, _, err := persistPluginSelection(ctx, []string{"plugin-worker-proof", "conflicting-ui-plugin"},
					map[int]bool{0: true, 1: true}, map[int]bool{0: true}, map[string]bool{}, nil)
				var refused *AdmissionRefused
				if !errors.As(err, &refused) || !strings.Contains(err.Error(), "plugin-proof-dep") {
					t.Fatalf("composite flip %v", err)
				}
			}
			for _, path := range watched {
				if data, err := os.ReadFile(path); err != nil || !bytes.Equal(data, before[path]) {
					t.Fatalf("%s changed %v", path, err)
				}
			}
			pubImports(t, ctx, root, h, core, "1.0")
		})
	}
}
