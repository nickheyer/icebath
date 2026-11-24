package tuiserver

import (
	"os"
	"path/filepath"
	"strings"
	"unicode"

	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
)

// Index file of a session's spawn tree snapshots
const spawnTreeIndex = "_index.jsonl"

// Spawn tree snapshot root under the home, created on first use
func spawnTreesRoot() string {
	root := filepath.Join(launchHome(), "spawn-trees")
	panicIf(os.MkdirAll(root, 0o755))
	return root
}

// Snapshot directory of a session, its id sanitized to alphanumerics, dash and underscore
func spawnTreeSessionDir(sessionID string) string {
	var name strings.Builder
	for _, c := range sessionID {
		if unicode.IsLetter(c) || unicode.IsDigit(c) || c == '-' || c == '_' {
			name.WriteRune(c)
		} else {
			name.WriteRune('_')
		}
	}
	dirName := name.String()
	if dirName == "" {
		dirName = "unknown"
	}
	d := filepath.Join(spawnTreesRoot(), dirName)
	panicIf(os.MkdirAll(d, 0o755))
	return d
}

// Appends one entry line to the session index, a failed write never blocking the save
func appendSpawnTreeIndex(sessionDir string, entry map[string]any) {
	line, err := jsonutil.Encode(entry)
	if err == nil {
		var f *os.File
		if f, err = os.OpenFile(filepath.Join(sessionDir, spawnTreeIndex), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644); err == nil {
			_, err = f.WriteString(line + "\n")
			f.Close()
		}
	}
	if err != nil {
		pkgLog().Debug("spawn_tree index append failed: " + err.Error())
	}
}

// Decoded lines of the session index, empty when it cannot be read and undecodable lines skipped
func readSpawnTreeIndex(sessionDir string) []any {
	data, err := os.ReadFile(filepath.Join(sessionDir, spawnTreeIndex))
	if err != nil {
		return []any{}
	}
	text := strings.TrimPrefix(string(data), "\ufeff")
	text = strings.ReplaceAll(strings.ReplaceAll(text, "\r\n", "\n"), "\r", "\n")
	out := []any{}
	for _, line := range strings.Split(text, "\n") {
		if line = strings.TrimSpace(line); line == "" {
			continue
		}
		if entry, err := jsonutil.Decode(line); err == nil {
			out = append(out, jsonutil.Plain(entry))
		}
	}
	return out
}
