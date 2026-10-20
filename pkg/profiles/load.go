package profiles

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
)

// Reads profiles from JSON, a list or an object with a profiles list told apart by the opening bracket
func Load(r io.Reader) ([]Profile, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return nil, err
	}
	var list []Profile
	if trimmed := bytes.TrimSpace(data); len(trimmed) > 0 && trimmed[0] == '[' {
		if err := json.Unmarshal(trimmed, &list); err != nil {
			return nil, fmt.Errorf("profiles: %w", err)
		}
	} else {
		var doc struct {
			Profiles []Profile `json:"profiles"`
		}
		if err := json.Unmarshal(trimmed, &doc); err != nil {
			return nil, fmt.Errorf("profiles: %w", err)
		}
		list = doc.Profiles
	}
	seen := map[string]bool{}
	for _, p := range list {
		if err := p.Validate(); err != nil {
			return nil, err
		}
		if seen[p.Name] {
			return nil, fmt.Errorf("profiles: duplicate profile %s", p.Name)
		}
		seen[p.Name] = true
	}
	return list, nil
}

func LoadFile(path string) ([]Profile, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return Load(f)
}

// Lays overrides over base by name, returning the union sorted by name
func Merge(base, overrides []Profile) []Profile {
	byName := map[string]Profile{}
	for _, p := range base {
		byName[p.Name] = p
	}
	for _, p := range overrides {
		byName[p.Name] = p
	}
	out := make([]Profile, 0, len(byName))
	for _, p := range byName {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Finds a profile by name among the built in ones and any extra lists, later lists winning
func Find(name string, extra ...[]Profile) (Profile, bool) {
	found, ok := Lookup(name)
	for _, list := range extra {
		for _, p := range list {
			if p.Name == name {
				found, ok = p, true
			}
		}
	}
	return found, ok
}
