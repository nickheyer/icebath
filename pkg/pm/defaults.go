package pm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

// DeclinedFilename records the default packages a user opted out of
const DeclinedFilename = "declined-packages.json"

// RetiredDefaults carries an old default's opt out over to the package that replaced it
var RetiredDefaults = map[string]string{"agent-browser": "chromium"}

// Current name of a default package, a retired name mapping to its successor
func CanonicalDefaultName(name string) string {
	if successor, ok := RetiredDefaults[name]; ok {
		return successor
	}
	return name
}

// Opt out record of an install, the binary's own tree when projectRoot is empty
func DeclinedPath(ctx context.Context, projectRoot string) string {
	if projectRoot == "" {
		projectRoot = RepoRoot()
	}
	return filepath.Join(InstallStateDir(ctx, projectRoot), DeclinedFilename)
}

// Declined default packages, a damaged record refusing rather than re adding a refused package
func Declined(ctx context.Context, projectRoot string) (map[string]bool, error) {
	path := DeclinedPath(ctx, projectRoot)
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return map[string]bool{}, nil
	}
	var doc any
	if err == nil {
		doc, err = loadJSON(data)
	}
	if err != nil {
		return nil, NewInstallError("defaults", fmt.Sprintf("cannot read %s: %v", path, err), "fix or delete the file, then retry")
	}
	out := map[string]bool{}
	m, _ := doc.(map[string]any)
	names, err := pyIterate(m["declined"])
	if err != nil {
		return nil, err
	}
	for _, name := range names {
		if s, ok := name.(string); ok {
			out[CanonicalDefaultName(s)] = true
		}
	}
	return out, nil
}

// Items Python iteration yields over a falsy-or-iterable JSON value, characters of a string and keys of an object
func pyIterate(v any) ([]any, error) {
	if !pyrepr.Truthy(v) {
		return nil, nil
	}
	switch t := v.(type) {
	case []any:
		return t, nil
	case string:
		out := []any{}
		for _, r := range t {
			out = append(out, string(r))
		}
		return out, nil
	case map[string]any:
		out := []any{}
		for _, k := range sortedKeys(t) {
			out = append(out, k)
		}
		return out, nil
	}
	return nil, fmt.Errorf("'%s' object is not iterable", pyrepr.TypeName(v))
}

// Updates the recorded opt outs, writing only on change, and returns the new set
func RecordDeclined(ctx context.Context, add, remove []string, projectRoot string) (map[string]bool, error) {
	current, err := Declined(ctx, projectRoot)
	if err != nil {
		return nil, err
	}
	updated := map[string]bool{}
	for name := range current {
		updated[name] = true
	}
	for _, name := range add {
		updated[CanonicalDefaultName(name)] = true
	}
	for _, name := range remove {
		delete(updated, name)
	}
	if !sameSet(updated, current) {
		names := make([]string, 0, len(updated))
		for name := range updated {
			names = append(names, name)
		}
		sort.Strings(names)
		var buf bytes.Buffer
		enc := json.NewEncoder(&buf)
		enc.SetEscapeHTML(false)
		enc.SetIndent("", "  ")
		if err := enc.Encode(struct {
			Schema   int      `json:"schema"`
			Declined []string `json:"declined"`
		}{1, names}); err != nil {
			return nil, err
		}
		if err := DurableWriteBytes(DeclinedPath(ctx, projectRoot), asciiEscape(buf.Bytes())); err != nil {
			return nil, err
		}
	}
	return updated, nil
}

// Every package that may be declined, the default ones of any target
func DefaultPackageNames() []string {
	var out []string
	for _, name := range AllPackages() {
		p, _ := GetPackage(name)
		if p.Base().Default && !p.Base().Internal {
			out = append(out, name)
		}
	}
	return out
}

// Optional defaults among the lockfile's names this install carries, skipping gaps and declined ones
func DefaultPackages(ctx context.Context, names []string, target string, declinedNames map[string]bool) ([]string, error) {
	if target == "" {
		t, err := CurrentTarget()
		if err != nil {
			return nil, err
		}
		target = t
	}
	refused := declinedNames
	if refused == nil {
		d, err := Declined(ctx, "")
		if err != nil {
			return nil, err
		}
		refused = d
	}
	var selected []string
	for _, name := range names {
		p, err := GetPackage(name)
		if err != nil {
			continue
		}
		base := p.Base()
		if base.Default && !base.Internal && !refused[name] && p.MissingReason(target) == "" {
			selected = append(selected, name)
		}
	}
	return selected, nil
}

// Reports whether two name sets hold the same names
func sameSet(a, b map[string]bool) bool {
	if len(a) != len(b) {
		return false
	}
	for k := range a {
		if !b[k] {
			return false
		}
	}
	return true
}
