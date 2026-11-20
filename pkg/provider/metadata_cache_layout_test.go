package provider

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestTTLMemoPutKeepsFileOrderAndPythonLayout(t *testing.T) {
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
	want := "{\n\"server_type:http://a\":{\n\"value\":\"vllm\",\n\"ts\":1700000000.25\n},\n\"server_type:http://c\":{\n\"value\":\"lm-studio\",\n\"ts\":1700000000.5\n}\n}"
	if string(got) != want {
		t.Fatalf("probe cache\n%s\nwant\n%s", got, want)
	}
	endpointDiskCachePut(ctx, "http://e/v1", map[string]map[string]any{"m": {"context_length": 8}})
	raw, _ := os.ReadFile(endpointMetadataCachePath(ctx))
	if want := "{\n\"http://e/v1\":{\n\"at\":1700000000.5,\n\"models\":{\n\"m\":{\n\"context_length\":8\n}\n}\n}\n}"; string(raw) != want {
		t.Fatalf("endpoint cache\n%s\nwant\n%s", raw, want)
	}
}
