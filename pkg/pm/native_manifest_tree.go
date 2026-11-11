package pm

import (
	"errors"
	"fmt"
	"io/fs"
	"path"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"gopkg.in/yaml.v3"
)

// Native plugin.yaml or plugin.yml of a plugin dir inside a tree, empty when neither exists
func NativeManifestFileFS(tree fs.FS, pluginDir string) (string, error) {
	for _, name := range []string{"plugin.yaml", "plugin.yml"} {
		p := path.Join(pluginDir, name)
		if _, err := fs.Stat(tree, p); err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			return "", &PyError{Class: "ValueError", Message: fmt.Sprintf("Could not inspect plugin manifest %s: %s", p, pypath.OSErrorText(err))}
		}
		return p, nil
	}
	return "", nil
}

// Manifest mapping of a native manifest inside a tree, read and decoded the way ReadNativeManifest reads a file
func ReadNativeManifestFS(tree fs.FS, name string) (map[string]any, error) {
	data, err := fs.ReadFile(tree, name)
	if err == nil {
		data = []byte(strings.TrimPrefix(string(data), "\ufeff"))
		if uerr := strictUTF8(data); uerr != nil {
			err = &PyError{Class: "UnicodeDecodeError", Message: uerr.Error()}
		}
	}
	if err != nil {
		var pyErr *PyError
		message := pypath.OSErrorText(err)
		if errors.As(err, &pyErr) {
			message = pyErr.Message
		}
		return nil, &PyError{Class: "ValueError", Message: fmt.Sprintf("Could not read plugin manifest %s: %s", name, message)}
	}
	return decodeNativeManifest(name, string(data))
}

// Mapping a native manifest text decodes to, an empty document reading as an empty mapping
func decodeNativeManifest(name, text string) (map[string]any, error) {
	var data any
	if err := yaml.Unmarshal([]byte(text), &data); err != nil {
		return nil, &PyError{Class: "ValueError", Message: fmt.Sprintf("Could not read plugin manifest %s: %s", name, err)}
	}
	if data == nil {
		return map[string]any{}, nil
	}
	manifest, ok := yamlNormalize(data).(map[string]any)
	if !ok {
		return nil, &PyError{Class: "ValueError", Message: "Plugin manifest must be a mapping: " + name}
	}
	return manifest, nil
}
