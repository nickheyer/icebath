package vsc

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/profiles"
)

// Directory under the configuration directory where every editor window advertises its listener
const WindowsDir = "ide"

// Listener one editor window advertises for the TUIs started in its terminals
type Window struct {
	Pid        int       `json:"pid"`
	Socket     string    `json:"socket"`
	Version    string    `json:"version"`
	Workspaces []string  `json:"workspaceFolders"`
	Created    time.Time `json:"created"`
	// Folder the editor installed the extension into, beside the registry of installed extensions
	ExtensionPath string `json:"extensionPath"`
	path          string
}

// Reports whether the process runs in the integrated terminal of VS Code or one of its forks
func InsideVSCode() bool { return os.Getenv("TERM_PROGRAM") == "vscode" }

// Directory the editor windows advertise in
func windowsDir() (string, error) {
	dir, err := profiles.ConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, WindowsDir), nil
}

// Every advertised window newest first, files that do not parse or name no listener left out
func Windows() ([]Window, error) {
	dir, err := windowsDir()
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []Window
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".json" {
			continue
		}
		path := filepath.Join(dir, e.Name())
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var w Window
		if json.Unmarshal(data, &w) != nil || w.Socket == "" {
			continue
		}
		w.path = path
		out = append(out, w)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Created.After(out[j].Created) })
	return out, nil
}

// Windows ordered for a directory, the ones whose workspace holds it first and the newest ahead within each group
func ordered(windows []Window, dir string) []Window {
	var holding, rest []Window
	for _, w := range windows {
		if holds(w, dir) {
			holding = append(holding, w)
		} else {
			rest = append(rest, w)
		}
	}
	return append(holding, rest...)
}

// Reports whether one workspace folder of the window is the directory or a parent of it
func holds(w Window, dir string) bool {
	for _, ws := range w.Workspaces {
		if within(dir, ws) {
			return true
		}
	}
	return false
}

// Reports whether the path lies at or under the root
func within(path, root string) bool {
	rel, err := filepath.Rel(root, path)
	if err != nil || filepath.IsAbs(rel) {
		return false
	}
	return rel == "." || rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// Drops the advertisement of a window whose listener is gone
func (w Window) forget() {
	if w.path != "" {
		os.Remove(w.path)
	}
}

// Reports whether the editor of the window still lists the extension as installed, true when that cannot be read
func (w Window) registered(id string) bool {
	if w.ExtensionPath == "" {
		return true
	}
	data, err := os.ReadFile(filepath.Join(filepath.Dir(w.ExtensionPath), "extensions.json"))
	if err != nil {
		return true
	}
	var entries []struct {
		Identifier struct {
			ID string `json:"id"`
		} `json:"identifier"`
	}
	if json.Unmarshal(data, &entries) != nil {
		return true
	}
	for _, e := range entries {
		if strings.EqualFold(e.Identifier.ID, id) {
			return true
		}
	}
	return false
}
