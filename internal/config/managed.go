package config

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"

	"gopkg.in/yaml.v3"

	"github.com/nickheyer/icebath/internal/fileutil"
)

// Environment variable an administrator sets to move the managed scope directory, honored only for an existing directory
const EnvManagedDir = "ICEBATH_MANAGED_DIR"

// Managed scope directory every platform looks in unless ICEBATH_MANAGED_DIR names another
const defaultManagedDir = "/etc/icebath"

// Raised for a managed env query before the environment loader installed its env file parser
var ErrNoEnvFileParser = errors.New("no env file parser is installed to read the managed env file")

// Directory of the administrator managed scope, empty when no scope is present
func ManagedDir() string {
	dir := defaultManagedDir
	if v := strings.TrimSpace(os.Getenv(EnvManagedDir)); v != "" {
		dir = v
	}
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		return ""
	}
	return dir
}

// Path of a file in the managed scope, empty without a scope
func managedFile(name string) string {
	dir := ManagedDir()
	if dir == "" {
		return ""
	}
	return filepath.Join(dir, name)
}

// Parsed managed files by path with the versions they were parsed at
type managedCache struct {
	mu     sync.Mutex
	config map[string]managedEntry[*yaml.Node]
	env    map[string]managedEntry[map[string]string]
}

// One parsed managed file
type managedEntry[T any] struct {
	version Revision
	value   T
}

// Warns loudly that an administrator file is ignored because it does not parse
func warnManagedParse(path string, err error) {
	slog.Warn(fmt.Sprintf("managed scope: failed to parse %s: %v — IGNORING this managed file. Admin policy from this file is NOT being applied. Fix and restart.", path, err))
}

// Raw managed config.yaml tree, an empty mapping when absent, malformed, or under a backend that takes no managed layer
func (s *Store) managedTree(b Backend) *yaml.Node {
	if !b.HonorsManagedConfig() {
		return emptyMapping()
	}
	path := managedFile("config.yaml")
	if path == "" {
		return emptyMapping()
	}
	v, err := fileCacheVersion(path, false)
	if err != nil {
		return emptyMapping()
	}
	s.managed.mu.Lock()
	if e, ok := s.managed.config[path]; ok && e.version == v {
		s.managed.mu.Unlock()
		return plainCopy(e.value)
	}
	s.managed.mu.Unlock()
	doc, err := readYAMLFile(path)
	if err != nil {
		warnManagedParse(path, err)
		return emptyMapping()
	}
	tree := emptyMapping()
	if isMapping(doc.root) {
		tree = doc.root
	}
	s.managed.mu.Lock()
	if s.managed.config == nil {
		s.managed.config = map[string]managedEntry[*yaml.Node]{}
	}
	s.managed.config[path] = managedEntry[*yaml.Node]{v, plainCopy(tree)}
	s.managed.mu.Unlock()
	return tree
}

// Managed tree as an overlay, the model keys canonical, a bare model promoted to model.default, references expanded
func (s *Store) managedOverlay(ctx context.Context, managed *yaml.Node) *yaml.Node {
	if !isMapping(managed) || len(managed.Content) == 0 {
		return nil
	}
	out := normalizeRootModelKeys(managed)
	if m := childOf(out, "model"); m != nil && isStringNode(m) {
		out = plainCopy(out)
		wrapped := emptyMapping()
		putKey(wrapped, "default", plainCopy(m))
		putKey(out, "model", wrapped)
	}
	return s.expandTree(ctx, out)
}

// Managed tree as the effective view lays it on, references expanded before the model keys are made canonical
func (s *Store) managedViewOverlay(ctx context.Context, managed *yaml.Node) *yaml.Node {
	if !isMapping(managed) || len(managed.Content) == 0 {
		return nil
	}
	out := normalizeRootModelKeys(s.expandTree(ctx, managed))
	if m := childOf(out, "model"); m != nil && isStringNode(m) {
		out = plainCopy(out)
		wrapped := emptyMapping()
		putKey(wrapped, "default", plainCopy(m))
		putKey(out, "model", wrapped)
	}
	return out
}

// Document with the administrator pinned values merged over it leaf by leaf, unchanged when no scope is present
func (s *Store) ApplyManagedOverlay(ctx context.Context, doc *YAMLDocument) (*YAMLDocument, error) {
	b, err := s.backend(ctx)
	if err != nil {
		return nil, err
	}
	base := emptyMapping()
	if doc != nil && isMapping(doc.root) {
		base = doc.root
	}
	overlay := s.managedViewOverlay(ctx, s.managedTree(b))
	if overlay == nil {
		return &YAMLDocument{root: plainCopy(base)}, nil
	}
	return &YAMLDocument{root: deepMerge(base, overlay)}, nil
}

// Dotted leaf keys the managed config pins, sorted
func (s *Store) ManagedKeys(ctx context.Context) ([]string, error) {
	b, err := s.backend(ctx)
	if err != nil {
		return nil, err
	}
	set := map[string]bool{}
	flattenKeys(s.managedTree(b), "", set)
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	slices.Sort(out)
	return out, nil
}

// Reports whether the exact dotted key is pinned by the managed config
func (s *Store) IsKeyManaged(ctx context.Context, key string) (bool, error) {
	keys, err := s.ManagedKeys(ctx)
	return slices.Contains(keys, key), err
}

// Managed env file as its assignments, empty when absent or unreadable, which is logged
func (s *Store) ManagedEnv(ctx context.Context) (map[string]string, error) {
	path := managedFile(".env")
	if path == "" {
		return map[string]string{}, nil
	}
	v, err := fileCacheVersion(path, false)
	if err != nil {
		return map[string]string{}, nil
	}
	if s.parseEnvFile == nil {
		return nil, ErrNoEnvFileParser
	}
	s.managed.mu.Lock()
	if e, ok := s.managed.env[path]; ok && e.version == v {
		s.managed.mu.Unlock()
		return maps.Clone(e.value), nil
	}
	s.managed.mu.Unlock()
	text, err := fileutil.ReadText(path)
	var parsed map[string]string
	if err == nil {
		parsed, err = s.parseEnvFile([]byte(text))
	}
	if err != nil {
		warnManagedParse(path, err)
		return map[string]string{}, nil
	}
	s.managed.mu.Lock()
	if s.managed.env == nil {
		s.managed.env = map[string]managedEntry[map[string]string]{}
	}
	s.managed.env[path] = managedEntry[map[string]string]{v, maps.Clone(parsed)}
	s.managed.mu.Unlock()
	return parsed, nil
}

// Reports whether the variable is pinned by the managed env file
func (s *Store) IsEnvManaged(ctx context.Context, name string) (bool, error) {
	env, err := s.ManagedEnv(ctx)
	if err != nil {
		return false, err
	}
	_, ok := env[name]
	return ok, nil
}

// Non-empty terminal backend the managed config pins, ok false when it pins none or an empty or null one
func (s *Store) PinnedTerminalBackend(ctx context.Context) (string, bool, error) {
	doc, err := s.ApplyManagedOverlay(ctx, nil)
	if err != nil {
		return "", false, err
	}
	backend := treeGet(doc.root, "terminal", "backend")
	if !isStringNode(backend) {
		return "", false, nil
	}
	v := strings.TrimSpace(backend.Value)
	return v, v != "", nil
}
