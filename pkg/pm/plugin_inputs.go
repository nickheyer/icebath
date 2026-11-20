//go:build !icebath_core && !icebath_noextras

package pm

import (
	"fmt"

	"github.com/nickheyer/icebath/pkg/errchain"
)

// MemberDir is one plugin member, the installed identity it answers to and the tree that supplies it
type MemberDir struct {
	Identity string
	Source   string
}

// Members builds exactly these members and skips config discovery, Mapped when identities differ from sources
type Members struct {
	Dirs   []MemberDir
	Mapped bool
}

func (*Members) pluginInput() {}

// Member dirs of plain paths, each its own identity
func MemberPaths(paths []string) []MemberDir {
	out := make([]MemberDir, len(paths))
	for i, p := range paths {
		out[i] = MemberDir{Identity: p, Source: p}
	}
	return out
}

// JSON form of a plugin input for the worker request, paths made absolute
func EncodePluginInput(p PluginInput) map[string]any {
	switch v := p.(type) {
	case nil:
		return nil
	case *Members:
		if v.Mapped {
			sources := make([]any, len(v.Dirs))
			for i, d := range v.Dirs {
				sources[i] = []any{absoluteUnresolved(d.Identity), absoluteUnresolved(d.Source)}
			}
			return map[string]any{"kind": "members", "sources": sources}
		}
		paths := make([]any, len(v.Dirs))
		for i, d := range v.Dirs {
			paths[i] = absoluteUnresolved(d.Source)
		}
		return map[string]any{"kind": "members", "paths": paths}
	case *Candidates:
		paths := make([]any, len(v.Dirs))
		for i, d := range v.Dirs {
			paths[i] = absoluteUnresolved(d)
		}
		return map[string]any{"kind": "candidates", "paths": paths}
	case *Selection:
		return map[string]any{"kind": "selection", "data": copyJSONMap(v.Data)}
	case *StagedUpdate:
		return map[string]any{"kind": "staged", "data": copyJSONMap(v.Data)}
	}
	return nil
}

// Required key of an encoded plugin input, the KeyError Python raises when it is missing
func pluginInputField(value map[string]any, key string) (any, error) {
	v, ok := value[key]
	if !ok {
		return nil, &PyError{Class: "KeyError", Message: "the plugin input has no " + key}
	}
	return v, nil
}

// String items of an encoded path list
func pluginInputPaths(value map[string]any, key string) ([]string, error) {
	raw, err := pluginInputField(value, key)
	if err != nil {
		return nil, err
	}
	return pyIterStrings(raw)
}

// Plugin input of its JSON form, nil for null and a ValueError for an unknown kind
func DecodePluginInput(value map[string]any) (PluginInput, error) {
	if value == nil {
		return nil, nil
	}
	rawKind, err := pluginInputField(value, "kind")
	if err != nil {
		return nil, err
	}
	kind, _ := rawKind.(string)
	switch kind {
	case "members":
		if raw, ok := value["sources"]; ok {
			list, ok := raw.([]any)
			if !ok {
				return nil, &PyError{Class: "TypeError", Message: fmt.Sprintf("the plugin input sources must be a list, got %s", errchain.Kind(raw))}
			}
			dirs := make([]MemberDir, 0, len(list))
			for _, item := range list {
				pair, err := pyIterStrings(item)
				if err != nil {
					return nil, err
				}
				if len(pair) != 2 {
					return nil, &PyError{Class: "ValueError", Message: fmt.Sprintf("a plugin input source must pair an identity with a path, got %d values", len(pair))}
				}
				dirs = append(dirs, MemberDir{Identity: pair[0], Source: pair[1]})
			}
			return &Members{Dirs: dirs, Mapped: true}, nil
		}
		paths, err := pluginInputPaths(value, "paths")
		if err != nil {
			return nil, err
		}
		return &Members{Dirs: MemberPaths(paths)}, nil
	case "candidates":
		paths, err := pluginInputPaths(value, "paths")
		if err != nil {
			return nil, err
		}
		return &Candidates{Dirs: paths}, nil
	case "selection", "staged":
		raw, err := pluginInputField(value, "data")
		if err != nil {
			return nil, err
		}
		data, ok := raw.(map[string]any)
		if !ok {
			return nil, &PyError{Class: "TypeError", Message: fmt.Sprintf("the plugin %s data must be a mapping, got %s", kind, errchain.Kind(raw))}
		}
		if kind == "selection" {
			return &Selection{Data: data}, nil
		}
		return &StagedUpdate{Data: data}, nil
	}
	return nil, &PyError{Class: "ValueError", Message: "unknown plugin input kind: " + errchain.Show(rawKind)}
}
