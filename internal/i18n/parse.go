package i18n

import (
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"math/big"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"

	"gopkg.in/yaml.v3"

	"github.com/nickheyer/icebath/internal/config"
)

var (
	errUnhashableKey                  = errors.New("found unhashable key")
	_                config.Languages = (*Catalog)(nil)
)

type ShapeError struct {
	Path string
	Got  string
}

func (e *ShapeError) Error() string {
	return fmt.Sprintf("%s: top level must be a mapping, got %s", e.Path, e.Got)
}

type Locale struct {
	Messages map[string]string
	NonText  []string
}

type File struct {
	Lang    string
	Surface Surface
	Path    string
}

func ReadLocale(path string) (Locale, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Locale{}, err
	}
	return parseLocale(path, data)
}

func parseLocale(path string, data []byte) (Locale, error) {
	var root yaml.Node
	if err := config.LoadYAML(data, &root); err != nil {
		return Locale{}, fmt.Errorf("%s: %w", path, err)
	}
	loc := Locale{Messages: map[string]string{}}
	if root.Kind == 0 || (root.Kind == yaml.ScalarNode && root.ShortTag() == "!!null") {
		return loc, nil
	}
	if !isMapping(&root) {
		return Locale{}, &ShapeError{Path: path, Got: typeName(&root)}
	}
	if err := walk(&root, "", loc.Messages, &loc.NonText); err != nil {
		return Locale{}, fmt.Errorf("%s: %w", path, err)
	}
	return loc, nil
}

func isMapping(n *yaml.Node) bool {
	return n.Kind == yaml.MappingNode && n.ShortTag() != "!!set"
}

func isText(n *yaml.Node) bool {
	return n.Kind == yaml.ScalarNode && n.ShortTag() == "!!str"
}

func walk(n *yaml.Node, prefix string, out map[string]string, nonText *[]string) error {
	if isMapping(n) {
		for i := 0; i+1 < len(n.Content); i += 2 {
			key, err := keyText(n.Content[i])
			if err != nil {
				return err
			}
			path := key
			if prefix != "" {
				path = prefix + "." + key
			}
			if err := walk(n.Content[i+1], path, out, nonText); err != nil {
				return err
			}
		}
		return nil
	}
	if isText(n) {
		out[prefix] = n.Value
		return nil
	}
	if prefix == "" {
		prefix = "<root>"
	}
	*nonText = append(*nonText, prefix)
	return nil
}

func keyText(n *yaml.Node) (string, error) {
	if n.Kind != yaml.ScalarNode {
		return "", fmt.Errorf("line %d: %w", n.Line, errUnhashableKey)
	}
	if n.Style == 0 && mayBeTyped(n.Value) {
		var typed yaml.Node
		if err := config.LoadYAML([]byte(n.Value), &typed); err == nil && typed.Kind == yaml.ScalarNode {
			n = &typed
		}
	}
	switch n.ShortTag() {
	case "!!bool":
		if n.Value == "true" {
			return "True", nil
		}
		return "False", nil
	case "!!null":
		return "None", nil
	case "!!int":
		if v, ok := new(big.Int).SetString(n.Value, 0); ok {
			return v.String(), nil
		}
	case "!!float":
		if f, err := strconv.ParseFloat(strings.ReplaceAll(n.Value, "_", ""), 64); err == nil {
			return floatRepr(f), nil
		}
	}
	return n.Value, nil
}

func mayBeTyped(key string) bool {
	if key == "" || strings.ContainsRune("0123456789+-.~", rune(key[0])) {
		return true
	}
	switch strings.ToLower(key) {
	case "yes", "no", "on", "off", "true", "false", "null":
		return true
	}
	return false
}

func typeName(n *yaml.Node) string {
	switch n.ShortTag() {
	case "!!seq":
		return "list"
	case "!!set":
		return "set"
	case "!!int":
		return "int"
	case "!!float":
		return "float"
	case "!!bool":
		return "bool"
	case "!!binary":
		return "bytes"
	case "!!timestamp":
		if strings.ContainsAny(n.Value, "tT :") {
			return "datetime"
		}
		return "date"
	}
	return "str"
}

func Flatten(tree map[string]any) map[string]string {
	out := map[string]string{}
	flattenValue(tree, "", out)
	return out
}

func flattenValue(v any, prefix string, out map[string]string) {
	join := func(key string) string {
		if prefix == "" {
			return key
		}
		return prefix + "." + key
	}
	switch t := v.(type) {
	case map[string]any:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			flattenValue(t[k], join(k), out)
		}
	case map[string]string:
		for k, s := range t {
			out[join(k)] = s
		}
	case string:
		out[prefix] = t
	}
}

func missing(err error) bool {
	return errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ENOTDIR) || errors.Is(err, syscall.ELOOP)
}

func ScanDir(dir string) ([]File, error) {
	fi, err := os.Stat(dir)
	if err != nil {
		if missing(err) {
			return nil, nil
		}
		return nil, err
	}
	if !fi.IsDir() {
		return nil, nil
	}
	return scanFS(os.DirFS(dir), dir)
}

func scanFS(fsys fs.FS, dir string) ([]File, error) {
	entries, err := fs.ReadDir(fsys, ".")
	if err != nil {
		return nil, err
	}
	var found []File
	for _, e := range entries {
		m := localeFilePattern.FindStringSubmatch(e.Name())
		if m == nil {
			continue
		}
		fi, err := fs.Stat(fsys, e.Name())
		if err != nil {
			if missing(err) {
				continue
			}
			return nil, err
		}
		if !fi.Mode().IsRegular() {
			continue
		}
		path := e.Name()
		if dir != "" {
			path = filepath.Join(dir, e.Name())
		}
		surface := Surface(m[2])
		if m[2] == "" {
			surface = Core
		}
		if !surface.Valid() {
			slog.Warn(fmt.Sprintf("Ignoring locale file %s: unknown surface %s (expected one of %s)", path, quote(m[2], false), surfaceList))
			continue
		}
		found = append(found, File{Lang: m[1], Surface: surface, Path: path})
	}
	return found, nil
}
