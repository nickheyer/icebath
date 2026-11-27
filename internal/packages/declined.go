package packages

import (
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
)

// An old default's opt out carried over to the package that replaced it
var retired = map[string]string{"agent-browser": "chromium"}

// Current name of a default package
func canonicalDefault(name string) string {
	if successor, ok := retired[name]; ok {
		return successor
	}
	return name
}

// Default packages the user opted out of, a damaged record refusing rather than adding back a refused package
func (h *Packages) declined() (map[string]bool, error) {
	path := filepath.Join(h.stateDir(), "declined-packages.json")
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return map[string]bool{}, nil
	}
	var doc struct {
		Declined []string `json:"declined"`
	}
	if err == nil {
		err = json.Unmarshal(data, &doc)
	}
	if err != nil {
		return nil, Failure("Left-out programs", fmt.Sprintf("The file %s cannot be read: %v", path, err), "Fix or delete the file, then try again.")
	}
	out := map[string]bool{}
	for _, name := range doc.Declined {
		out[canonicalDefault(name)] = true
	}
	return out, nil
}

// Adds and removes opt outs, writing only on change, and returns the new set
func (h *Packages) recordDeclined(add, remove []string) (map[string]bool, error) {
	current, err := h.declined()
	if err != nil {
		return nil, err
	}
	updated := maps.Clone(current)
	for _, name := range add {
		updated[canonicalDefault(name)] = true
	}
	for _, name := range remove {
		delete(updated, name)
	}
	if maps.Equal(updated, current) {
		return updated, nil
	}
	return updated, writeJSON(filepath.Join(h.stateDir(), "declined-packages.json"), map[string]any{"schema": 1, "declined": slices.Sorted(maps.Keys(updated))})
}

// Every package the user may decline: the optional ones installed by default
func (h *Packages) defaultNames() []string {
	var out []string
	for _, name := range h.Names() {
		if p, _ := h.Package(name); p.Default && !p.Internal {
			out = append(out, name)
		}
	}
	return out
}

// Optional defaults among the names this machine carries, leaving out gaps and declined ones
func (h *Packages) defaults(names []string) ([]string, error) {
	refused, err := h.declined()
	if err != nil {
		return nil, err
	}
	target := CurrentTarget()
	var out []string
	for _, name := range names {
		if p, err := h.Package(name); err == nil && p.Default && !p.Internal && !refused[name] && p.Missing(target) == "" {
			out = append(out, name)
		}
	}
	return out, nil
}

// Required packages of the names this machine carries: neither optional nor internal, and built for it
func (h *Packages) required(names []string) ([]string, error) {
	target := CurrentTarget()
	var out []string
	for _, name := range names {
		p, err := h.Package(name)
		if err != nil {
			return nil, err
		}
		if !p.Internal && !p.Optional && p.Missing(target) == "" {
			out = append(out, name)
		}
	}
	return out, nil
}
