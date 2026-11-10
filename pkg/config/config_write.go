package config

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/nickheyer/icebath/pkg/home"
)

// Strings a YAML 1.1 reader resolves to a bool or null, quoted on write at every depth
var yaml11AmbiguousWords = map[string]bool{
	"y": true, "n": true, "yes": true, "no": true, "true": true, "false": true,
	"on": true, "off": true, "null": true, "~": true,
}

// Mapping of an existing config file, refusing an unreadable or unparseable one before a write replaces it
func RequireReadableConfigBeforeWrite(ctx context.Context, configPath string) (map[string]any, error) {
	if configPath == "" {
		if snapshot := WorkerConfigSnapshot(); snapshot != nil {
			return Clone(snapshot).(map[string]any), nil
		}
		configPath = home.ConfigPath(home.Dir(ctx))
	}
	if _, err := ConfigVersion(ctx, configPath); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return map[string]any{}, nil
		}
		if IsOSError(err) {
			return nil, RefuseOverwrite(configPath, "cannot be accessed", err, fixPerms)
		}
		return nil, err
	}
	loaded, err := ReadConfigDoc(ctx, configPath)
	if err != nil {
		var exit SystemExitError
		if errors.As(err, &exit) {
			return nil, err
		}
		if IsOSError(err) {
			return nil, RefuseOverwrite(configPath, "cannot be read", err, fixPerms)
		}
		if werr := WarnConfigParseFailure(ctx, configPath, err, "refuse-write"); werr != nil {
			return nil, werr
		}
		return nil, RefuseOverwrite(configPath, "has a formatting error", err,
			fmt.Sprintf(fixYAML, BackupsDirDisplay(home.Dir(ctx))))
	}
	if loaded == nil {
		return map[string]any{}, nil
	}
	m, ok := loaded.(map[string]any)
	if !ok {
		kind := PyTypeName(loaded)
		terr := fmt.Errorf("top-level YAML must be a mapping, got %s", kind)
		if werr := WarnConfigParseFailure(ctx, configPath, terr, "refuse-write"); werr != nil {
			return nil, werr
		}
		return nil, RefuseOverwrite(configPath, "must start with settings names, but its top level is a "+kind, terr,
			fmt.Sprintf(fixYAML, BackupsDirDisplay(home.Dir(ctx))))
	}
	return m, nil
}

// Document node of a YAML file loaded through the round trip editor, an empty mapping when missing or blank
func roundtripLoad(path string) (*yaml.Node, []byte, error) {
	data, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, nil, err
	}
	doc, err := NewRoundtripYAML().Load(data)
	if err != nil {
		return nil, nil, err
	}
	blank := doc.Node == nil || len(doc.Node.Content) == 0 || (doc.Node.Content[0].Kind == yaml.ScalarNode && doc.Node.Content[0].Tag == "!!null")
	if _, err := doc.Root(); err != nil {
		return nil, nil, err
	}
	if blank {
		return doc.Node, nil, nil
	}
	return doc.Node, data, nil
}

// Writes a round trip document through the editor with optional trailing content via an atomic replace
func roundtripDump(ctx context.Context, path string, doc *yaml.Node, source []byte, extra string) error {
	var buf strings.Builder
	if err := NewRoundtripYAML().Dump(&RoundtripDoc{Node: doc, Source: source}, &buf); err != nil {
		return err
	}
	return atomicPreservingWrite(ctx, path, []byte(buf.String()+extra), "."+strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))+"_")
}

// Flushes a written temp file to disk before the replace, swapped by tests counting syncs
var fsyncFile = (*os.File).Sync

// Replaces the symlink resolved target through a synced temp file keeping its mode and owner
func atomicPreservingWrite(ctx context.Context, path string, data []byte, prefix string) error {
	return atomicWriteMode(ctx, path, data, prefix, 0, false)
}

// Atomic replace of the symlink resolved target, the forced mode when set, else the existing or umask mode
func atomicWriteMode(ctx context.Context, path string, data []byte, prefix string, forced os.FileMode, force bool) error {
	if _, err := home.Mkdir(ctx, filepath.Dir(path)); err != nil {
		return err
	}
	target := path
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		target = resolved
	}
	mode, haveMode := os.FileMode(0), false
	var owner fileOwnerID
	if fi, err := os.Stat(target); err == nil {
		mode, haveMode = fi.Mode().Perm(), true
		owner = fileOwnerOf(fi)
	} else {
		mode, haveMode = defaultNewFileMode()
	}
	if force {
		mode, haveMode = forced, true
	}
	tmp, err := os.CreateTemp(filepath.Dir(target), prefix+"*.tmp")
	if err != nil {
		return err
	}
	name := tmp.Name()
	fail := func(err error) error {
		tmp.Close()
		os.Remove(name)
		return err
	}
	if haveMode {
		if err := tmp.Chmod(mode); err != nil {
			return fail(err)
		}
	}
	if _, err := tmp.Write(data); err != nil {
		return fail(err)
	}
	if err := fsyncFile(tmp); err != nil {
		return fail(err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(name)
		return err
	}
	if err := os.Rename(name, target); err != nil {
		os.Remove(name)
		return err
	}
	restoreFileOwner(target, owner)
	if haveMode {
		_ = os.Chmod(target, mode)
	}
	return nil
}

// Plain value as a node with YAML 1.1 ambiguous strings quoted in the given style at every depth
func rtNode(value any, quote yaml.Style) (*yaml.Node, error) {
	switch v := value.(type) {
	case map[string]any:
		node := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
		for _, k := range MappingKeys(v) {
			child, err := rtNode(v[k], quote)
			if err != nil {
				return nil, err
			}
			node.Content = append(node.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: k}, child)
		}
		return node, nil
	case []any:
		node := &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"}
		for _, item := range v {
			child, err := rtNode(item, quote)
			if err != nil {
				return nil, err
			}
			node.Content = append(node.Content, child)
		}
		return node, nil
	case string:
		node := &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: v}
		if yaml11AmbiguousWords[strings.ToLower(v)] {
			node.Style = quote
		}
		return node, nil
	case nil:
		return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!null", Value: "null"}, nil
	case float64:
		return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!float", Value: yamlFloatText(v)}, nil
	case float32:
		return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!float", Value: yamlFloatText(float64(v))}, nil
	}
	rv := reflect.ValueOf(value)
	switch rv.Kind() {
	case reflect.Slice, reflect.Array:
		items := make([]any, rv.Len())
		for i := range items {
			items[i] = rv.Index(i).Interface()
		}
		return rtNode(items, quote)
	case reflect.Map:
		m := make(map[string]any, rv.Len())
		for _, k := range rv.MapKeys() {
			m[fmt.Sprint(k.Interface())] = rv.MapIndex(k).Interface()
		}
		return rtNode(m, quote)
	}
	var n yaml.Node
	if err := n.Encode(value); err != nil {
		return nil, err
	}
	return &n, nil
}

// Float spelled the way the round trip representer writes it, a YAML 1.1 float on reload
func yamlFloatText(f float64) string {
	switch {
	case math.IsNaN(f):
		return ".nan"
	case math.IsInf(f, 1):
		return ".inf"
	case math.IsInf(f, -1):
		return "-.inf"
	}
	s := pyFloatRepr(f)
	if mant, exp, ok := strings.Cut(s, "e"); ok && !strings.Contains(mant, ".") {
		s = mant + ".0e" + exp
	}
	return s
}

// Index of the value node of key in a mapping node, or minus one
func mappingIndex(node *yaml.Node, key string) int {
	for i := 0; i+1 < len(node.Content); i += 2 {
		if node.Content[i].Value == key {
			return i + 1
		}
	}
	return -1
}

// Places a value node at key, keeping the comments of the node it replaces
func mappingSet(node *yaml.Node, key string, value *yaml.Node) {
	if i := mappingIndex(node, key); i >= 0 {
		replaceKeepingComments(node, i, value)
		return
	}
	node.Content = append(node.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key}, value)
}

// Puts value at index of a container node carrying over the old node comments
func replaceKeepingComments(container *yaml.Node, i int, value *yaml.Node) {
	old := container.Content[i]
	value.HeadComment, value.LineComment, value.FootComment = old.HeadComment, old.LineComment, old.FootComment
	container.Content[i] = value
}

// Drops key and its value from a mapping node
func mappingDelete(node *yaml.Node, key string) {
	if i := mappingIndex(node, key); i >= 0 {
		node.Content = append(node.Content[:i-1], node.Content[i+1:]...)
	}
}

// Sets one dotted key in a YAML file keeping comments, order and quoting, nil removing the key
func atomicRoundtripYAMLUpdate(ctx context.Context, path, keyPath string, value any) error {
	if _, err := home.Mkdir(ctx, filepath.Dir(path)); err != nil {
		return err
	}
	doc, source, err := roundtripLoad(path)
	if err != nil {
		return err
	}
	current := doc.Content[0]
	keys := SplitPath(keyPath)
	for i := 0; ; {
		remaining := keys[i:]
		present := map[string]any{}
		for j := 0; j+1 < len(current.Content); j += 2 {
			present[current.Content[j].Value] = nil
		}
		seg, consumed, ok := greedyLiteral(present, remaining)
		if !ok {
			seg, consumed = remaining[0], 1
		}
		if i+consumed == len(keys) {
			if value == nil {
				mappingDelete(current, seg)
				break
			}
			node, err := rtNode(value, yaml.SingleQuotedStyle)
			if err != nil {
				return err
			}
			mappingSet(current, seg, node)
			break
		}
		idx := mappingIndex(current, seg)
		if idx < 0 || current.Content[idx].Kind != yaml.MappingNode {
			if value == nil {
				return nil
			}
			mappingSet(current, seg, &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"})
			idx = mappingIndex(current, seg)
		}
		current = current.Content[idx]
		i += consumed
	}
	return roundtripDump(ctx, path, doc, source, "")
}

// Persists a whole config state onto the file, reassigning only changed nodes and deleting absent keys
func atomicRoundtripYAMLSave(ctx context.Context, path string, state map[string]any, extraOnCreate string) error {
	if _, err := home.Mkdir(ctx, filepath.Dir(path)); err != nil {
		return err
	}
	if _, err := RequireReadableConfigBeforeWrite(ctx, path); err != nil {
		return err
	}
	creating := true
	if data, err := os.ReadFile(path); err == nil {
		creating = strings.TrimSpace(string(data)) == ""
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	doc, source, err := roundtripLoad(path)
	if err != nil {
		return err
	}
	if err := rtMerge(doc.Content[0], state); err != nil {
		return err
	}
	extra := ""
	if creating {
		extra = extraOnCreate
	}
	return roundtripDump(ctx, path, doc, source, extra)
}

// Merges src onto a mapping node, appending new keys and deleting keys src lacks
func rtMerge(dst *yaml.Node, src map[string]any) error {
	seen := map[string]bool{}
	var content []*yaml.Node
	for i := 0; i+1 < len(dst.Content); i += 2 {
		key := dst.Content[i].Value
		value, keep := src[key]
		if !keep || seen[key] {
			continue
		}
		seen[key] = true
		content = append(content, dst.Content[i], dst.Content[i+1])
		if err := rtMergeItem(content, len(content)-1, value); err != nil {
			return err
		}
	}
	for _, key := range MappingKeys(src) {
		if seen[key] {
			continue
		}
		node, err := rtNode(src[key], yaml.DoubleQuotedStyle)
		if err != nil {
			return err
		}
		content = append(content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key}, node)
	}
	dst.Content = content
	return nil
}

// Merges value into the node at index of a container slice, reassigning it only when it changed
func rtMergeItem(content []*yaml.Node, i int, value any) error {
	current := content[i]
	if m, ok := value.(map[string]any); ok && current.Kind == yaml.MappingNode {
		return rtMerge(current, m)
	}
	if l, ok := value.([]any); ok && current.Kind == yaml.SequenceNode {
		return rtMergeSeq(current, l)
	}
	var have any
	if err := current.Decode(&have); err == nil {
		have = normalizeMaps(have)
		_, haveBool := have.(bool)
		_, wantBool := value.(bool)
		if pyEq(have, value) && haveBool == wantBool {
			return nil
		}
	}
	node, err := rtNode(value, yaml.DoubleQuotedStyle)
	if err != nil {
		return err
	}
	holder := &yaml.Node{Content: content}
	replaceKeepingComments(holder, i, node)
	return nil
}

// Merges a list item by item onto a sequence node, appending extra items and truncating the rest
func rtMergeSeq(dst *yaml.Node, src []any) error {
	for i, value := range src {
		if i < len(dst.Content) {
			if err := rtMergeItem(dst.Content, i, value); err != nil {
				return err
			}
			continue
		}
		node, err := rtNode(value, yaml.DoubleQuotedStyle)
		if err != nil {
			return err
		}
		dst.Content = append(dst.Content, node)
	}
	dst.Content = dst.Content[:len(src)]
	return nil
}
