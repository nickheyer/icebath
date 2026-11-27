package packages

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"

	"github.com/nickheyer/icebath/pkg/fsx"
)

// Schema of the lock and facts files
const lockSchema = 1

// Stands for the store's folder inside recorded values, so a moved store still reads
const storeToken = "{{store}}"

// Pins in force: each declaration's own, those the user pinned since over them
type lockfile struct {
	// File of the user's pins
	path string
	// Pins in force, by package
	pins map[string]Pin
	// User pins as read, and the ones this run changed
	user    map[string]Pin
	base    map[string]Pin
	touched map[string]bool
}

// Pins in force now
func (h *Packages) lock() *lockfile {
	l := &lockfile{path: filepath.Join(h.stateDir(), "lock.json"), pins: map[string]Pin{}, touched: map[string]bool{}}
	byName, _ := h.index()
	for name, p := range byName {
		if p.Pin.Version != "" {
			l.pins[name] = p.Pin
		}
	}
	l.user = readPins(l.path)
	l.base = maps.Clone(l.user)
	for name, pin := range l.user {
		if _, declared := byName[name]; declared {
			l.pins[name] = pin
		}
	}
	return l
}

// User pins of the file, none when it is missing or damaged, a damaged one kept beside it
func readPins(path string) map[string]Pin {
	data, err := os.ReadFile(path)
	if err != nil {
		return map[string]Pin{}
	}
	var doc struct {
		Schema   int            `json:"schema"`
		Packages map[string]Pin `json:"packages"`
	}
	if json.Unmarshal(bytes.TrimPrefix(data, []byte("\xef\xbb\xbf")), &doc) != nil || doc.Schema != lockSchema {
		if len(bytes.TrimSpace(data)) > 0 {
			_ = os.WriteFile(strings.TrimSuffix(path, filepath.Ext(path))+".corrupt", data, 0o666)
		}
		return map[string]Pin{}
	}
	if doc.Packages == nil {
		doc.Packages = map[string]Pin{}
	}
	return doc.Packages
}

// Version pinned for a package, false when it has none
func (l *lockfile) version(name string) (string, bool) {
	p, ok := l.pins[name]
	return p.Version, ok && p.Version != ""
}

// Archives of a package for a target in extraction order
func (l *lockfile) artifacts(name, target string) Archives {
	return l.pins[name].For(target)
}

// Names of every pinned package, sorted
func (l *lockfile) names() []string {
	return slices.Sorted(maps.Keys(l.pins))
}

// Pins a package to a version and its archives, published by save
func (l *lockfile) set(name, version string, artifacts map[string]Archives) {
	pin := Pin{Version: version, Artifacts: maps.Clone(artifacts)}
	l.pins[name], l.user[name] = pin, pin
	l.touched[name] = true
}

// A pin another writer changed since this run read the file
type StaleError struct{ Message string }

func (e *StaleError) Error() string { return e.Message }

// Publishes the changed pins under the file's lock, refusing every pin another writer changed meanwhile
func (l *lockfile) save() error {
	release, err := fsx.Lock(context.Background(), filepath.Join(filepath.Dir(l.path), "."+filepath.Base(l.path)+".lock"), -1)
	if err != nil {
		return err
	}
	defer release()
	current := readPins(l.path)
	updated := maps.Clone(current)
	for _, name := range slices.Sorted(maps.Keys(l.touched)) {
		intended := l.user[name]
		if now, ok := current[name]; ok && !reflect.DeepEqual(now, l.base[name]) && !reflect.DeepEqual(now, intended) {
			return &StaleError{Message: fmt.Sprintf("%s changed in %s; read the lockfile again and retry", name, l.path)}
		}
		updated[name] = intended
	}
	if err := writeJSON(l.path, map[string]any{"schema": lockSchema, "packages": updated}); err != nil {
		return err
	}
	l.user, l.base = updated, maps.Clone(updated)
	for name := range l.touched {
		l.pins[name] = updated[name]
	}
	clear(l.touched)
	return nil
}

// Writes a document as indented JSON with sorted keys, durably
func writeJSON(path string, doc any) error {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(doc); err != nil {
		return err
	}
	return fsx.Write(path, buf.Bytes(), 0o600)
}

// What one install recorded of a package: its entry, version, variables, and the target and archives it came from with a digest of
// its tree
type fact struct {
	Entry     string   `json:"entry"`
	Version   string   `json:"version"`
	Env       Env      `json:"env"`
	Target    string   `json:"target,omitempty"`
	Artifacts []string `json:"artifacts,omitempty"`
	Digest    string   `json:"digest,omitempty"`
}

// Installed state of a store, written only by the package manager
type facts struct {
	path     string
	packages map[string]fact
}

// Facts of a store, missing ones reading as none; strict refuses a damaged file
func readFacts(root string, strict bool) (*facts, error) {
	f := &facts{path: filepath.Join(root, "facts.json")}
	return f, f.reload(strict)
}

func (f *facts) reload(strict bool) error {
	f.packages = map[string]fact{}
	data, err := os.ReadFile(f.path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		if strict {
			return err
		}
		return nil
	}
	var doc struct {
		Schema   int             `json:"schema"`
		Packages map[string]fact `json:"packages"`
	}
	if json.Unmarshal(bytes.TrimPrefix(data, []byte("\xef\xbb\xbf")), &doc) != nil || doc.Schema != lockSchema {
		if strict {
			return fmt.Errorf("cannot read recorded package state: %s", f.path)
		}
		if len(bytes.TrimSpace(data)) > 0 {
			_ = os.WriteFile(strings.TrimSuffix(f.path, ".json")+".corrupt", data, 0o666)
		}
		return nil
	}
	if doc.Packages != nil {
		f.packages = doc.Packages
	}
	return nil
}

func (f *facts) get(name string) (fact, bool) {
	got, ok := f.packages[name]
	return got, ok
}

// Whether a package's recorded entry is there, of the version and identity expected when they are given
func (f *facts) installed(name, root string, version *string, identity *identity) bool {
	got, ok := f.packages[name]
	if !ok || got.Entry == "" {
		return false
	}
	if version != nil && got.Version != *version {
		return false
	}
	if identity != nil && (got.Target != identity.target || !slices.Equal(got.Artifacts, identity.digests)) {
		return false
	}
	_, err := os.Stat(filepath.Join(root, got.Entry))
	return err == nil
}

// Recorded variables of a package with the store's folder put back
func (f *facts) env(name, root string) Env {
	return f.packages[name].Env.replace(storeToken, root)
}

// Records a package, read again and merged so concurrent installs of other packages are kept
func (f *facts) record(name string, got fact, root string) error {
	got.Env = got.Env.replace(root, storeToken)
	if err := f.reload(true); err != nil {
		return err
	}
	f.packages[name] = got
	return writeJSON(f.path, map[string]any{"schema": lockSchema, "packages": f.packages})
}

// Drops the facts of packages no longer declared, so gc reclaims their entries
func (f *facts) forget(keep func(name string) bool) error {
	if err := f.reload(true); err != nil {
		return err
	}
	dropped := false
	for name := range f.packages {
		if !keep(name) {
			delete(f.packages, name)
			dropped = true
		}
	}
	if !dropped {
		return nil
	}
	return writeJSON(f.path, map[string]any{"schema": lockSchema, "packages": f.packages})
}

// Entry names the facts hold
func (f *facts) entries() map[string]bool {
	out := map[string]bool{}
	for _, got := range f.packages {
		if got.Entry != "" {
			out[got.Entry] = true
		}
	}
	return out
}

// Target and archive digests a pin installs, which a recorded install must match
type identity struct {
	target  string
	digests []string
}

func (l *lockfile) identity(name, target string) *identity {
	artifacts := l.artifacts(name, target)
	if len(artifacts) == 0 {
		return nil
	}
	id := &identity{target: target}
	for _, a := range artifacts {
		id.digests = append(id.digests, a.SHA256)
	}
	return id
}
