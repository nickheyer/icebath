package terminal

import (
	"context"
	"encoding/json"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"time"

	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/pkg/fsx"
)

// Longest a change to a snapshot store waits for its lock
const snapshotsLock = 10 * time.Second

// Ids of the filesystem snapshots a cloud backend took of its sandboxes, by the key of the environment each was taken for, kept in a
// JSON file of the profile folder so a later session of the same key starts from it
type Snapshots struct {
	path string
}

// Store of the profile in the file of the name
func NewSnapshots(cfg *config.Config, name string) Snapshots {
	return Snapshots{path: filepath.Join(cfg.ProfileDir(cfg.Profile()), name)}
}

// Snapshot id kept for the key, empty when there is none
func (s Snapshots) Get(key string) string {
	return s.read()[key]
}

// Keeps the snapshot id for the key
func (s Snapshots) Set(key, id string) error {
	return s.update(func(ids map[string]string) { ids[key] = id })
}

// Forgets the snapshot of the key while it is still the one of the id, whichever it is when the id is empty
func (s Snapshots) Delete(key, id string) error {
	return s.update(func(ids map[string]string) {
		if current, ok := ids[key]; ok && (id == "" || current == id) {
			delete(ids, key)
		}
	})
}

// Ids of the file, none when it can't be read
func (s Snapshots) read() map[string]string {
	ids := map[string]string{}
	if data, err := os.ReadFile(s.path); err == nil {
		var stored map[string]any
		if json.Unmarshal(data, &stored) == nil {
			for key, value := range stored {
				if id, ok := value.(string); ok {
					ids[key] = id
				}
			}
		}
	}
	return ids
}

// Read, change and write of the file under its lock across processes, unlocked when the lock can't be had
func (s Snapshots) update(change func(map[string]string)) error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}
	if unlock, err := fsx.Lock(context.Background(), s.path+".lock", snapshotsLock); err != nil {
		slog.Warn("Updating snapshot store without its lock", "path", s.path, "error", err)
	} else {
		defer unlock()
	}
	ids := s.read()
	before := maps.Clone(ids)
	change(ids)
	if maps.Equal(ids, before) {
		return nil
	}
	data, err := json.MarshalIndent(ids, "", "  ")
	if err != nil {
		return err
	}
	return fsx.Write(s.path, data, 0o644)
}
