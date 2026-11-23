package settings

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sync"

	"github.com/nickheyer/icebath/pkg/config"
)

// File in the home holding runtime state settings, the ones the app writes for itself and an install may never lock
const stateFile = "settings_state.json"

// Path of a home's state settings file
func statePath(h string) string { return filepath.Join(h, stateFile) }

// Stat signature of a home's state file, empty when absent
func stateSignature(h string) []any {
	fi, err := os.Stat(statePath(h))
	if err != nil {
		return nil
	}
	return []any{fi.ModTime().UnixNano(), fi.Size()}
}

// Parsed state file of a home, empty when absent; an unreadable or broken file is an error
func readState(h string) (map[string]any, error) {
	data, err := os.ReadFile(statePath(h))
	if errors.Is(err, fs.ErrNotExist) {
		return map[string]any{}, nil
	}
	if err != nil {
		return nil, err
	}
	var doc map[string]any
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, err
	}
	if doc == nil {
		doc = map[string]any{}
	}
	return doc, nil
}

// Serialises state file writes of this process
var stateMu sync.Mutex

// Sets or removes one key of a home's state file in place
func writeState(ctx context.Context, h, key string, value any, reset bool) error {
	stateMu.Lock()
	defer stateMu.Unlock()
	doc, err := readState(h)
	if err != nil {
		return err
	}
	if reset {
		if !config.Tree(doc).Unset(key) {
			return nil
		}
	} else if err := config.Tree(doc).Set(key, value); err != nil {
		return err
	}
	return config.AtomicJSONWrite(ctx, statePath(h), doc)
}
