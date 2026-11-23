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
	"github.com/nickheyer/icebath/pkg/internal/pyfs"
)

// Strings a YAML 1.1 reader resolves to a bool or null, quoted on write at every depth
var yaml11AmbiguousWords = map[string]bool{
	"y": true, "n": true, "yes": true, "no": true, "true": true, "false": true,
	"on": true, "off": true, "null": true, "~": true,
}

// Mapping of an existing config file, refusing an unreadable or unparseable one before a write replaces it
func RequireReadableConfigBeforeWrite(ctx context.Context, configPath string) (map[string]any, error) {
	if configPath == "" {
		if snapshot := workerConfigSnapshot(); snapshot != nil {
			return Clone(snapshot).(map[string]any), nil
		}
		configPath = GetConfigPath(home.Dir(ctx))
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

// Replaces the symlink resolved target through a synced temp file keeping its mode and owner
func atomicPreservingWrite(ctx context.Context, path string, data []byte, prefix string) error {
	mode, ok := pyfs.PreserveFileMode(path)
	return atomicWriteDoc(ctx, path, data, pyfs.WriteOptions{Prefix: prefix, Mode: mode, ModeSet: ok, PreserveOwner: true})
}

// Atomic replace keeping the owner, the forced mode when set, else the existing bits or the umask mode
func atomicWriteMode(ctx context.Context, path string, data []byte, prefix string, forced os.FileMode, force, fsyncDir bool) error {
	if !force {
		mode, ok := pyfs.PreserveFileMode(path)
		return atomicWriteDoc(ctx, path, data, pyfs.WriteOptions{Prefix: prefix, Mode: mode, ModeSet: ok, PreserveOwner: true, FsyncDir: fsyncDir})
	}
	return atomicWriteDoc(ctx, path, data, pyfs.WriteOptions{Prefix: prefix, Mode: pyfs.PosixModeBits(forced), ModeSet: true, PreserveOwner: true, FsyncDir: fsyncDir})
}

// Serialized document written as text through the shared atomic writer
func atomicWriteDoc(ctx context.Context, path string, data []byte, o pyfs.WriteOptions) error {
	return pyfs.AtomicWrite(ctx, path, func(f *os.File) error {
		_, err := f.WriteString(pyfs.PlatformNewlines(string(data)))
		return err
	}, o)
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

// Node an alias stands for, the node itself otherwise
func rtResolve(node *yaml.Node) *yaml.Node {
	for node.Kind == yaml.AliasNode && node.Alias != nil {
		node = node.Alias
	}
	return node
}

// Keys of a mapping node in file order
func mappingKeyNames(node *yaml.Node) []string {
	names := make([]string, 0, len(node.Content)/2)
	for i := 0; i+1 < len(node.Content); i += 2 {
		names = append(names, node.Content[i].Value)
	}
	return names
}

// Literal key of a mapping node matching the longest prefix of parts, with the segments it consumes
func rtLiteral(node *yaml.Node, parts []string) (string, int, bool) {
	names := mappingKeyNames(node)
	present := make(map[string]any, len(names))
	for _, name := range names {
		present[name] = nil
	}
	return greedyLiteral(present, parts)
}

// Sets the value at a dotted path under a YAML node the way Tree.Set does: list items by index, literal dotted keys first, and no nested key beside a dotted sibling it would shadow
func rtSet(root *yaml.Node, path string, value any) error {
	node, err := rtNode(value, yaml.SingleQuotedStyle)
	if err != nil {
		return err
	}
	parts := SplitPath(path)
	current := root
	for i := 0; i < len(parts); {
		remaining := parts[i:]
		atLeaf := len(remaining) == 1
		current = rtResolve(current)
		switch current.Kind {
		case yaml.SequenceNode:
			part := remaining[0]
			idx, perr := listIndex(part, len(current.Content))
			if atLeaf {
				if perr != nil {
					return perr
				}
				replaceKeepingComments(current, idx, node)
				return nil
			}
			if perr != nil && perr.Kind == "ValueError" {
				return &PathError{"TypeError", fmt.Sprintf("Cannot navigate into list at key %s: segment %s is not a numeric index", PyRepr(path), PyRepr(part))}
			}
			if perr != nil {
				return perr
			}
			current = current.Content[idx]
			i++
		case yaml.MappingNode:
			if seg, n, ok := rtLiteral(current, remaining); ok {
				at := mappingIndex(current, seg)
				if i+n == len(parts) {
					replaceKeepingComments(current, at, node)
					return nil
				}
				if kind := rtResolve(current.Content[at]).Kind; kind != yaml.MappingNode && kind != yaml.SequenceNode {
					replaceKeepingComments(current, at, &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"})
				}
				current = current.Content[at]
				i += n
				continue
			}
			part := remaining[0]
			if atLeaf {
				mappingSet(current, part, node)
				return nil
			}
			for _, name := range mappingKeyNames(current) {
				if strings.HasPrefix(name, part+".") {
					return &PhantomKeyError{Part: part, Path: path, Shadowed: name, Escaped: strings.ReplaceAll(name, ".", `\.`)}
				}
			}
			next := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
			mappingSet(current, part, next)
			current = next
			i++
		default:
			var held any
			if err := current.Decode(&held); err != nil {
				return err
			}
			return &PathError{"TypeError", fmt.Sprintf("Cannot navigate into %s at key %s", PyTypeName(held), PyRepr(path))}
		}
	}
	return nil
}

// Removes the value at a dotted path under a YAML node and every mapping it leaves empty, the way Tree.Unset does, reporting whether the path named a value
func rtUnset(root *yaml.Node, path string) bool {
	type step struct {
		container *yaml.Node
		// Index of the value node inside the container's content
		at int
	}
	parts := SplitPath(path)
	var steps []step
	current := root
	for i := 0; i < len(parts); {
		current = rtResolve(current)
		at, consumed := -1, 1
		switch current.Kind {
		case yaml.SequenceNode:
			idx, perr := listIndex(parts[i], len(current.Content))
			if perr != nil {
				return false
			}
			at = idx
		case yaml.MappingNode:
			seg, n, ok := rtLiteral(current, parts[i:])
			if !ok {
				return false
			}
			at, consumed = mappingIndex(current, seg), n
		default:
			return false
		}
		steps = append(steps, step{current, at})
		current = current.Content[at]
		i += consumed
	}
	for j := len(steps) - 1; j >= 0; j-- {
		container, at := steps[j].container, steps[j].at
		if container.Kind == yaml.MappingNode {
			container.Content = append(container.Content[:at-1], container.Content[at+1:]...)
		} else {
			container.Content = append(container.Content[:at], container.Content[at+1:]...)
		}
		if container.Kind != yaml.MappingNode || len(container.Content) != 0 {
			break
		}
	}
	return true
}

// Sets and then removes dotted keys of a YAML file in one rewrite that keeps comments, order and quoting; nothing is written when a key cannot be set or nothing changes
func atomicRoundtripYAMLApply(ctx context.Context, path string, set []KeyValue, unset []string) error {
	if _, err := home.Mkdir(ctx, filepath.Dir(path)); err != nil {
		return err
	}
	doc, source, err := roundtripLoad(path)
	if err != nil {
		return err
	}
	root := doc.Content[0]
	changed := false
	for _, kv := range set {
		if kv.Value == nil {
			changed = rtUnset(root, kv.Key) || changed
			continue
		}
		if err := rtSet(root, kv.Key, kv.Value); err != nil {
			return err
		}
		changed = true
	}
	for _, key := range unset {
		changed = rtUnset(root, key) || changed
	}
	if !changed {
		return nil
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
