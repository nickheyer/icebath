package profiles

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// Profile list as written, with the name of the profile used when the configuration names none
type Document struct {
	Default  string
	Profiles []Profile
}

// Profiles with their templates applied, ready to open
func (d Document) Expanded() ([]Profile, error) {
	var out []Profile
	for _, p := range d.Profiles {
		e, err := p.Expand()
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, nil
}

// Index of the profile with the name, -1 when there is none
func (d Document) Index(name string) int {
	for i, p := range d.Profiles {
		if p.Name == name {
			return i
		}
	}
	return -1
}

// Checks every profile and that no two share a name
func (d Document) Validate() error {
	seen := map[string]bool{}
	for _, p := range d.Profiles {
		e, err := p.Expand()
		if err != nil {
			return err
		}
		if err := e.Validate(); err != nil {
			return err
		}
		if seen[p.Name] {
			return fmt.Errorf("profiles: duplicate profile %s", p.Name)
		}
		seen[p.Name] = true
	}
	return nil
}

// Replaces the profile named old, or appends p when old is empty, keeping its place in the order
func (d *Document) Put(old string, p Profile) error {
	if i := d.Index(p.Name); i >= 0 && p.Name != old {
		return fmt.Errorf("profiles: a profile named %s already exists", p.Name)
	}
	if old == "" {
		d.Profiles = append(d.Profiles, p)
		return nil
	}
	i := d.Index(old)
	if i < 0 {
		return fmt.Errorf("profiles: no profile %s", old)
	}
	d.Profiles[i] = p
	if d.Default == old {
		d.Default = p.Name
	}
	return nil
}

// Drops the profile with the name and the default when it pointed there
func (d *Document) Remove(name string) error {
	i := d.Index(name)
	if i < 0 {
		return fmt.Errorf("profiles: no profile %s", name)
	}
	d.Profiles = append(d.Profiles[:i], d.Profiles[i+1:]...)
	if d.Default == name {
		d.Default = ""
	}
	return nil
}

// Moves the profile delta places through the order, held at either end
func (d *Document) Move(name string, delta int) error {
	i := d.Index(name)
	if i < 0 {
		return fmt.Errorf("profiles: no profile %s", name)
	}
	j := min(max(i+delta, 0), len(d.Profiles)-1)
	p := d.Profiles[i]
	d.Profiles = append(d.Profiles[:i], d.Profiles[i+1:]...)
	d.Profiles = append(d.Profiles[:j], append([]Profile{p}, d.Profiles[j:]...)...)
	return nil
}

// Saves the document as an object with the default and the profile list, readable by the owner alone
func WriteFile(path string, d Document) error {
	if err := d.Validate(); err != nil {
		return err
	}
	data, err := json.MarshalIndent(struct {
		Default  string    `json:"default,omitempty"`
		Profiles []Profile `json:"profiles"`
	}{d.Default, append([]Profile{}, d.Profiles...)}, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0o600)
}

// Reads a list of profiles, or an object holding the list under profiles beside the default name
func Read(r io.Reader) (Document, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return Document{}, err
	}
	var out Document
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) > 0 && trimmed[0] == '[' {
		if err := json.Unmarshal(trimmed, &out.Profiles); err != nil {
			return Document{}, fmt.Errorf("profiles: %w", err)
		}
	} else {
		var doc struct {
			Default  string    `json:"default"`
			Profiles []Profile `json:"profiles"`
		}
		if err := json.Unmarshal(trimmed, &doc); err != nil {
			return Document{}, fmt.Errorf("profiles: %w", err)
		}
		out.Default, out.Profiles = doc.Default, doc.Profiles
	}
	if err := out.Validate(); err != nil {
		return Document{}, err
	}
	return out, nil
}

// Reads a profile file, naming the path in every error but a missing file
func ReadFile(path string) (Document, error) {
	f, err := os.Open(path)
	if err != nil {
		return Document{}, err
	}
	defer f.Close()
	out, err := Read(f)
	if err != nil {
		return Document{}, fmt.Errorf("%s: %w", path, err)
	}
	return out, nil
}
