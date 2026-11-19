//go:build !icebath_core && !icebath_noextras

package pm

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/nickheyer/icebath/pkg/errchain"
)

// Parsed pyproject of a source tree the way tomllib load reads it
func loadPyproject(repoDir string) (*OrderedObject, error) {
	data, err := os.ReadFile(filepath.Join(repoDir, "pyproject.toml"))
	if err != nil {
		return nil, err
	}
	if err := utf8Strict(data); err != nil {
		return nil, err
	}
	return tomlLoads(string(data))
}

// Keys of the optional dependencies table of a parsed pyproject
func optionalDependencyNames(data *OrderedObject) ([]string, error) {
	project, err := tomlGet(data, "project", NewOrderedObject())
	if err != nil {
		return nil, err
	}
	table, err := tomlGet(project, "optional-dependencies", NewOrderedObject())
	if err != nil {
		return nil, err
	}
	items, err := tomlIter(table)
	if err != nil {
		return nil, err
	}
	return tomlStrings(items)
}

// Strings of decoded TOML items, a TypeError for a value Python could not sort with them
func tomlStrings(items []any) ([]string, error) {
	out := make([]string, len(items))
	for i, item := range items {
		s, ok := item.(string)
		if !ok {
			return nil, &PyError{Class: "TypeError", Message: fmt.Sprintf("expected names as text, got %s", tomlKind(item))}
		}
		out[i] = s
	}
	return out, nil
}

// Every extra the source tree's pyproject declares, sorted
func declaredExtras(repoDir string) ([]string, error) {
	data, err := loadPyproject(repoDir)
	if err != nil {
		return nil, err
	}
	names, err := optionalDependencyNames(data)
	if err != nil {
		return nil, err
	}
	sort.Strings(names)
	return names, nil
}

// Extras only an explicit selection installs, a ValueError when the opt in list names undeclared extras
func optInExtras(repoDir string) ([]string, error) {
	data, err := loadPyproject(repoDir)
	if err != nil {
		return nil, err
	}
	tool, err := tomlGet(data, "tool", NewOrderedObject())
	if err != nil {
		return nil, err
	}
	own, err := tomlGet(tool, "icebath", NewOrderedObject())
	if err != nil {
		return nil, err
	}
	raw, err := tomlGet(own, "opt-in-extras", []any{})
	if err != nil {
		return nil, err
	}
	items, err := tomlIter(raw)
	if err != nil {
		return nil, err
	}
	names, err := tomlStrings(items)
	if err != nil {
		return nil, err
	}
	declared, err := optionalDependencyNames(data)
	if err != nil {
		return nil, err
	}
	known := map[string]bool{}
	for _, name := range declared {
		known[name] = true
	}
	var unknown []string
	for _, name := range sortedSet(names) {
		if !known[name] {
			unknown = append(unknown, name)
		}
	}
	if len(unknown) > 0 {
		return nil, &PyError{Class: "ValueError", Message: "[tool.icebath] opt-in-extras names undeclared extras: " + errchain.Show(unknown)}
	}
	return sortedSet(names), nil
}
