package plugins

import (
	"context"
	"os"
	"path/filepath"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

const updateCheckMarkersDir = "plugin-update-checks"

func updateCheckMarkerPath(h string) string {
	return filepath.Join(h, updateCheckMarkersDir, "last-run")
}

// Markers dir created on demand for write paths
func updateCheckMarkersDirOf(h string) (string, error) {
	d := filepath.Dir(updateCheckMarkerPath(h))
	return d, os.MkdirAll(d, 0o755)
}

// Install records an update check compares against, read before the check runs
func snapshotInstalls(pluginsDir string) map[string]any { return ReadSidecarRows(pluginsDir) }

// Persists a finished check with the install record each row was checked against, never failing
func recordCheckResults(ctx context.Context, results []*CheckResult, checked map[string]any) {
	rows := make([]any, 0, len(results))
	for _, r := range results {
		row := checkResultObject(r)
		row.Set("checked_install", installFingerprint(checked[r.Name]))
		rows = append(rows, row)
	}
	dir, err := updateCheckMarkersDirOf(home.Dir(ctx))
	if err != nil {
		return
	}
	text, err := pyjson.Dumps(rows, pyjson.Indent(2))
	if err != nil {
		return
	}
	_ = atomicWrite(filepath.Join(dir, "results.json"), []byte(text+"\n"), 0o644)
}

// Source, revision and pinned parts of an install record, nil for a non mapping
func installFingerprint(record any) any {
	m, ok := record.(map[string]any)
	if !ok {
		return nil
	}
	o := pyjson.NewObject()
	o.Set("source", m["source"])
	o.Set("revision", m["revision"])
	o.Set("pinned", m["pinned"] == true)
	return o
}
