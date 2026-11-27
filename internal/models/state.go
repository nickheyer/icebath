package models

import (
	"bytes"
	"encoding/json"
	"os"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/fsx"
)

// What one profile's caches hold in memory, beside the files they keep in its folder
type state struct {
	dir string
	dev registry

	mu sync.Mutex
	// Models listings of endpoints by address and key, with when they were read
	endpoints map[endpointKey]endpointRows
	// Kind of server found at the root of a local address, and when
	servers map[string]verdict
	// Hosts a probe timed out connecting to, and when
	blackholed map[string]time.Time
	// Context windows local servers reported, and when
	probes map[probeKey]probeHit
	// Models and endpoints already warned of as falling back to the default window
	defaulted map[string]bool
	// Windows found for routes, and when
	windows map[windowKey]windowHit
	// Account usage readings and their refreshes, by slot
	usage map[usageKey]*usageSlot
	// Online catalog as last read, from its file and when that was written, and the addresses failing to answer until when
	manifest      map[string]any
	manifestAt    time.Time
	manifestRetry map[string]time.Time
	// Refreshes running in the background, by what they refresh
	refreshing map[string]bool

	// Writes of the context window cache and of the model list cache, one at a time in this process
	contexts sync.Mutex
	catalogs sync.Mutex
}

func newState(dir string) *state {
	return &state{dir: dir, dev: registry{warned: map[string]bool{}}, endpoints: map[endpointKey]endpointRows{}, servers: map[string]verdict{},
		blackholed: map[string]time.Time{}, probes: map[probeKey]probeHit{}, defaulted: map[string]bool{}, windows: map[windowKey]windowHit{},
		usage: map[usageKey]*usageSlot{}, manifestRetry: map[string]time.Time{}, refreshing: map[string]bool{}}
}

// Starts the refresh named unless one runs, freeing the name once it returns
func (s *state) background(name string, refresh func()) {
	s.mu.Lock()
	if s.refreshing[name] {
		s.mu.Unlock()
		return
	}
	s.refreshing[name] = true
	s.mu.Unlock()
	go func() {
		defer func() {
			s.mu.Lock()
			delete(s.refreshing, name)
			s.mu.Unlock()
		}()
		refresh()
	}()
}

func writeJSON(path string, data any) error {
	raw, err := json.Marshal(data)
	if err != nil {
		return err
	}
	return fsx.Write(path, raw, 0o600)
}

// JSON object a file holds, nil when it is missing or holds something else
func readJSON(path string) map[string]any {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var data map[string]any
	if json.Unmarshal(bytes.TrimPrefix(raw, []byte("\uFEFF")), &data) != nil {
		return nil
	}
	return data
}
