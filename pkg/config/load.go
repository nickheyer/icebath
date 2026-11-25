package config

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"

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
	doc, err := SafeLoadText(data, true)
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
	NoteRuntimeImport("config")
	return ReadDocument(GetConfigPath(h))
}

// Document as stored, refused when its agent section is set to something other than a mapping
func Canonicalize(doc map[string]any) (map[string]any, error) {
	if err := sectionMappingError("agent", doc["agent"]); err != nil {
		return nil, err
	}
	return doc, nil
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
