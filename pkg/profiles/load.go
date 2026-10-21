package profiles

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"sort"
	"strings"

	"github.com/nickheyer/icebath/pkg/protocol"
)

// Profile list with the name of the profile used when the configuration names none
type Document struct {
	Default  string
	Profiles []Profile
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
	seen := map[string]bool{}
	for _, p := range out.Profiles {
		if err := p.Validate(); err != nil {
			return Document{}, err
		}
		if seen[p.Name] {
			return Document{}, fmt.Errorf("profiles: duplicate profile %s", p.Name)
		}
		seen[p.Name] = true
	}
	return out, nil
}

// Profiles from JSON in any shape Read accepts
func Load(r io.Reader) ([]Profile, error) {
	f, err := Read(r)
	return f.Profiles, err
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
	return f.Profiles, err
}

// Vendor every entry of the earlier file shape spoke to, an OpenAI compatible chat completions endpoint
const earlierVendor = "openai"

// Entry shape the earlier icebath wrote under each profile name
type earlierEntry struct {
	APIKey  string `json:"API_KEY"`
	BaseURL string `json:"BASE_URL"`
	Model   string `json:"MODEL"`
}

// Converts the earlier name keyed map into profiles sorted by name, rejecting fields the shape never had
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
		dec := json.NewDecoder(bytes.NewReader(entries[name]))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&e); err != nil {
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
