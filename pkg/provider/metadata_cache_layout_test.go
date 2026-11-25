package provider

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestTTLMemoPutKeepsFileOrderAndCompactLayout(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	ctx := context.Background()
	saved := wallNow
	wallNow = func() time.Time { return time.Unix(1700000000, 500000000) }
	t.Cleanup(func() { wallNow = saved })
	path := localProbeDiskCachePath(ctx)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	seed := `{"server_type:http://b":{"value":"ollama","ts":1600000000},"server_type:http://a":{"value":"vllm","ts":1700000000.25}}`
	if err := os.WriteFile(path, []byte(seed), 0o644); err != nil {
		t.Fatal(err)
	}
	localProbeDiskPut(ctx, "server_type", "http://c", "lm-studio")
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"server_type:http://a":{"value":"vllm","ts":1700000000.25},"server_type:http://c":{"value":"lm-studio","ts":1700000000.5}}`
	if string(got) != want {
		t.Fatalf("probe cache\n%s\nwant\n%s", got, want)
	}
	endpointDiskCachePut(ctx, "http://e/v1", map[string]map[string]any{"m": {"context_length": 8}})
	raw, _ := os.ReadFile(endpointMetadataCachePath(ctx))
	if want := `{"http://e/v1":{"at":1700000000.5,"models":{"m":{"context_length":8}}}}`; string(raw) != want {
		t.Fatalf("endpoint cache\n%s\nwant\n%s", raw, want)
	}
}
