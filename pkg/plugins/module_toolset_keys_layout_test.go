package plugins

import (
	"context"
	"os"
	"testing"
)

func TestPersistPluginToolsetKeysWritesPythonDumpLayout(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	ctx := context.Background()
	persistPluginToolsetKeys(ctx)
	raw, err := os.ReadFile(pluginToolsetKeysCachePath(ctx))
	if err != nil {
		t.Fatal(err)
	}
	if got := string(raw); got != `{"toolset_keys": [], "portable_mcp": []}` {
		t.Fatalf("layout %q", got)
	}
}
