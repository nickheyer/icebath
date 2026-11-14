package toolchain

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/nickheyer/icebath/internal/fileutil"
)

// Layout version of the receipt file and the token its environment values carry for the store root
const (
	factsSchema = 1
	storeToken  = "{{store}}"
	factsName   = "facts.json"
)

// Receipt of one published install
type fact struct {
	Entry     string              `json:"entry"`
	Version   string              `json:"version"`
	Env       map[string]envValue `json:"env"`
	Target    string              `json:"target,omitempty"`
	Artifacts []string            `json:"artifacts,omitempty"`
	Digest    string              `json:"digest,omitempty"`
}

// Environment value of a receipt, one string or a list of them
type envValue []string

// Reads a string or a list of strings
func (v *envValue) UnmarshalJSON(data []byte) error {
	if bytes.HasPrefix(bytes.TrimSpace(data), []byte("[")) {
		var list []string
		if err := json.Unmarshal(data, &list); err != nil {
			return err
		}
		*v = list
		return nil
	}
	var one string
	if err := json.Unmarshal(data, &one); err != nil {
		return err
	}
	*v = envValue{one}
	return nil
}

// Receipt file as stored, rows this build does not define kept as written
type factsDocument struct {
	Schema   int                        `json:"schema"`
	Packages map[string]json.RawMessage `json:"packages"`
}

// Receipts of one store root
type facts struct {
	path     string
	packages map[string]json.RawMessage
}

// Reads a receipt file, a missing one empty, an unreadable one failing only when strict
func readFacts(path string, strict bool) (map[string]json.RawMessage, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return map[string]json.RawMessage{}, nil
	}
	if err != nil {
		if strict {
			return nil, err
		}
		return map[string]json.RawMessage{}, nil
	}
	var doc factsDocument
	if json.Unmarshal(bytes.TrimPrefix(data, []byte("\xef\xbb\xbf")), &doc) == nil && doc.Schema == factsSchema && doc.Packages != nil {
		return doc.Packages, nil
	}
	if strict {
		return nil, fmt.Errorf("cannot read recorded package state: %s", path)
	}
	if strings.TrimSpace(string(data)) != "" {
		corrupt := strings.TrimSuffix(path, filepath.Ext(path)) + ".corrupt"
		if err := os.WriteFile(corrupt, data, 0o644); err != nil {
			slog.Warn(fmt.Sprintf("invalid install receipts at %s could not be kept aside: %v", path, err))
		}
	}
	return map[string]json.RawMessage{}, nil
}

// Receipts at path as they are on disk now, damage tolerated
func loadFacts(path string) *facts {
	packages, _ := readFacts(path, false)
	return &facts{path: path, packages: packages}
}

// Rereads the file, failing on one that cannot be trusted
func (f *facts) reload() error {
	packages, err := readFacts(f.path, true)
	if err != nil {
		return err
	}
	f.packages = packages
	return nil
}

// Receipt of name, false when there is none or it records no install directory
func (f *facts) get(name string) (fact, bool) {
	raw, ok := f.packages[name]
	if !ok {
		return fact{}, false
	}
	var row fact
	if json.Unmarshal(raw, &row) != nil || row.Entry == "" {
		return fact{}, false
	}
	return row, true
}

// Reports a receipt for the wanted version and identity whose install directory exists under root
func (f *facts) installed(name, version string, identity []string, target Target, root string) bool {
	row, ok := f.get(name)
	if !ok {
		return false
	}
	if version != "" && row.Version != version {
		return false
	}
	if identity != nil {
		if row.Target == "" || row.Artifacts == nil || row.Target != string(target) || !equalStrings(row.Artifacts, identity) {
			return false
		}
	}
	_, err := os.Lstat(filepath.Join(root, row.Entry))
	return err == nil
}

// Reports two string lists with the same elements in the same order
func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// Variables a receipt records for an install, each store token replaced by root
func (f *facts) env(name, root string) map[string][]string {
	row, _ := f.get(name)
	out := make(map[string][]string, len(row.Env))
	for key, values := range row.Env {
		resolved := make([]string, len(values))
		for i, v := range values {
			resolved[i] = strings.ReplaceAll(v, storeToken, root)
		}
		out[key] = resolved
	}
	return out
}

// Environment with the store root replaced by its token, the form a receipt keeps
func templatize(env map[string][]string, root string) map[string]envValue {
	out := make(map[string]envValue, len(env))
	for key, values := range env {
		row := make(envValue, len(values))
		for i, v := range values {
			row[i] = strings.ReplaceAll(v, root, storeToken)
		}
		out[key] = row
	}
	return out
}

// Writes the receipts durably as sorted indented JSON
func (f *facts) write(ctx context.Context) error {
	data, err := fileutil.EncodeJSON(factsDocument{Schema: factsSchema, Packages: f.packages}, fileutil.JSONStyle{Indent: 2, Sorted: true, ASCII: true})
	if err != nil {
		return err
	}
	data = append(bytes.TrimRight(data, "\n"), '\n')
	return fileutil.WriteFile(ctx, f.path, data, fileutil.SyncDir(), fileutil.TempPrefix(".publish-"))
}

// Merges one receipt into what is on disk and writes the result
func (f *facts) record(ctx context.Context, name string, row fact) error {
	onDisk, err := readFacts(f.path, true)
	if err != nil {
		return err
	}
	for key, value := range f.packages {
		if _, ok := onDisk[key]; !ok {
			onDisk[key] = value
		}
	}
	raw, err := json.Marshal(row)
	if err != nil {
		return err
	}
	onDisk[name] = raw
	f.packages = onDisk
	return f.write(ctx)
}

// Drops the receipts of names and writes the rest when anything changed
func (f *facts) forget(ctx context.Context, names ...string) error {
	onDisk, err := readFacts(f.path, true)
	if err != nil {
		return err
	}
	changed := false
	for _, name := range names {
		if _, ok := onDisk[name]; ok {
			delete(onDisk, name)
			changed = true
		}
	}
	f.packages = onDisk
	if !changed {
		return nil
	}
	return f.write(ctx)
}

// Install directory names every receipt still points at
func (f *facts) entriesInUse() map[string]bool {
	keep := map[string]bool{}
	for name := range f.packages {
		if row, ok := f.get(name); ok {
			keep[row.Entry] = true
		}
	}
	return keep
}

// Names with a receipt, in sorted order
func (f *facts) names() []string {
	names := make([]string, 0, len(f.packages))
	for name := range f.packages {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
