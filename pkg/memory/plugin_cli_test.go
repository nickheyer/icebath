package memory

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/home"
)

type fakeProviderCLIRun struct {
	name, dir, module, desc string
	handler                 bool
	argv                    []string
}

// Swaps the plugin host CLI seams for fakes recording every run
func fakeProviderCLIHost(t *testing.T, registerCLI, handler bool, describeErr error, rc int, dispatched bool) *[]fakeProviderCLIRun {
	runs := &[]fakeProviderCLIRun{}
	oldDescribe, oldRun := describeProviderCLI, runProviderCLI
	t.Cleanup(func() { describeProviderCLI, runProviderCLI = oldDescribe, oldRun })
	describeProviderCLI = func(ctx context.Context, name, dir, module string) (bool, bool, error) {
		*runs = append(*runs, fakeProviderCLIRun{name: name, dir: dir, module: module})
		return registerCLI, handler, describeErr
	}
	runProviderCLI = func(ctx context.Context, name, dir, module, desc string, withHandler bool, argv []string, stdin io.Reader, stdout, stderr io.Writer) (int, bool, error) {
		*runs = append(*runs, fakeProviderCLIRun{name: name, dir: dir, module: module, desc: desc, handler: withHandler, argv: argv})
		io.WriteString(stdout, "ran "+strings.Join(argv, " "))
		return rc, dispatched, nil
	}
	return runs
}

// Swaps the plugin host schema seam for one answering per path and counting loads
func fakeSchemaHost(t *testing.T, answer func(path string) (any, error)) map[string]int {
	calls := map[string]int{}
	old := providerConfigSchema
	t.Cleanup(func() { providerConfigSchema = old })
	providerConfigSchema = func(ctx context.Context, path string) (any, error) {
		calls[path]++
		return answer(path)
	}
	return calls
}

func TestUserProviderConfigSchemaRunsInThePluginHost(t *testing.T) {
	isolateRegistry(t)
	root := t.TempDir()
	dirs := map[string]string{}
	for _, label := range []string{"A", "B"} {
		h := filepath.Join(root, label)
		dirs[label] = discProvider(t, filepath.Join(h, "plugins"), "custom")
		discWrite(t, filepath.Join(dirs[label], "config_schema.py"), "CONFIG_SCHEMA = None\n")
	}
	calls := fakeSchemaHost(t, func(path string) (any, error) {
		label := filepath.Base(filepath.Dir(filepath.Dir(filepath.Dir(path))))
		return map[string]any{"name": "custom", "label": label, "fields": []any{map[string]any{"key": "api_key", "label": "Key", "kind": "secret"}}}, nil
	})
	t.Setenv("PYTHONPATH", "")
	for _, label := range []string{"A", "B", "A"} {
		ctx := home.WithOverride(context.Background(), filepath.Join(root, label))
		s := GetProviderConfigSchema(ctx, nil, "custom")
		if s == nil || s.Label != label || len(s.Fields) != 1 || s.Fields[0].Key != "api_key" {
			t.Fatalf("profile %s schema %+v", label, s)
		}
	}
	pathA := filepath.Join(dirs["A"], "config_schema.py")
	if calls[pathA] != 1 || calls[filepath.Join(dirs["B"], "config_schema.py")] != 1 {
		t.Fatalf("schema loads %v", calls)
	}
	if err := os.Remove(pathA); err != nil {
		t.Fatal(err)
	}
	if s := GetProviderConfigSchema(home.WithOverride(context.Background(), filepath.Join(root, "A")), nil, "custom"); s != nil {
		t.Fatalf("schema served after its file went away %+v", s)
	}
}
