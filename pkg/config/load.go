package config

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/nickheyer/icebath/pkg/home"
)

// Error of a config.yaml that does not parse
type InvalidUserConfigError struct {
	Path    string
	Err     error
	Message string
}

func (e *InvalidUserConfigError) Error() string {
	if e.Message != "" {
		return e.Message
	}
	return "config file " + e.Path + " cannot be parsed: " + e.Err.Error()
}

func (e *InvalidUserConfigError) Unwrap() error { return e.Err }

type fileSig struct {
	size  int64
	mtime int64
	hash  [32]byte
}

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
	data, err := readYAMLFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return map[string]any{}, nil
		}
		return nil, err
	}
	doc, err := FastSafeLoad(data)
	if err != nil {
		return nil, &InvalidUserConfigError{Path: path, Err: err}
	}
	if doc == nil {
		return map[string]any{}, nil
	}
	m := AsMap(doc)
	if m == nil {
		return nil, &InvalidUserConfigError{Path: path, Err: fmt.Errorf("top level is %T, a mapping is required", doc)}
	}
	return m, nil
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
	return ReadDocument(GetConfigPath(h))
}

// Document with legacy root max_turns under agent and the model section canonical, the Python error for an agent that is no mapping
func Canonicalize(doc map[string]any) (map[string]any, error) {
	moved, err := normalizeMaxTurns(doc)
	if err != nil {
		return nil, err
	}
	return normalizeModelKeys(moved), nil
}

// Shallow copy with a root max_turns moved under agent, an agent key kept as a mapping whenever present
func normalizeMaxTurns(doc map[string]any) (map[string]any, error) {
	agent, err := pyDictOf(doc["agent"])
	if err != nil {
		return nil, err
	}
	out := make(map[string]any, len(doc)+1)
	for k, v := range doc {
		out[k] = v
	}
	order := MappingKeys(doc)
	raw, hasRoot := doc["max_turns"]
	if _, set := agent["max_turns"]; hasRoot && !set {
		agent["max_turns"] = raw
		AppendMappingKey(agent, "max_turns")
	}
	if _, has := doc["agent"]; len(agent) > 0 || has {
		if !has {
			order = append(order, "agent")
		}
		out["agent"] = agent
	}
	delete(out, "max_turns")
	SetMappingKeys(out, order)
	return out, nil
}

// Model and provider of a model default value that may be a string or a mapping
func SplitModelDefault(raw any) (string, string) {
	if m, ok := raw.(map[string]any); ok {
		provider := strings.TrimFunc(pyOrEmpty(m["provider"]), isPySpace)
		model := m["model"]
		if !PyTruthy(model) {
			model = m["default"]
		}
		return strings.TrimFunc(pyOrEmpty(model), isPySpace), provider
	}
	return strings.TrimFunc(pyOrEmpty(raw), isPySpace), ""
}

// Canonicalizes the model section of a parsed config document the way every load and save does
func NormalizeRootModelKeys(doc map[string]any) map[string]any { return normalizeModelKeys(doc) }

// Document with root provider, base_url, context_length and api_base folded into the model section
func normalizeModelKeys(doc map[string]any) map[string]any {
	modelMap, isMap := doc["model"].(map[string]any)
	needsWork := false
	if isMap {
		if p := modelMap["provider"]; p != nil {
			if _, isStr := p.(string); !isStr {
				needsWork = true
			}
		}
		if PyTruthy(modelMap["api_base"]) || PyTruthy(modelMap["model"]) || PyTruthy(modelMap["name"]) {
			needsWork = true
		}
		for _, k := range []string{"default", "model", "name"} {
			if _, ok := modelMap[k].(map[string]any); ok {
				needsWork = true
			}
		}
	}
	hasRoot := false
	for _, k := range []string{"provider", "base_url", "context_length", "api_base"} {
		if PyTruthy(doc[k]) {
			hasRoot = true
		}
	}
	if !hasRoot && !needsWork {
		return doc
	}
	out := make(map[string]any, len(doc))
	for k, v := range doc {
		out[k] = v
	}
	SetMappingKeys(out, MappingKeys(doc))
	model := map[string]any{}
	switch m := out["model"].(type) {
	case map[string]any:
		for k, v := range m {
			model[k] = v
		}
		SetMappingKeys(model, MappingKeys(m))
	default:
		if PyTruthy(m) {
			model["default"] = m
		}
	}
	setOrderedKey(out, "model", model)
	for _, key := range []string{"default", "model", "name"} {
		nested, ok := model[key].(map[string]any)
		if !ok {
			continue
		}
		name, provider := SplitModelDefault(nested)
		model[key] = name
		if provider != "" {
			if outer := strings.TrimFunc(pyOrEmpty(model["provider"]), isPySpace); outer == "" || outer == "auto" {
				setOrderedKey(model, "provider", provider)
			}
		}
	}
	for _, key := range []string{"provider", "base_url", "context_length"} {
		if rootVal := out[key]; PyTruthy(rootVal) && !PyTruthy(model[key]) {
			setOrderedKey(model, key, rootVal)
		}
		delete(out, key)
	}
	if p := model["provider"]; p != nil {
		model["provider"] = strings.TrimFunc(PyStr(p), isPySpace)
	}
	for _, alias := range []any{out["api_base"], model["api_base"]} {
		if PyTruthy(alias) && !PyTruthy(model["base_url"]) {
			setOrderedKey(model, "base_url", alias)
		}
	}
	delete(out, "api_base")
	delete(model, "api_base")
	alias := model["model"]
	if !PyTruthy(alias) {
		alias = model["name"]
	}
	if !PyTruthy(model["default"]) && PyTruthy(alias) {
		setOrderedKey(model, "default", alias)
	}
	if PyTruthy(model["default"]) {
		delete(model, "model")
		delete(model, "name")
	}
	return out
}

// Writes the effective document of a home, dropping values equal to the defaults the user never set
func Save(h string, doc Tree) error {
	return SaveConfig(context.Background(), h, map[string]any(doc), SaveOptions{})
}

// Writes the raw user document of a home exactly as given
func SaveUser(ctx context.Context, h string, doc map[string]any) error {
	if _, err := home.Mkdir(ctx, h); err != nil {
		return err
	}
	return AtomicConfigReplace(ctx, h, GetConfigPath(h), doc, "")
}

// Sets one dotted key in the user config of a home
func SetKey(ctx context.Context, h, key string, value any) error {
	raw, err := ReadUser(h)
	if err != nil {
		return err
	}
	tree := Tree(raw)
	if err := tree.Set(key, value); err != nil {
		return err
	}
	return SaveUser(ctx, h, map[string]any(tree))
}

// Removes one dotted key from the user config of a home, reporting whether it existed
func UnsetKey(ctx context.Context, h, key string) (bool, error) {
	raw, err := ReadUser(h)
	if err != nil {
		return false, err
	}
	tree := Tree(raw)
	if !tree.Unset(key) {
		return false, nil
	}
	return true, SaveUser(ctx, h, map[string]any(tree))
}
