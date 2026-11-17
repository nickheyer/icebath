package install

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

// UTC ISO 8601 with milliseconds, the completedAt format every receipt writer shares
func ISO8601UTC(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05.000Z") }

// Writes the receipt through a synced temp sibling renamed over any prior one
func writeMarker(installRoot string, commit, branch *string, completed time.Time) (json.RawMessage, error) {
	markerPath := BootstrapMarkerPath(installRoot)
	if err := os.MkdirAll(installRoot, 0o777); err != nil {
		return nil, fmt.Errorf("could not create bootstrap marker directory %s: %w", installRoot, err)
	}
	body := fmt.Sprintf("{\n  \"schemaVersion\": 1,\n  \"pinnedCommit\": %s,\n  \"pinnedBranch\": %s,\n  \"completedAt\": %s\n}\n",
		jsonOrNull(commit), jsonOrNull(branch), strconv.Quote(ISO8601UTC(completed)))
	tmp := filepath.Join(installRoot, BootstrapMarkerName+".tmp")
	f, err := os.Create(tmp)
	if err != nil {
		return nil, fmt.Errorf("could not create temp bootstrap marker %s: %w", tmp, err)
	}
	if _, err := f.WriteString(body); err != nil {
		f.Close()
		os.Remove(tmp)
		return nil, fmt.Errorf("could not write temp bootstrap marker %s: %w", tmp, err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(tmp)
		return nil, fmt.Errorf("could not flush temp bootstrap marker %s: %w", tmp, err)
	}
	f.Close()
	if _, err := os.Stat(markerPath); err == nil {
		if err := os.Remove(markerPath); err != nil {
			os.Remove(tmp)
			return nil, fmt.Errorf("could not replace existing bootstrap marker %s: %w", markerPath, err)
		}
	}
	if err := os.Rename(tmp, markerPath); err != nil {
		os.Remove(tmp)
		return nil, fmt.Errorf("could not publish bootstrap marker %s -> %s: %w", tmp, markerPath, err)
	}
	return json.RawMessage(body), nil
}

func jsonOrNull(value *string) string {
	if value == nil {
		return "null"
	}
	data, _ := json.Marshal(*value)
	return string(data)
}
