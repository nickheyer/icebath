package plugins

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyversion"
	"github.com/nickheyer/icebath/pkg/pm"
)

func TestCoreRequirementErrorExport(t *testing.T) {
	ctx := context.Background()
	cases := map[string]*pm.PythonDeclaration{
		"external":    {Requirements: []string{"icebath-agent>=999"}, Manifest: map[string]any{"python_runtime": "external"}},
		"unrelated":   {Requirements: []string{"requests>=2"}, Manifest: map[string]any{}},
		"satisfied":   {Requirements: []string{"icebath-agent>=0"}, Manifest: map[string]any{}},
		"unversioned": {Requirements: []string{"icebath-agent"}, Manifest: map[string]any{}},
	}
	for name, declaration := range cases {
		reason, err := pm.CoreRequirementError(ctx, declaration)
		if err != nil || reason != "" {
			t.Errorf("%s: reason %q err %v, want none", name, reason, err)
		}
	}
	reason, err := pm.CoreRequirementError(ctx, &pm.PythonDeclaration{Requirements: []string{"icebath-agent>=999"}, Manifest: map[string]any{}})
	if err != nil {
		t.Fatal(err)
	}
	want := ""
	if _, perr := pyversion.Parse(RunningVersion); perr == nil {
		want = "requires icebath-agent>=999, running " + RunningVersion
	}
	if reason != want {
		t.Errorf("newer core pin: %q, want %q", reason, want)
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "plugin.yaml"), []byte("name: p\npython_dependencies:\n  - icebath-agent>=999\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := coreRequirementError(dir); got != want {
		t.Errorf("loader verdict %q, want %q", got, want)
	}
	if got := coreRequirementError(filepath.Join(dir, "missing")); got != "" {
		t.Errorf("unreadable declaration answers empty, got %q", got)
	}
}
