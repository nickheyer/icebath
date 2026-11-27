package skills

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/nickheyer/icebath/internal/config"
	skillsconfig "github.com/nickheyer/icebath/internal/skills/config"
	"github.com/nickheyer/icebath/pkg/fsx"
)

// Several skills loaded together under one slash command, kept as a YAML file of the bundles folder
type Bundle struct {
	Name string
	// Its slash command, without the slash
	Slug   string
	Skills []string
	// Its file
	Path        string
	Description string
	// Told to the model along with the skills
	Instruction string
}

// Folder of the profile's bundles
func (s *Scope) BundlesDir() string {
	if dir := strings.TrimSpace(s.Config.GetString(skillsconfig.BundlesDir)); dir != "" {
		return fsx.ExpandHome(s.expandVars(dir))
	}
	return filepath.Join(s.Home(), "skill-bundles")
}

// Bundle files of the folder: the .yaml ones, then the .yml ones, each sorted
func (s *Scope) bundleFiles() []string {
	entries, err := os.ReadDir(s.BundlesDir())
	if err != nil {
		return nil
	}
	fold := func(name string) string { return name }
	if runtime.GOOS == "windows" {
		fold = strings.ToLower
	}
	var out []string
	for _, ext := range []string{".yaml", ".yml"} {
		var names []string
		for _, entry := range entries {
			if strings.HasSuffix(fold(entry.Name()), ext) {
				names = append(names, entry.Name())
			}
		}
		slices.SortFunc(names, func(a, b string) int { return strings.Compare(fold(a), fold(b)) })
		for _, name := range names {
			out = append(out, filepath.Join(s.BundlesDir(), name))
		}
	}
	return out
}

// One bundle file, false with a warning when it is no usable bundle
func readBundle(path string) (Bundle, bool, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		slog.Warn("skills: could not read a bundle", "path", path, "err", err)
		return Bundle{}, false, nil
	}
	content, err := strictText(data, path)
	if err != nil {
		return Bundle{}, false, fmt.Errorf("bundle %s: %w", path, err)
	}
	parsed, err := decodeYAML(content)
	if err != nil {
		slog.Warn("skills: a bundle is not valid YAML; skipping", "path", path, "err", err)
		return Bundle{}, false, nil
	}
	skip := func(reason string) (Bundle, bool, error) {
		slog.Warn("skills: skipping a bundle that "+reason, "path", path)
		return Bundle{}, false, nil
	}
	m, ok := parsed.(map[string]any)
	if !ok {
		return skip("is not a mapping")
	}
	stem := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	name := stem
	if config.Truthy(m["name"]) {
		name = text(m["name"])
	}
	if name = strings.TrimSpace(name); name == "" {
		return skip("has no name")
	}
	items, isList := m["skills"].([]any)
	if !isList || len(items) == 0 {
		return skip("has no skills list")
	}
	var members []string
	empty := 0
	for _, item := range items {
		member := strings.TrimSpace(text(item))
		if item == nil || member == "" {
			empty++
			continue
		}
		members = append(members, member)
	}
	if len(members) == 0 {
		return skip("has an empty skills list")
	}
	if empty > 0 {
		slog.Warn("skills: a bundle has empty skills entries; ignoring them", "path", path, "count", empty)
	}
	slug := Slug(name)
	if slug == "" {
		return skip("yields an empty slug")
	}
	description := strings.TrimSpace(text(m["description"]))
	if description == "" {
		description = fmt.Sprintf("Load %d skills as a bundle", len(members))
	}
	return Bundle{Name: name, Slug: slug, Skills: members, Path: path, Description: description, Instruction: strings.TrimSpace(text(m["instruction"]))}, true, nil
}

// Every bundle of the profile by slug, the alphabetically first file keeping a slug two share
func (s *Scope) Bundles() (map[string]Bundle, error) {
	out := map[string]Bundle{}
	for _, path := range s.bundleFiles() {
		bundle, ok, err := readBundle(path)
		if err != nil {
			return nil, err
		}
		if !ok {
			continue
		}
		if prior, taken := out[bundle.Slug]; taken {
			slog.Warn("skills: two bundles share a slash command; the first is kept", "command", "/"+bundle.Slug, "skipped", path, "kept", prior.Path)
			continue
		}
		out[bundle.Slug] = bundle
	}
	return out, nil
}

// Bundles sorted by slug
func (s *Scope) BundleList() ([]Bundle, error) {
	bundles, err := s.Bundles()
	if err != nil {
		return nil, err
	}
	out := make([]Bundle, 0, len(bundles))
	for _, bundle := range bundles {
		out = append(out, bundle)
	}
	slices.SortFunc(out, func(a, b Bundle) int { return cmp.Compare(a.Slug, b.Slug) })
	return out, nil
}

// The bundle of a name, its slug matched
func (s *Scope) Bundle(name string) (Bundle, bool, error) {
	bundles, err := s.Bundles()
	if err != nil {
		return Bundle{}, false, err
	}
	bundle, ok := bundles[Slug(name)]
	return bundle, ok, nil
}

// File a bundle of the name is kept in
func (s *Scope) bundlePath(name string) (string, error) {
	slug := Slug(name)
	if slug == "" {
		return "", fmt.Errorf("Bundle name %q normalizes to an empty slug", name)
	}
	return filepath.Join(s.BundlesDir(), slug+".yaml"), nil
}

// Writes a bundle, refusing to replace one unless asked; its file
func (s *Scope) SaveBundle(name string, skills []string, description, instruction string, overwrite bool) (string, error) {
	if name = strings.TrimSpace(name); name == "" {
		return "", errors.New("Bundle name is required")
	}
	var members []string
	for _, skill := range skills {
		if skill = strings.TrimSpace(skill); skill != "" {
			members = append(members, skill)
		}
	}
	if len(members) == 0 {
		return "", errors.New("Bundle must reference at least one skill")
	}
	path, err := s.bundlePath(name)
	if err != nil {
		return "", err
	}
	if fsx.Exists(path) && !overwrite {
		return "", fmt.Errorf("Bundle already exists at %s: %w", path, fs.ErrExist)
	}
	content := struct {
		Name        string   `yaml:"name"`
		Skills      []string `yaml:"skills"`
		Description string   `yaml:"description,omitempty"`
		Instruction string   `yaml:"instruction,omitempty"`
	}{name, members, description, instruction}
	data, err := yaml.Marshal(content)
	if err != nil {
		return "", err
	}
	return path, fsx.Replace(path, data, 0o644)
}

// Deletes the bundle of a name; its file
func (s *Scope) DeleteBundle(name string) (string, error) {
	path, err := s.bundlePath(name)
	if err != nil {
		return "", err
	}
	if !fsx.Exists(path) {
		return "", fmt.Errorf("No bundle at %s: %w", path, fs.ErrNotExist)
	}
	return path, os.Remove(path)
}

// The message a bundle's slash line becomes, false when no bundle has the name or none of its skills loads
func (s *Scope) invokeBundle(ctx context.Context, name, instruction string) (Invocation, bool, error) {
	bundles, err := s.Bundles()
	if err != nil {
		return Invocation{}, false, err
	}
	key, ok := resolveKey(name, bundles)
	if !ok {
		return Invocation{}, false, nil
	}
	bundle := bundles[key]
	names, missing, disabled, blocks := s.blocks(ctx, bundle.Skills, func(id string) (loaded, bool) { return s.load(ctx, id) }, func(string) string {
		return fmt.Sprintf("[Loaded as part of the \"%s\" skill bundle.]", bundle.Name)
	}, blockOptions{disabled: s.Disabled()})
	if len(blocks) == 0 {
		return Invocation{}, false, fmt.Errorf("No skill of the %s bundle could be loaded: %s.", bundle.Name, Missing(append(missing, disabled...)))
	}
	header := scaffold(fmt.Sprintf("\"%s\" skill bundle", bundle.Name), names, []string{"Bundle: " + bundle.Name}, missing, disabled, bundle.Instruction, instruction)
	return Invocation{Message: strings.Join(append([]string{header}, blocks...), "\n\n"), Loaded: names, Missing: missing}, true, nil
}
