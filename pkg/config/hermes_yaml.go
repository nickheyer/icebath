package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

// Parse, construct or represent failure of the shared YAML reader and writer
type YAMLError struct{ Err error }

func (e *YAMLError) Error() string { return e.Err.Error() }

func (e *YAMLError) Unwrap() error { return e.Err }

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
	return nil, fmt.Errorf("'%s' object has no attribute 'read'", PyTypeName(stream))
}

// Value of one YAML document read from text, bytes or a reader with YAML 1.1 booleans
func SafeLoad(stream any) (any, error) {
	data, err := readYAMLStream(stream)
	if err != nil {
		return nil, err
	}
	return loadYAMLDocument(data)
}

// Document bytes rewritten for the parser and whether a YAML 1.2 directive selects 1.2 resolution
func yamlDocumentVersion(src []byte) ([]byte, bool, error) {
	m := yamlDirectiveVersion.FindSubmatchIndex(src)
	if m == nil {
		return src, false, nil
	}
	major, minor := string(src[m[4]:m[5]]), string(src[m[6]:m[7]])
	if major != "1" {
		line := bytes.Count(src[:m[3]], []byte("\n")) + 1
		return nil, false, &YAMLError{fmt.Errorf("yaml: line %d: found incompatible YAML document (version 1.* is required)", line)}
	}
	switch strings.TrimLeft(minor, "0") {
	case "1":
		return src, false, nil
	case "2":
		patched := append(append(append([]byte{}, src[:m[6]]...), '1'), src[m[7]:]...)
		return patched, true, nil
	}
	n := strings.TrimLeft(minor, "0")
	if n == "" {
		n = "0"
	}
	return nil, false, fmt.Errorf("version minor part can only be 2 or 1, got (1, %s)", n)
}

// Single document node of a stream, nil for an empty stream, refusing a second document
func parseSingleYAMLDocument(src []byte) (*yaml.Node, error) {
	dec := yaml.NewDecoder(bytes.NewReader(src))
	var node yaml.Node
	if err := dec.Decode(&node); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, nil
		}
		return nil, &YAMLError{err}
	}
	var extra yaml.Node
	if err := dec.Decode(&extra); err == nil {
		return nil, &YAMLError{fmt.Errorf("yaml: line %d: but found another document", extra.Line)}
	} else if !errors.Is(err, io.EOF) {
		return nil, &YAMLError{err}
	}
	return &node, nil
}

// Tags the safe constructor builds values for, any other explicit tag is refused
var safeConstructorTags = map[string]bool{
	"!!null": true, "!!bool": true, "!!int": true, "!!float": true, "!!binary": true, "!!timestamp": true,
	"!!omap": true, "!!pairs": true, "!!set": true, "!!str": true, "!!seq": true, "!!map": true, "!!merge": true,
}

// Full spelling of a tag the parser shortened to its secondary handle
func longYAMLTag(tag string) string {
	if strings.HasPrefix(tag, "!!") {
		return "tag:yaml.org,2002:" + tag[2:]
	}
	return tag
}

// Refuses explicit tags without a safe constructor and rewrites omap and pairs into the values they build
func checkSafeTags(n *yaml.Node, seen map[*yaml.Node]bool) error {
	if seen[n] {
		return nil
	}
	seen[n] = true
	if n.Kind == yaml.AliasNode {
		return nil
	}
	if n.Style&yaml.TaggedStyle != 0 && !safeConstructorTags[n.Tag] {
		return &YAMLError{fmt.Errorf("yaml: line %d: could not determine a constructor for the tag '%s'", n.Line, longYAMLTag(n.Tag))}
	}
	for _, c := range n.Content {
		if err := checkSafeTags(c, seen); err != nil {
			return err
		}
	}
	switch n.Tag {
	case "!!omap":
		return orderedPairsNode(n, "while constructing an ordered map", true)
	case "!!pairs":
		return orderedPairsNode(n, "while constructing pairs", false)
	}
	return nil
}

// Turns a sequence of one item mappings into a mapping for omap or a list of key value lists for pairs
func orderedPairsNode(n *yaml.Node, context string, omap bool) error {
	if n.Kind != yaml.SequenceNode {
		return &YAMLError{fmt.Errorf("yaml: line %d: %s expected a sequence, but found %s", n.Line, context, yamlNodeID(n))}
	}
	var content []*yaml.Node
	for _, sub := range n.Content {
		if sub.Kind != yaml.MappingNode {
			return &YAMLError{fmt.Errorf("yaml: line %d: %s expected a mapping of length 1, but found %s", sub.Line, context, yamlNodeID(sub))}
		}
		if len(sub.Content) != 2 {
			return &YAMLError{fmt.Errorf("yaml: line %d: %s expected a single mapping item, but found %d items", sub.Line, context, len(sub.Content)/2)}
		}
		if omap {
			content = append(content, sub.Content...)
			continue
		}
		content = append(content, &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq", Content: sub.Content, Line: sub.Line, Column: sub.Column})
	}
	n.Content, n.Style = content, n.Style&^yaml.TaggedStyle
	if omap {
		n.Kind, n.Tag = yaml.MappingNode, "!!map"
	} else {
		n.Tag = "!!seq"
	}
	return nil
}

// Node class name the constructor messages print
func yamlNodeID(n *yaml.Node) string {
	switch n.Kind {
	case yaml.MappingNode:
		return "mapping"
	case yaml.SequenceNode:
		return "sequence"
	}
	return "scalar"
}

// Value of one YAML document under the safe constructor, YAML 1.1 rules unless the document asks for 1.2
func loadYAMLDocument(data []byte) (any, error) {
	src, v12, err := yamlDocumentVersion(bytes.TrimPrefix(data, utf8BOM))
	if err != nil {
		return nil, err
	}
	node, err := parseSingleYAMLDocument(src)
	if err != nil || node == nil || len(node.Content) == 0 {
		return nil, err
	}
	if err := checkSafeTags(node, map[*yaml.Node]bool{}); err != nil {
		return nil, err
	}
	resolveYAMLVersion(node, false, v12)
	if err := yaml11TimestampError(node); err != nil {
		return nil, err
	}
	var doc any
	if err := node.Decode(&doc); err != nil {
		return nil, &YAMLError{err}
	}
	doc = normalizeMaps(doc)
	recordNodeOrder(node, doc)
	return doc, nil
}

// Duplicate key refusal of a round trip mapping, keys compared by the value they construct
func checkDuplicateKeys(n *yaml.Node, seen map[*yaml.Node]bool) error {
	if seen[n] {
		return nil
	}
	seen[n] = true
	if n.Kind == yaml.MappingNode {
		first := map[string]*yaml.Node{}
		for i := 0; i+1 < len(n.Content); i += 2 {
			k := n.Content[i]
			if k.Kind != yaml.ScalarNode || (k.Tag == "!!merge" && k.Value == "<<") {
				continue
			}
			id := constructedKeyID(k)
			if prev, ok := first[id]; ok {
				return &YAMLError{fmt.Errorf("yaml: line %d: found duplicate key %q with value %q (original value: %q)", k.Line, k.Value, scalarText(n.Content[i+1]), scalarText(n.Content[mappingIndexOf(n, prev)]))}
			}
			first[id] = k
		}
	}
	for _, c := range n.Content {
		if err := checkDuplicateKeys(c, seen); err != nil {
			return err
		}
	}
	return nil
}

// Index of the value node that follows a key node in a mapping
func mappingIndexOf(n *yaml.Node, key *yaml.Node) int {
	for i := 0; i+1 < len(n.Content); i += 2 {
		if n.Content[i] == key {
			return i + 1
		}
	}
	return 0
}

func scalarText(n *yaml.Node) string {
	if n.Kind == yaml.ScalarNode {
		return n.Value
	}
	return ""
}

// Equality class of a scalar key the way Python compares constructed keys, so true and 1 collide
func constructedKeyID(k *yaml.Node) string {
	if k.Style != 0 && k.Style&yaml.TaggedStyle == 0 {
		return "s" + k.Value
	}
	probe := &yaml.Node{Kind: yaml.ScalarNode, Tag: k.Tag, Value: k.Value, Style: k.Style}
	resolveYAMLVersion(probe, false, false)
	switch probe.Tag {
	case "!!bool":
		if probe.Value == "true" {
			return "n1"
		}
		return "n0"
	case "!!int":
		return "n" + probe.Value
	case "!!null":
		return "null"
	case "!!float":
		var f float64
		if probe.Decode(&f) == nil && f == float64(int64(f)) {
			return fmt.Sprintf("n%d", int64(f))
		}
		return "f" + probe.Value
	}
	return "s" + k.Value
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

// Parses a document for editing, refusing duplicate keys and further documents, nil node for an empty stream
func (r *RoundtripYAML) Load(stream any) (*RoundtripDoc, error) {
	data, err := readYAMLStream(stream)
	if err != nil {
		return nil, err
	}
	src, _, err := yamlDocumentVersion(bytes.TrimPrefix(data, utf8BOM))
	if err != nil {
		return nil, err
	}
	node, err := parseSingleYAMLDocument(src)
	if err != nil {
		return nil, err
	}
	if node != nil {
		if err := checkDuplicateKeys(node, map[*yaml.Node]bool{}); err != nil {
			return nil, err
		}
	}
	return &RoundtripDoc{Node: node, Source: data}, nil
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
	return loadYAMLDocument([]byte(text))
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
