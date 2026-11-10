package profiles

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/internal/pyfs"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

const (
	ManifestFilename    = "distribution.yaml"
	EnvTemplateFilename = ".env.template"
	EnvExampleFilename  = ".env.EXAMPLE"
)

// Install, update and manifest failure of a profile distribution
type DistributionError struct{ Msg string }

func (e *DistributionError) Error() string { return e.Msg }

func distErrorf(format string, args ...any) error {
	return &DistributionError{Msg: fmt.Sprintf(format, args...)}
}

// One env_requires row, Default nil when the manifest gives none
type EnvRequirement struct {
	Name        string
	Description string
	Required    bool
	Default     any
}

// Requirement with the dataclass defaults, required and without a default
func NewEnvRequirement(name, description string) EnvRequirement {
	return EnvRequirement{Name: name, Description: description, Required: true}
}

// Text of a manifest key, default when the value is falsy
func manifestStr(data map[string]any, key, def string) string {
	v := data[key]
	if !pyTruthy(v) {
		return def
	}
	return pyStr(v)
}

// Requirement parsed from one env_requires entry, which must be a mapping with a name
func EnvRequirementFromDict(data any) (EnvRequirement, error) {
	m, ok := data.(map[string]any)
	if !ok {
		return EnvRequirement{}, distErrorf("env_requires entry must be a mapping, got %s", pyrepr.TypeName(data))
	}
	name := pystr.Strip(manifestStr(m, "name", ""))
	if name == "" {
		return EnvRequirement{}, distErrorf("env_requires entry missing 'name'")
	}
	required := true
	if v, ok := m["required"]; ok {
		required = pyTruthy(v)
	}
	return EnvRequirement{Name: name, Description: manifestStr(m, "description", ""), Required: required, Default: m["default"]}, nil
}

// Mapping of the requirement with required and default written only when they differ from the defaults
func (r EnvRequirement) ToDict() map[string]any {
	out := map[string]any{"name": r.Name, "description": r.Description}
	keys := []string{"name", "description"}
	if !r.Required {
		out["required"] = false
		keys = append(keys, "required")
	}
	if r.Default != nil {
		out["default"] = r.Default
		keys = append(keys, "default")
	}
	config.SetMappingKeys(out, keys)
	return out
}

// Parsed distribution.yaml plus the source and install stamp recorded after install
type DistributionManifest struct {
	Name              string
	Version           string
	Description       string
	HermesRequires    string
	Author            string
	License           string
	EnvRequires       []EnvRequirement
	DistributionOwned []string
	Source            string
	InstalledAt       string
}

// Manifest with the dataclass defaults, version 0.1.0
func NewDistributionManifest(name string) *DistributionManifest {
	return &DistributionManifest{Name: name, Version: "0.1.0"}
}

// Manifest of a parsed YAML root, rejecting a non mapping, a missing name and malformed lists
func DistributionManifestFromDict(data any) (*DistributionManifest, error) {
	m, ok := data.(map[string]any)
	if !ok {
		return nil, distErrorf("%s must be a mapping, got %s", ManifestFilename, pyrepr.TypeName(data))
	}
	name := pystr.Strip(manifestStr(m, "name", ""))
	if name == "" {
		return nil, distErrorf("%s missing 'name'", ManifestFilename)
	}
	envRaw := m["env_requires"]
	if !pyTruthy(envRaw) {
		envRaw = []any{}
	}
	envList, ok := envRaw.([]any)
	if !ok {
		return nil, distErrorf("env_requires must be a list")
	}
	ownedRaw := m["distribution_owned"]
	ownedList, isList := ownedRaw.([]any)
	if pyTruthy(ownedRaw) && !isList {
		return nil, distErrorf("distribution_owned must be a list")
	}
	out := &DistributionManifest{
		Name: name, Version: manifestStr(m, "version", "0.1.0"), Description: manifestStr(m, "description", ""),
		HermesRequires: manifestStr(m, "hermes_requires", ""), Author: manifestStr(m, "author", ""),
		License: manifestStr(m, "license", ""),
	}
	for _, e := range envList {
		req, err := EnvRequirementFromDict(e)
		if err != nil {
			return nil, err
		}
		out.EnvRequires = append(out.EnvRequires, req)
	}
	for _, p := range ownedList {
		if s := pyStr(p); pystr.Strip(s) != "" {
			out.DistributionOwned = append(out.DistributionOwned, strings.Trim(pystr.Strip(s), "/"))
		}
	}
	out.Source, out.InstalledAt = manifestStr(m, "source", ""), manifestStr(m, "installed_at", "")
	return out, nil
}

// Mapping in on disk key order, optional keys written only when set
func (d *DistributionManifest) ToDict() map[string]any {
	out := map[string]any{"name": d.Name, "version": d.Version}
	keys := []string{"name", "version"}
	add := func(k string, v any, set bool) {
		if set {
			out[k] = v
			keys = append(keys, k)
		}
	}
	add("description", d.Description, d.Description != "")
	add("hermes_requires", d.HermesRequires, d.HermesRequires != "")
	add("author", d.Author, d.Author != "")
	add("license", d.License, d.License != "")
	env := make([]any, len(d.EnvRequires))
	for i, r := range d.EnvRequires {
		env[i] = r.ToDict()
	}
	add("env_requires", env, len(env) > 0)
	owned := make([]any, len(d.DistributionOwned))
	for i, p := range d.DistributionOwned {
		owned[i] = p
	}
	add("distribution_owned", owned, len(owned) > 0)
	add("source", d.Source, d.Source != "")
	add("installed_at", d.InstalledAt, d.InstalledAt != "")
	config.SetMappingKeys(out, keys)
	return out
}

// Manifest of profileDir, nil without a distribution.yaml and an error when it does not parse
func ReadManifest(profileDir string) (*DistributionManifest, error) {
	path := filepath.Join(profileDir, ManifestFilename)
	if st, err := os.Stat(path); err != nil || !st.Mode().IsRegular() {
		return nil, nil
	}
	data, err := readUTF8Sig(path)
	if err != nil {
		return nil, distErrorf("Failed to parse %s: %s", path, pyExcText(err))
	}
	doc, err := config.FastSafeLoad(data)
	if err != nil {
		return nil, distErrorf("Failed to parse %s: %v", path, err)
	}
	if !pyTruthy(doc) {
		doc = map[string]any{}
	}
	return DistributionManifestFromDict(doc)
}

// Bytes of a utf-8-sig text file, the BOM dropped and invalid UTF-8 a UnicodeDecodeError
func readUTF8Sig(path string) ([]byte, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	data = bytes.TrimPrefix(data, []byte("\xef\xbb\xbf"))
	for i := 0; i < len(data); {
		r, n := utf8.DecodeRune(data[i:])
		if r == utf8.RuneError && n <= 1 {
			return nil, pystr.UTF8DecodeError(data, i)
		}
		i += n
	}
	return data, nil
}

// Python str of an exception, the errno form for OS errors
func pyExcText(err error) string {
	var pe *os.PathError
	if errors.As(err, &pe) {
		return pyfs.OSErrorText(err)
	}
	return err.Error()
}

// Atomically writes distribution.yaml, keeping an existing file's mode and creating a new one 0644
func WriteManifest(ctx context.Context, profileDir string, manifest *DistributionManifest) (string, error) {
	path := filepath.Join(profileDir, ManifestFilename)
	if _, err := os.Stat(path); err == nil {
		return path, config.AtomicYAMLWrite(ctx, path, manifest.ToDict(), "")
	}
	text, err := config.SafeDump(manifest.ToDict())
	if err != nil {
		return path, err
	}
	return path, pyfs.AtomicWriteTextMode(ctx, path, string(text), ".distribution_", 0o644)
}
