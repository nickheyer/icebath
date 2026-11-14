package tuigateway_test

import (
	"testing"

	"github.com/nickheyer/icebath/pkg/tuigateway/contractgen"
)

// Both committed contract files match a fresh render of the Go contract catalog byte for byte
func TestGeneratedFilesAreCurrent(t *testing.T) {
	files, err := contractgen.Render()
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != len(contractgen.Committed) {
		t.Fatalf("rendered %d files, digests list %d: run go generate ./pkg/tuigateway", len(files), len(contractgen.Committed))
	}
	for path, text := range files {
		if got, want := contractgen.Digest(text), contractgen.Committed[path]; got != want {
			t.Errorf("stale generated contract file %s (render %s, committed %s): run go generate ./pkg/tuigateway", path, got, want)
		}
	}
}
