package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
	"sync"

	"github.com/nickheyer/icebath/pkg/errchain"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	ry "github.com/nickheyer/icebath/pkg/internal/ruamelyaml"
	"gopkg.in/yaml.v3"
)

// Failure of the shared YAML reader or writer, a ruamel YAMLError or the Python exception a load raised
type YAMLError struct{ Err error }

func (e *YAMLError) Error() string { return e.Err.Error() }

func (e *YAMLError) Unwrap() error { return e.Err }

// Python class of the underlying failure, YAMLError for a writer failure without one
func (e *YAMLError) PyTypeName() string {
	if p, ok := e.Err.(ry.PyError); ok {
		return p.PyTypeName()
	}
	return "YAMLError"
}

// Reports whether err is a failure of the shared YAML reader or writer
func IsYAMLError(err error) bool {
	var ye *YAMLError
	return errors.As(err, &ye)
}

var (
	yamlDirectiveVersion = regexp.MustCompile(`\A((?:[ \t\r]*(?:#[^\n]*)?\n|%[^\n]*\n)*?)%YAML[ \t]+([0-9]+)\.([0-9]+)`)
	utf8BOM              = []byte("\xef\xbb\xbf")
)

// Bytes of a YAML document given as text, bytes or a reader
func readYAMLStream(stream any) ([]byte, error) {
	switch s := stream.(type) {
	case string:
		return []byte(s), nil
	case []byte:
		return s, nil
	case io.Reader:
		return io.ReadAll(s)
	}
	return nil, fmt.Errorf("YAML source is %s where text, bytes or a reader was expected", errchain.Kind(stream))
}

// Value of one YAML document given as a Python str, bytes or a reader yielding bytes, as builds it
func SafeLoad(stream any) (any, error) {
	if s, ok := stream.(string); ok {
		return safeLoadInput(ry.TextInput(s))
	}
	data, err := readYAMLStream(stream)
	if err != nil {
		return nil, err
	}
	return safeLoadInput(ry.BytesInput(data))
}

// Value of a YAML file's bytes read the way open with utf-8 or utf-8-sig and read does, universal newlines included
func SafeLoadText(data []byte, sig bool) (any, error) {
	text, err := PyReadText(data, sig)
	if err != nil {
		return nil, err
	}
	return safeLoadInput(ry.TextInput(text))
}

// Text of file bytes as a text mode read gives it, strict utf-8 or utf-8-sig with CRLF and CR turned into LF
func PyReadText(data []byte, sig bool) (string, error) {
	var text string
	var err error
	if sig {
		text, err = pystr.DecodeUTF8Sig(data)
	} else {
		text, err = pystr.DecodeUTF8(data)
	}
	if err != nil {
		return "", &YAMLError{ry.Raise("UnicodeDecodeError", err.Error())}
	}
	return strings.ReplaceAll(strings.ReplaceAll(text, "\r\n", "\n"), "\r", "\n"), nil
}

// The C loader's value, the pure loader answering any YAMLError of it, every other exception raised as is
func safeLoadInput(in ry.Input) (any, error) {
	doc, ok, err := ry.ComposeC(in)
	if err != nil {
		return nil, &YAMLError{err}
	}
	if ok {
		value, warnings, cerr := constructRoot(doc)
		emitYAMLWarnings(warnings)
		if cerr == nil {
			return value, nil
		}
		if !ry.IsYAMLError(cerr) {
			return nil, &YAMLError{cerr}
		}
	}
	pure, err := ry.ComposePure(in, false)
	if pure != nil {
		emitYAMLWarnings(pure.Warnings)
	}
	if err != nil {
		return nil, &YAMLError{err}
	}
	value, warnings, cerr := constructRoot(pure)
	emitYAMLWarnings(warnings)
	if cerr != nil {
		return nil, &YAMLError{cerr}
	}
	return value, nil
}

// Constructed value of a composed document, None for an empty stream
func constructRoot(doc *ry.Document) (any, []ry.Warning, error) {
	if doc.Root == nil {
		return nil, nil, nil
	}
	return constructDocument(doc)
}

// Stream Python warnings of YAML loads are written to
var YAMLWarningOut io.Writer = os.Stderr

var (
	yamlWarnMu   sync.Mutex
	yamlWarnSeen = map[string]bool{}
)

// Prints load warnings the way Python's warning display does, once filters by text and default ones by text and site
func emitYAMLWarnings(warnings []ry.Warning) {
	yamlWarnMu.Lock()
	defer yamlWarnMu.Unlock()
	for _, w := range warnings {
		key := w.Category + "\x00" + w.Message
		if !w.Site.Once {
			key += "\x00" + w.Site.Where
		}
		if yamlWarnSeen[key] {
			continue
		}
		yamlWarnSeen[key] = true
		fmt.Fprintf(YAMLWarningOut, "%s: %s: %s\n  %s\n", w.Site.Where, w.Category, w.Message, w.Site.Source)
	}
}

// Document bytes rewritten for the go-yaml editor and whether a YAML 1.2 directive selects 1.2 resolution
func yamlDocumentVersion(src []byte) ([]byte, bool) {
	m := yamlDirectiveVersion.FindSubmatchIndex(src)
	if m == nil {
		return src, false
	}
	if strings.TrimLeft(string(src[m[6]:m[7]]), "0") == "2" {
		patched := append(append(append([]byte{}, src[:m[6]]...), '1'), src[m[7]:]...)
		return patched, true
	}
	return src, false
}

// Comment, quote and order keeping editor for user authored YAML, lines never folded
type RoundtripYAML struct{}

// One loaded round trip document with the source it came from
type RoundtripDoc struct {
	Node   *yaml.Node
	Source []byte
}

// Fresh round trip editor, every operation owns its own parser and emitter state
func NewRoundtripYAML() *RoundtripYAML { return &RoundtripYAML{} }

// Parses a document for editing through the round trip loader, nil node for an empty stream
func (r *RoundtripYAML) Load(stream any) (*RoundtripDoc, error) {
	data, err := readYAMLStream(stream)
	if err != nil {
		return nil, err
	}
	text, err := PyReadText(data, false)
	if err != nil {
		return nil, err
	}
	in := ry.TextInput(text)
	if _, err := roundtripLoadValue(in); err != nil {
		return nil, err
	}
	src, _ := yamlDocumentVersion(bytes.TrimPrefix([]byte(text), utf8BOM))
	dec := yaml.NewDecoder(bytes.NewReader(src))
	var node yaml.Node
	if err := dec.Decode(&node); err != nil {
		if errors.Is(err, io.EOF) {
			return &RoundtripDoc{Source: data}, nil
		}
		doc, cerr := ry.ComposePure(in, true)
		if cerr != nil {
			return nil, &YAMLError{cerr}
		}
		return &RoundtripDoc{Node: editorDocument(doc.Root), Source: data}, nil
	}
	return &RoundtripDoc{Node: &node, Source: data}, nil
}

var editorStyles = map[rune]yaml.Style{'\'': yaml.SingleQuotedStyle, '"': yaml.DoubleQuotedStyle, '|': yaml.LiteralStyle, '>': yaml.FoldedStyle}

// Editing node tree of a document the round trip loader composed, anchors and aliases kept, positions one based
func editorDocument(root *ry.Node) *yaml.Node {
	doc := &yaml.Node{Kind: yaml.DocumentNode}
	if root == nil {
		return doc
	}
	built := map[*ry.Node]*yaml.Node{}
	var conv func(n *ry.Node) *yaml.Node
	conv = func(n *ry.Node) *yaml.Node {
		if y, ok := built[n]; ok {
			return &yaml.Node{Kind: yaml.AliasNode, Value: y.Anchor, Alias: y, Line: y.Line, Column: y.Column}
		}
		tag := n.Tag()
		if strings.HasPrefix(tag, yamlTagPrefix) {
			tag = "!!" + strings.TrimPrefix(tag, yamlTagPrefix)
		}
		y := &yaml.Node{Tag: tag, Line: n.Start.Line + 1, Column: n.Start.Column + 1, Style: editorStyles[n.Style]}
		if n.Anchor != nil {
			y.Anchor = *n.Anchor
		}
		if n.Handle != nil {
			y.Style |= yaml.TaggedStyle
		}
		built[n] = y
		switch n.Kind {
		case ry.ScalarNode:
			y.Kind, y.Value = yaml.ScalarNode, strings.ReplaceAll(n.Value, "\a", "")
		case ry.SequenceNode:
			y.Kind = yaml.SequenceNode
			if n.Flow {
				y.Style |= yaml.FlowStyle
			}
			for _, it := range n.Items {
				y.Content = append(y.Content, conv(it))
			}
		case ry.MappingNode:
			y.Kind = yaml.MappingNode
			if n.Flow {
				y.Style |= yaml.FlowStyle
			}
			for _, p := range n.Pairs {
				y.Content = append(y.Content, conv(p.Key), conv(p.Value))
			}
		}
		return y
	}
	doc.Content = []*yaml.Node{conv(root)}
	return doc
}

// Python value the document holds under YAML 1.1 rules, the loaded view of the editable data
func (d *RoundtripDoc) Value() (any, error) {
	if d.Node == nil || len(d.Node.Content) == 0 {
		return nil, nil
	}
	text, err := encodeRoundtripNode(d.Node)
	if err != nil {
		return nil, err
	}
	return SafeLoad(text)
}

// Root mapping of the document, created empty for an empty document
func (d *RoundtripDoc) Root() (*yaml.Node, error) {
	if d.Node == nil || len(d.Node.Content) == 0 || (d.Node.Content[0].Kind == yaml.ScalarNode && d.Node.Content[0].Tag == "!!null") {
		d.Node = &yaml.Node{Kind: yaml.DocumentNode, Content: []*yaml.Node{{Kind: yaml.MappingNode, Tag: "!!map"}}}
	}
	root := d.Node.Content[0]
	if root.Kind != yaml.MappingNode {
		var v any
		_ = root.Decode(&v)
		return nil, fmt.Errorf("top-level YAML must be a mapping, got %s", PyTypeName(v))
	}
	return root, nil
}

// Assigns a plain value at one top level key the way a mapping item assignment does
func (d *RoundtripDoc) Set(key string, value any) error {
	root, err := d.Root()
	if err != nil {
		return err
	}
	node, err := rtNode(value, 0)
	if err != nil {
		return err
	}
	mappingSet(root, key, node)
	return nil
}

// Writes the document with two space mappings, indented block sequences and untouched source lines kept
func (r *RoundtripYAML) Dump(doc *RoundtripDoc, w io.Writer) error {
	if doc.Node == nil {
		_, err := io.WriteString(w, "null\n...\n")
		return err
	}
	text, err := encodeRoundtripNode(doc.Node)
	if err != nil {
		return err
	}
	if len(doc.Source) > 0 {
		text = restoreSourceLines(doc.Source, doc.Node, text)
	}
	_, err = io.WriteString(w, text)
	return err
}

// Emitted text of a round trip node with created strings styled the way the ruamel emitter picks
func encodeRoundtripNode(doc *yaml.Node) (string, error) {
	styleCreatedScalars(doc, map[*yaml.Node]bool{})
	var buf strings.Builder
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(doc); err != nil {
		return "", &YAMLError{err}
	}
	if err := enc.Close(); err != nil {
		return "", &YAMLError{err}
	}
	return buf.String(), nil
}

// Quotes str scalars built in memory whose plain text a YAML 1.1 reader would not load back as that str
func styleCreatedScalars(n *yaml.Node, seen map[*yaml.Node]bool) {
	if seen[n] {
		return
	}
	seen[n] = true
	if n.Kind == yaml.ScalarNode && n.Line == 0 && n.Style == 0 && (n.Tag == "!!str" || n.Tag == "") {
		n.Style = roundtripScalarStyle(n.Value)
	}
	for _, c := range n.Content {
		styleCreatedScalars(c, seen)
	}
}

// Style the ruamel emitter chooses for a str in block context, zero for plain
func roundtripScalarStyle(text string) yaml.Style {
	a := analyzeScalar([]rune(text), true)
	if !a.empty && !a.multiline && a.allowBlockPlain && plainResolvesToStr(text) {
		return 0
	}
	if a.allowDouble && (strings.ContainsRune(text, '\'') || strings.ContainsRune(text, '\n')) {
		return yaml.DoubleQuotedStyle
	}
	if a.allowSingle && !a.multiline {
		return yaml.SingleQuotedStyle
	}
	return yaml.DoubleQuotedStyle
}
