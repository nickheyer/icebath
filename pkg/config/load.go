package config

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"gopkg.in/yaml.v3"

	"github.com/nickheyer/icebath/pkg/home"
)

// Error of a config.yaml that does not parse
type InvalidUserConfigError struct {
	Path string
	Err  error
}

func (e *InvalidUserConfigError) Error() string {
	return "config file " + e.Path + " cannot be parsed: " + e.Err.Error()
}

func (e *InvalidUserConfigError) Unwrap() error { return e.Err }

type fileSig struct {
	size  int64
	mtime int64
	hash  [32]byte
}

type cacheEntry struct {
	sig      fileSig
	exists   bool
	managed  fileSig
	expanded Tree
	raw      map[string]any
	refs     map[string]*string
}

var (
	cacheMu sync.Mutex
	cache   = map[string]*cacheEntry{}
)

// Signature of a file, exists false when it is absent
func signature(path string) (fileSig, bool, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fileSig{}, false, nil
		}
		return fileSig{}, false, err
	}
	st, err := os.Stat(path)
	if err != nil {
		return fileSig{}, false, err
	}
	return fileSig{size: st.Size(), mtime: st.ModTime().UnixNano(), hash: sha256.Sum256(data)}, true, nil
}

// Parsed user document of a YAML file, empty when the file is absent or blank
func ReadDocument(path string) (map[string]any, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return map[string]any{}, nil
		}
		return nil, err
	}
	var doc any
	if err := yaml.Unmarshal(bytes.TrimPrefix(data, []byte("\xef\xbb\xbf")), &doc); err != nil {
		return nil, &InvalidUserConfigError{Path: path, Err: err}
	}
	if doc == nil {
		return map[string]any{}, nil
	}
	m := AsMap(doc)
	if m == nil {
		return nil, &InvalidUserConfigError{Path: path, Err: fmt.Errorf("top level is %T, a mapping is required", doc)}
	}
	return normalizeMaps(m).(map[string]any), nil
}

// Document with every nested mapping as a string keyed map
func normalizeMaps(v any) any {
	switch c := v.(type) {
	case map[string]any:
		for k, val := range c {
			c[k] = normalizeMaps(val)
		}
		return c
	case map[any]any:
		out := make(map[string]any, len(c))
		for k, val := range c {
			out[fmt.Sprint(k)] = normalizeMaps(val)
		}
		return out
	case []any:
		for i, val := range c {
			c[i] = normalizeMaps(val)
		}
		return c
	}
	return v
}

// Raw user config of a home, no defaults merged and no references expanded
func ReadUser(h string) (map[string]any, error) {
	NoteRuntimeImport("hermes_cli.config")
	return ReadDocument(home.ConfigPath(h))
}

// Document with the model section and the turn cap in their canonical places
func Canonicalize(doc map[string]any) map[string]any {
	return normalizeModelKeys(normalizeMaxTurns(doc))
}

// Document with a root max_turns moved under agent
func normalizeMaxTurns(doc map[string]any) map[string]any {
	raw, has := doc["max_turns"]
	if !has {
		return doc
	}
	out := Clone(doc).(map[string]any)
	agent, _ := out["agent"].(map[string]any)
	if agent == nil {
		agent = map[string]any{}
	}
	if _, set := agent["max_turns"]; !set {
		agent["max_turns"] = raw
	}
	out["agent"] = agent
	delete(out, "max_turns")
	return out
}

// Model and provider of a model default value that may be a string or a mapping
func SplitModelDefault(raw any) (string, string) {
	if m, ok := raw.(map[string]any); ok {
		model := m["model"]
		if model == nil {
			model = m["default"]
		}
		return strings.TrimSpace(fmt.Sprint(valueOrEmpty(model))), strings.TrimSpace(fmt.Sprint(valueOrEmpty(m["provider"])))
	}
	return strings.TrimSpace(fmt.Sprint(valueOrEmpty(raw))), ""
}

func valueOrEmpty(v any) any {
	if v == nil {
		return ""
	}
	return v
}

// Document with root provider, base_url, context_length and api_base folded into the model section
func normalizeModelKeys(doc map[string]any) map[string]any {
	modelIn := doc["model"]
	modelMap, isMap := modelIn.(map[string]any)
	needsWork := false
	if isMap {
		if _, ok := modelMap["provider"]; ok {
			if _, isStr := modelMap["provider"].(string); !isStr {
				needsWork = true
			}
		}
		for _, k := range []string{"api_base", "model", "name"} {
			if v, ok := modelMap[k]; ok && v != nil && v != "" {
				needsWork = true
			}
		}
		for _, k := range []string{"default", "model", "name"} {
			if _, ok := modelMap[k].(map[string]any); ok {
				needsWork = true
			}
		}
	}
	hasRoot := false
	for _, k := range []string{"provider", "base_url", "context_length", "api_base"} {
		if v, ok := doc[k]; ok && v != nil && v != "" {
			hasRoot = true
		}
	}
	if !hasRoot && !needsWork {
		return doc
	}
	out := Clone(doc).(map[string]any)
	model := map[string]any{}
	switch m := out["model"].(type) {
	case map[string]any:
		model = m
	case nil:
	default:
		if s := strings.TrimSpace(fmt.Sprint(m)); s != "" {
			model["default"] = s
		}
	}
	out["model"] = model
	for _, key := range []string{"default", "model", "name"} {
		nested, ok := model[key].(map[string]any)
		if !ok {
			continue
		}
		name, provider := SplitModelDefault(nested)
		model[key] = name
		outer := strings.TrimSpace(fmt.Sprint(valueOrEmpty(model["provider"])))
		if provider != "" && (outer == "" || outer == "auto") {
			model["provider"] = provider
		}
	}
	for _, key := range []string{"provider", "base_url", "context_length"} {
		rootVal, ok := out[key]
		if ok && rootVal != nil && rootVal != "" {
			if cur, has := model[key]; !has || cur == nil || cur == "" {
				model[key] = rootVal
			}
		}
		delete(out, key)
	}
	if p, ok := model["provider"]; ok && p != nil {
		model["provider"] = strings.TrimSpace(fmt.Sprint(p))
	}
	for _, alias := range []any{out["api_base"], model["api_base"]} {
		if alias != nil && alias != "" {
			if cur, has := model["base_url"]; !has || cur == nil || cur == "" {
				model["base_url"] = alias
			}
		}
	}
	delete(out, "api_base")
	delete(model, "api_base")
	alias := model["model"]
	if alias == nil || alias == "" {
		alias = model["name"]
	}
	if cur := model["default"]; (cur == nil || cur == "") && alias != nil && alias != "" {
		model["default"] = alias
	}
	if cur := model["default"]; cur != nil && cur != "" {
		delete(model, "model")
		delete(model, "name")
	}
	return out
}

// Effective configuration of a home, defaults merged under the user file and references expanded
func Load(h string, lookup Lookup) (Tree, error) {
	NoteRuntimeImport("hermes_cli.config")
	path := home.ConfigPath(h)
	sig, exists, err := signature(path)
	if err != nil {
		return nil, err
	}
	cacheMu.Lock()
	entry := cache[path]
	cacheMu.Unlock()
	managedSig := managedConfigSignature()
	if entry != nil && entry.exists == exists && (!exists || entry.sig == sig) && entry.managed == managedSig && SnapshotCurrent(entry.refs, lookup) {
		return entry.expanded.Clone(), nil
	}
	user := map[string]any{}
	if exists {
		user, err = ReadDocument(path)
		if err != nil {
			return nil, err
		}
	}
	canonical := Canonicalize(DeepMerge(map[string]any(Defaults()), Canonicalize(user)))
	expanded := Tree(Canonicalize(ApplyManagedOverlay(Expand(canonical, lookup).(map[string]any))))
	cacheMu.Lock()
	cache[path] = &cacheEntry{sig: sig, exists: exists, managed: managedSig, expanded: expanded.Clone(), raw: Clone(user).(map[string]any), refs: RefSnapshot(canonical, lookup)}
	cacheMu.Unlock()
	return expanded, nil
}

// Forgets every cached configuration document
func ResetCache() {
	cacheMu.Lock()
	cache = map[string]*cacheEntry{}
	cacheMu.Unlock()
}

// Header written above a new config file
const fileHeader = "# icebath configuration. Settings live here, secrets live in .env beside this file.\n"

// Writes the effective document of a home, dropping values equal to the defaults the user never set
func Save(h string, doc Tree) error {
	path := home.ConfigPath(h)
	if _, err := home.Mkdir(h); err != nil {
		return err
	}
	raw, err := ReadDocument(path)
	if err != nil {
		return err
	}
	normalized := Canonicalize(Clone(map[string]any(doc)).(map[string]any))
	stripped := StripDefaults(normalized, map[string]any(Defaults()), ExplicitPaths(Canonicalize(raw)))
	stripped["_config_version"] = Version
	return writeDocument(path, stripped)
}

// Writes the raw user document of a home exactly as given
func SaveUser(h string, doc map[string]any) error {
	if _, err := home.Mkdir(h); err != nil {
		return err
	}
	return writeDocument(home.ConfigPath(h), doc)
}

// Sets one dotted key in the user config of a home
func SetKey(h, key string, value any) error {
	raw, err := ReadUser(h)
	if err != nil {
		return err
	}
	tree := Tree(raw)
	if err := tree.Set(key, value); err != nil {
		return err
	}
	return SaveUser(h, map[string]any(tree))
}

// Removes one dotted key from the user config of a home, reporting whether it existed
func UnsetKey(h, key string) (bool, error) {
	raw, err := ReadUser(h)
	if err != nil {
		return false, err
	}
	tree := Tree(raw)
	if !tree.Unset(key) {
		return false, nil
	}
	return true, SaveUser(h, map[string]any(tree))
}

// Writes a document onto the existing file keeping its comments and key order
func writeDocument(path string, doc map[string]any) error {
	existing, err := os.ReadFile(path)
	var root yaml.Node
	if err == nil && len(bytes.TrimSpace(existing)) > 0 {
		if perr := yaml.Unmarshal(existing, &root); perr != nil {
			root = yaml.Node{}
		}
	}
	if root.Kind == 0 || len(root.Content) == 0 || root.Content[0].Kind != yaml.MappingNode {
		root = yaml.Node{Kind: yaml.DocumentNode, Content: []*yaml.Node{{Kind: yaml.MappingNode, Tag: "!!map"}}}
	}
	applyMapping(root.Content[0], doc)
	var buf bytes.Buffer
	if err != nil || len(bytes.TrimSpace(existing)) == 0 {
		buf.WriteString(fileHeader)
	}
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(&root); err != nil {
		return err
	}
	if err := enc.Close(); err != nil {
		return err
	}
	ResetCache()
	return AtomicWrite(path, buf.Bytes(), 0o600)
}

// Rewrites a mapping node to hold exactly the keys of value, keeping comments of retained keys
func applyMapping(node *yaml.Node, value map[string]any) {
	var content []*yaml.Node
	seen := map[string]bool{}
	for i := 0; i+1 < len(node.Content); i += 2 {
		keyNode, valNode := node.Content[i], node.Content[i+1]
		want, keep := value[keyNode.Value]
		if !keep {
			continue
		}
		seen[keyNode.Value] = true
		content = append(content, keyNode, mergeNode(valNode, want))
	}
	keys := make([]string, 0, len(value))
	for k := range value {
		if !seen[k] {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	for _, k := range keys {
		content = append(content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: k}, buildNode(value[k]))
	}
	node.Content = content
}

// Node carrying want, reusing the existing node and its comments when the shapes match
func mergeNode(existing *yaml.Node, want any) *yaml.Node {
	switch w := want.(type) {
	case map[string]any:
		if existing.Kind == yaml.MappingNode {
			applyMapping(existing, w)
			return existing
		}
	case []any:
		if existing.Kind == yaml.SequenceNode {
			fresh := buildNode(w)
			if sameSequence(existing, fresh) {
				return existing
			}
			fresh.HeadComment, fresh.LineComment, fresh.FootComment = existing.HeadComment, existing.LineComment, existing.FootComment
			return fresh
		}
	default:
		if existing.Kind == yaml.ScalarNode {
			fresh := buildNode(want)
			if existing.Value == fresh.Value && existing.Tag == fresh.Tag {
				return existing
			}
			fresh.HeadComment, fresh.LineComment, fresh.FootComment = existing.HeadComment, existing.LineComment, existing.FootComment
			fresh.Style = existing.Style
			if fresh.Tag != existing.Tag {
				fresh.Style = 0
			}
			return fresh
		}
	}
	fresh := buildNode(want)
	fresh.HeadComment, fresh.LineComment, fresh.FootComment = existing.HeadComment, existing.LineComment, existing.FootComment
	return fresh
}

// Reports whether two sequence nodes encode the same values
func sameSequence(a, b *yaml.Node) bool {
	var av, bv any
	if a.Decode(&av) != nil || b.Decode(&bv) != nil {
		return false
	}
	return Equal(normalizeMaps(av), normalizeMaps(bv))
}

// Fresh node for a value
func buildNode(value any) *yaml.Node {
	var n yaml.Node
	if err := n.Encode(value); err != nil {
		return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: fmt.Sprint(value)}
	}
	return &n
}

// Subdirectories every home carries
var homeSubdirs = []string{
	"cron", "sessions", "logs", "memories", "skills", "plugins", "skill-bundles", "hooks",
	"cache", "cache/scratch", "hook_outputs", "pending", "scripts", "trajectories", "skins",
}

// Creates the home directory skeleton, the config file and the secrets file when missing
func EnsureHome(h string) error {
	if _, err := home.Mkdir(h); err != nil {
		return err
	}
	for _, sub := range homeSubdirs {
		if err := os.MkdirAll(filepath.Join(h, sub), 0o700); err != nil {
			return err
		}
	}
	cfg := home.ConfigPath(h)
	if _, err := os.Stat(cfg); errors.Is(err, os.ErrNotExist) {
		if err := writeDocument(cfg, map[string]any{"_config_version": Version}); err != nil {
			return err
		}
	}
	env := home.EnvPath(h)
	if _, err := os.Stat(env); errors.Is(err, os.ErrNotExist) {
		if err := AtomicWrite(env, []byte("# Secrets only. Settings belong in config.yaml.\n"), 0o600); err != nil {
			return err
		}
	}
	if err := SecureEnvFile(env); err != nil {
		return err
	}
	return seedSoul(h)
}
