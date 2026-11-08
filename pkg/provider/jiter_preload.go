package provider

import (
	"sync"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

// Import failure of a named module, what the streaming parser preload records
type ModuleNotFoundError struct{ Name string }

func (e *ModuleNotFoundError) Error() string { return "No module named '" + e.Name + "'" }

var (
	jiterPreloadMu     sync.Mutex
	jiterPreloaded     bool
	jiterPreloadError  error
	jiterLoadedModules = map[string]bool{}
	jiterImportModule  = importJiterModule
)

// Loads the SDK streaming parser module by name, a probe parse proving the parser works in this process
func importJiterModule(name string) error {
	if _, err := pyjson.LoadsPartial(`{"jiter": [1, 2, {"from_json": true}]}`); err != nil {
		return err
	}
	jiterLoadedModules[name] = true
	return nil
}

// Loads the streaming parser once, false with the error recorded when a module fails to load
func PreloadJiterNativeExtension() bool {
	jiterPreloadMu.Lock()
	defer jiterPreloadMu.Unlock()
	if jiterPreloaded {
		return true
	}
	for _, name := range []string{"jiter.jiter", "jiter"} {
		if err := jiterImportModule(name); err != nil {
			jiterPreloadError = err
			return false
		}
	}
	jiterPreloaded, jiterPreloadError = true, nil
	return true
}

func init() { PreloadJiterNativeExtension() }
