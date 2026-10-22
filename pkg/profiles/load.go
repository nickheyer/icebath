package profiles

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/nickheyer/icebath/pkg/protocol"
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

// Reads a list, an object with a profiles list and default, or the earlier live and name keyed map form
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
			Default  string          `json:"default"`
			Live     string          `json:"live"`
			Profiles json.RawMessage `json:"profiles"`
		}
		if err := json.Unmarshal(trimmed, &doc); err != nil {
			return Document{}, fmt.Errorf("profiles: %w", err)
		}
		out.Default = doc.Default
		if out.Default == "" {
			out.Default = doc.Live
		}
		switch body := bytes.TrimSpace(doc.Profiles); {
		case len(body) == 0 || bytes.Equal(body, []byte("null")):
		case body[0] == '{':
			if out.Profiles, err = earlierProfiles(body); err != nil {
				return Document{}, err
			}
		default:
			if err := json.Unmarshal(body, &out.Profiles); err != nil {
				return Document{}, fmt.Errorf("profiles: %w", err)
			}
		}
	}
	if err := out.Validate(); err != nil {
		return Document{}, err
	}
	return out, nil
}

// Profiles from JSON in any shape Read accepts, templates applied
func Load(r io.Reader) ([]Profile, error) {
	f, err := Read(r)
	if err != nil {
		return nil, err
	}
	return f.Expanded()
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

func LoadFile(path string) ([]Profile, error) {
	f, err := ReadFile(path)
	if err != nil {
		return nil, err
	}
	return f.Expanded()
}

// Vendor every entry of the earlier file shape spoke to, an OpenAI compatible chat completions endpoint
const earlierVendor = "openai"

// Entry shape the earlier icebath wrote under each profile name
type earlierEntry struct {
	APIKey  string `json:"API_KEY"`
	BaseURL string `json:"BASE_URL"`
	Model   string `json:"MODEL"`
}

// Converts the earlier name keyed map into profiles sorted by name, ignoring fields the shape never had
func earlierProfiles(raw []byte) ([]Profile, error) {
	var entries map[string]json.RawMessage
	if err := json.Unmarshal(raw, &entries); err != nil {
		return nil, fmt.Errorf("profiles: %w", err)
	}
	names := make([]string, 0, len(entries))
	for name := range entries {
		names = append(names, name)
	}
	sort.Strings(names)
	out := make([]Profile, 0, len(names))
	for _, name := range names {
		var e earlierEntry
		if err := json.Unmarshal(entries[name], &e); err != nil {
			return nil, fmt.Errorf("profiles: %s: %w", name, err)
		}
		out = append(out, e.profile(name))
	}
	return out, nil
}

// Chat completions profile carrying the key as a static bearer credential and the model as the default
func (e earlierEntry) profile(name string) Profile {
	p := Profile{Name: name, Vendor: earlierVendor, Protocol: protocol.OpenAICompletions, BaseURL: earlierBaseURL(e.BaseURL), Auth: AuthNone}
	if e.APIKey != "" {
		p.Auth = AuthBearer
		p.Key = Static(e.APIKey)
	}
	if e.Model != "" {
		p.Models = []string{e.Model}
	}
	return p
}

// Appends the /v1 path chat completions expects when the earlier bare host form left it off
func earlierBaseURL(raw string) string {
	raw = strings.TrimRight(strings.TrimSpace(raw), "/")
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil || u.Path != "" {
		return raw
	}
	return raw + "/v1"
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
