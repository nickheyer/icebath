package connections

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/desktop/hardening"
)

var desktopProfileNameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)

// The v1 connection.json, profiles kept in the order the file lists them
type connectionConfig struct {
	Mode     string
	Remote   any
	Profiles map[string]any
	order    []string
}

func (c connectionConfig) clone() connectionConfig {
	out := connectionConfig{Mode: c.Mode, Remote: c.Remote, Profiles: make(map[string]any, len(c.Profiles)), order: append([]string{}, c.order...)}
	for k, v := range c.Profiles {
		out.Profiles[k] = v
	}
	return out
}

// Decoded JSON object the registry helpers read the config as
func (c connectionConfig) object() map[string]any {
	profiles := make(map[string]any, len(c.Profiles))
	for k, v := range c.Profiles {
		profiles[k] = v
	}
	return map[string]any{"mode": c.Mode, "remote": c.Remote, "profiles": profiles}
}

// Profile names in file order with names the order does not list after them
func (c connectionConfig) profileOrder() []string {
	return orderedKeys(c.Profiles, c.order)
}

// Writes mode, remote and profiles in that order with profiles in file order
func (c connectionConfig) MarshalJSON() ([]byte, error) {
	var b bytes.Buffer
	b.WriteString(`{"mode":`)
	if err := writeJSONValue(&b, c.Mode); err != nil {
		return nil, err
	}
	b.WriteString(`,"remote":`)
	if err := writeJSONValue(&b, c.Remote); err != nil {
		return nil, err
	}
	b.WriteString(`,"profiles":{`)
	for i, name := range c.profileOrder() {
		if i > 0 {
			b.WriteByte(',')
		}
		if err := writeJSONValue(&b, name); err != nil {
			return nil, err
		}
		b.WriteByte(':')
		if err := writeJSONValue(&b, c.Profiles[name]); err != nil {
			return nil, err
		}
	}
	b.WriteString("}}")
	return b.Bytes(), nil
}

func writeJSONValue(b *bytes.Buffer, v any) error {
	text, err := jsonMarshal(v)
	if err != nil {
		return err
	}
	b.WriteString(text)
	return nil
}

// JSON.stringify with two space indentation
func jsonStringifyIndent(v any) ([]byte, error) {
	text, err := jsonMarshal(v)
	if err != nil {
		return nil, err
	}
	var out bytes.Buffer
	if err := json.Indent(&out, []byte(text), "", "  "); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

// Keys of the object under field of the top level object, in source order
func topLevelObjectKeys(raw []byte, field string) []string {
	var keys []string
	walkObject(raw, func(name string, value json.RawMessage) {
		if name == field {
			keys = nil
			seen := map[string]bool{}
			walkObject(value, func(key string, _ json.RawMessage) {
				if !seen[key] {
					seen[key] = true
					keys = append(keys, key)
				}
			})
		}
	})
	return keys
}

// Calls visit for each member of a JSON object in source order
func walkObject(raw []byte, visit func(name string, value json.RawMessage)) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	if t, err := dec.Token(); err != nil || t != json.Delim('{') {
		return
	}
	for dec.More() {
		t, err := dec.Token()
		if err != nil {
			return
		}
		name, _ := t.(string)
		var value json.RawMessage
		if dec.Decode(&value) != nil {
			return
		}
		visit(name, value)
	}
}

// Per profile overrides cleaned to the fields routing reads, invalid names and entries dropped
func sanitizeConnectionProfiles(raw any, order []string) (map[string]any, []string) {
	out := map[string]any{}
	obj, ok := jsEntries(raw)
	if !ok {
		return out, nil
	}
	var kept []string
	for _, name := range orderedKeys(obj, order) {
		entry, ok := jsEntries(obj[name])
		if !ok {
			continue
		}
		if name != "default" && !desktopProfileNameRE.MatchString(name) {
			continue
		}
		if entry["mode"] == "ssh" {
			if ssh := NormalizeSSHConfig(entry); ssh != nil {
				cleaned := ssh.object()
				if isJSObject(entry["token"]) {
					cleaned["token"] = entry["token"]
				}
				out[name] = cleaned
				kept = append(kept, name)
			}
			continue
		}
		cleaned := map[string]any{"mode": "local"}
		if ModeIsRemoteLike(entry["mode"]) {
			cleaned["mode"] = entry["mode"]
		}
		if cleaned["mode"] == "local" {
			if savedSSH := NormalizeSSHConfig(entry["savedSsh"]); savedSSH != nil {
				cleaned["savedSsh"] = savedSSH.object()
			}
		}
		if u := strings.TrimSpace(jsOrString(entry["url"])); u != "" {
			cleaned["url"] = u
		}
		cleaned["authMode"] = NormAuthMode(entry["authMode"])
		if jsTruthy(entry["token"]) && isJSObject(entry["token"]) {
			cleaned["token"] = entry["token"]
		}
		if rp := NormalizeRemoteProfileName(entry["remoteProfile"]); rp != "" {
			cleaned["remoteProfile"] = rp
		}
		if headers := NormalizeRemoteHeaders(entry["headers"]); len(headers) > 0 {
			cleaned["headers"] = headers
		}
		if cleaned["mode"] == "cloud" {
			if cloudName := strings.TrimSpace(jsOrString(entry["name"])); cloudName != "" {
				cleaned["name"] = cloudName
			}
			if org := strings.TrimSpace(jsOrString(entry["org"])); org != "" {
				cleaned["org"] = org
			}
		}
		out[name] = cleaned
		kept = append(kept, name)
	}
	return out, kept
}

// Own enumerable entries of a decoded object or array the way Object.entries lists them
func jsEntries(v any) (map[string]any, bool) {
	switch t := v.(type) {
	case map[string]any:
		return t, true
	case []any:
		out := make(map[string]any, len(t))
		for i, item := range t {
			out[fmt.Sprint(i)] = item
		}
		return out, true
	}
	return nil, false
}

// Whether a decoded value is a non null JavaScript object, arrays included
func isJSObject(v any) bool {
	switch v.(type) {
	case map[string]any, []any:
		return true
	}
	return false
}

// Modification stamp of a file, ok false when it cannot be stat'ed
type fileStamp struct {
	ok  bool
	mod time.Time
}

func statStamp(path string) fileStamp {
	info, err := os.Stat(path)
	if err != nil {
		return fileStamp{}
	}
	return fileStamp{ok: true, mod: info.ModTime()}
}

func (a fileStamp) equal(b fileStamp) bool {
	return a.ok == b.ok && a.mod.Equal(b.mod)
}

func (s *Service) connectionConfigPath() string { return filepath.Join(s.userData, "connection.json") }

func (s *Service) connectionsRegistryPath() string {
	return filepath.Join(s.userData, "connections.json")
}

// The v1 connection settings, cached until the file changes on disk, local mode when missing or malformed
func (s *Service) readDesktopConnectionConfig() connectionConfig {
	path := s.connectionConfigPath()
	stamp := statStamp(path)
	if s.config != nil && s.configStamp.equal(stamp) {
		return *s.config
	}
	config := connectionConfig{Mode: "local", Remote: map[string]any{}, Profiles: map[string]any{}}
	if raw, err := os.ReadFile(path); err == nil {
		hardening.TightenSecretFileMode(path, hardening.SecretFileOptions{})
		var parsed any
		if json.Unmarshal(raw, &parsed) == nil {
			if obj, ok := jsEntries(parsed); ok {
				var remote any = map[string]any{}
				if isJSObject(obj["remote"]) {
					remote = obj["remote"]
				}
				if m, ok := remote.(map[string]any); ok {
					if m["authMode"] == "oauth" {
						m["authMode"] = "oauth"
					} else {
						m["authMode"] = "token"
					}
				}
				mode := "local"
				if obj["mode"] == "ssh" {
					mode = "ssh"
				} else if ModeIsRemoteLike(obj["mode"]) {
					mode = obj["mode"].(string)
				}
				profiles, order := sanitizeConnectionProfiles(obj["profiles"], topLevelObjectKeys(raw, "profiles"))
				config = connectionConfig{Mode: mode, Remote: remote, Profiles: profiles, order: order}
			}
		}
	}
	s.config = &config
	s.configStamp = stamp
	return config
}

// Writes connection.json owner only and refreshes the cache
func (s *Service) writeDesktopConnectionConfig(config connectionConfig) error {
	path := s.connectionConfigPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o777); err != nil {
		return err
	}
	data, err := jsonStringifyIndent(config)
	if err != nil {
		return err
	}
	if err := hardening.WriteSecretFileAtomic(path, data, hardening.SecretFileOptions{}); err != nil {
		return err
	}
	s.config = &config
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	s.configStamp = fileStamp{ok: true, mod: info.ModTime()}
	return nil
}

// The v2 registry, imported from v1 once, healed of v1 drift, degraded to local only when corrupt
func (s *Service) readDesktopConnectionsRegistry() (ConnectionRegistry, error) {
	path := s.connectionsRegistryPath()
	stamp := statStamp(path)
	if s.registry != nil && s.registryStamp.equal(stamp) {
		return *s.registry, nil
	}
	if !stamp.ok {
		config := s.readDesktopConnectionConfig()
		registry := MigrateV1ToRegistry(config.object(), config.profileOrder())
		if s.writeDesktopConnectionsRegistry(registry) != nil {
			s.registry = &registry
			s.registryStamp = fileStamp{}
		}
		return *s.registry, nil
	}
	hardening.TightenSecretFileMode(path, hardening.SecretFileOptions{})
	var registry ConnectionRegistry
	var parsed any
	raw, err := os.ReadFile(path)
	if err == nil {
		err = json.Unmarshal(raw, &parsed)
	}
	if err != nil {
		s.preserveCorruptRegistrySidecar()
		registry = NormalizeRegistry(nil)
	} else {
		registry = NormalizeRegistry(parsed)
	}
	if n := len(registry.Quarantined); n > 0 {
		noun := "ies were"
		if n == 1 {
			noun = "y was"
		}
		s.log(fmt.Sprintf(`[connections] %d malformed registry entr%s quarantined (kept under "quarantined" in connections.json); healthy connections loaded normally.`, n, noun))
	}
	reconciled, changed, err := ReconcileRegistryDrift(registry, s.readDesktopConnectionConfig().object())
	if err != nil {
		return registry, err
	}
	if changed {
		registry = reconciled
		if s.writeDesktopConnectionsRegistry(registry) == nil {
			return *s.registry, nil
		}
		s.registry = &registry
		s.registryStamp = fileStamp{}
		return registry, nil
	}
	s.registry = &registry
	s.registryStamp = stamp
	return registry, nil
}

// Copies an unparseable connections.json aside once per corruption event so a later write never loses it
func (s *Service) preserveCorruptRegistrySidecar() {
	path := s.connectionsRegistryPath()
	raw, err := os.ReadFile(path)
	if err != nil || strings.TrimSpace(string(raw)) == "" {
		return
	}
	stamp := strings.NewReplacer(":", "-", ".", "-").Replace(time.Now().UTC().Format("2006-01-02T15:04:05.000Z"))
	sidecar := path + ".corrupt-" + stamp
	if _, err := os.Stat(sidecar); err != nil {
		if os.WriteFile(sidecar, raw, 0o600) != nil {
			return
		}
	}
	s.log(fmt.Sprintf("[connections] connections.json could not be parsed; preserved the original file at %s and continuing with a local-only registry. No connection data was deleted.", sidecar))
}

// Writes connections.json owner only and refreshes the cache
func (s *Service) writeDesktopConnectionsRegistry(registry ConnectionRegistry) error {
	path := s.connectionsRegistryPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o777); err != nil {
		return err
	}
	data, err := jsonStringifyIndent(registry)
	if err != nil {
		return err
	}
	if err := hardening.WriteSecretFileAtomic(path, data, hardening.SecretFileOptions{}); err != nil {
		return err
	}
	s.registry = &registry
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	s.registryStamp = fileStamp{ok: true, mod: info.ModTime()}
	return nil
}
