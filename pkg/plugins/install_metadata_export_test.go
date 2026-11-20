package plugins

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestReadInstallMetadataExportReadsTheProfileRecord(t *testing.T) {
	h := t.TempDir()
	doc, err := ReadInstallMetadata(h)
	if err != nil || doc == nil || len(doc) != 0 {
		t.Fatalf("missing metadata gave %v, %v", doc, err)
	}
	dir := filepath.Join(h, "plugins")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, ".install-metadata.json")
	if err := os.WriteFile(path, []byte(`{"demo": {"source": "https://example.com/demo.git", "ref": "v1"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	doc, err = ReadInstallMetadata(h)
	if err != nil {
		t.Fatal(err)
	}
	entry, ok := doc["demo"].(map[string]any)
	if !ok || entry["source"] != "https://example.com/demo.git" || entry["ref"] != "v1" {
		t.Fatalf("metadata %v", doc)
	}
	if err := os.WriteFile(path, []byte(`["demo"]`), 0o644); err != nil {
		t.Fatal(err)
	}
	var opErr *PluginOperationError
	if _, err = ReadInstallMetadata(h); !errors.As(err, &opErr) || opErr.Msg != "Plugin install metadata must be a JSON object." {
		t.Fatalf("non object metadata gave %v", err)
	}
}
