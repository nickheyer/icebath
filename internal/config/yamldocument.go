package config

import (
	"fmt"
	"reflect"

	"gopkg.in/yaml.v3"
)

// Document of the product YAML dialect that keeps its source, so writing it back changes only the values edited
type YAMLDocument struct {
	raw     string
	src     string
	doc     *yaml.Node
	root    *yaml.Node
	readErr error
}

// Parses data as one editable document, refusing a second document, duplicate keys, unbuildable values and excessive aliasing
func ParseYAMLDocument(data []byte) (*YAMLDocument, error) {
	text, err := yamlText(data)
	if err != nil {
		return nil, err
	}
	doc, err := parseYAMLText(text)
	if err != nil {
		return nil, err
	}
	d := &YAMLDocument{raw: string(data), src: text, doc: doc}
	if doc != nil {
		if d.root, err = valueTree(doc.Content[0]); err != nil {
			return nil, err
		}
	}
	return d, nil
}

// Document with no source holding an empty mapping
func NewYAMLDocument() *YAMLDocument {
	return &YAMLDocument{root: emptyMapping()}
}

// Independent copy whose edits leave the receiver as it is
func (d *YAMLDocument) Clone() *YAMLDocument {
	c := *d
	if d.root != nil {
		c.root = plainCopy(d.root)
	}
	return &c
}

// Failure of the read this document stands in for, nil when it was read normally, a document writers refuse
func (d *YAMLDocument) ReadError() error { return d.readErr }

// Copy of the value tree, YAML 1.1 typed with aliases expanded, nil for an empty document
func (d *YAMLDocument) Node() *yaml.Node {
	if d.root == nil {
		return nil
	}
	return plainCopy(d.root)
}

// Reports a document whose top level is a mapping, an empty document counting as an empty mapping
func (d *YAMLDocument) IsMapping() bool {
	return d.root == nil || isNull(d.root) || d.root.Kind == yaml.MappingNode
}

// Decodes the document into the value v points at, leaving it untouched for an empty document
func (d *YAMLDocument) Decode(v any) error {
	if d.root == nil {
		return nil
	}
	if target, ok := v.(*yaml.Node); ok {
		*target = *plainCopy(d.root)
		return nil
	}
	rv := reflect.ValueOf(v)
	if rv.Kind() != reflect.Pointer || rv.IsNil() {
		return fmt.Errorf("%w: decode target must be a non-nil pointer", ErrShape)
	}
	_, err := decodeValue(d.root, rv.Elem(), coerceDefault)
	return err
}

// Copy of the value at a dotted path, literal keys holding dots matched before the path is split further
func (d *YAMLDocument) Get(path string) (*yaml.Node, bool) {
	n, ok := getNested(d.root, path)
	if !ok {
		return nil, false
	}
	return plainCopy(n), true
}

// Sets the value at a dotted path, creating mappings on the way, a node copied and other values encoded
func (d *YAMLDocument) Set(path string, value any) error {
	n, err := nodeFor(value)
	if err != nil {
		return err
	}
	if d.root == nil || isNull(d.root) {
		d.root = emptyMapping()
	}
	return setNested(d.root, path, n)
}

// Removes the value at a dotted path and the mappings it leaves empty, reporting whether it existed
func (d *YAMLDocument) Unset(path string) bool {
	return unsetNested(d.root, path)
}

// Bytes to write as they are, the source kept byte for byte with its line endings except where values changed
func (d *YAMLDocument) Render() ([]byte, error) {
	return d.render(roundTripStyle())
}

// Bytes of the document with new values written under style
func (d *YAMLDocument) render(style yamlStyle) ([]byte, error) {
	if d.doc == nil && d.root == nil {
		return []byte(d.raw), nil
	}
	out, err := spliceDocument(d.raw, d.src, d.doc, d.root, style)
	if err != nil {
		return nil, err
	}
	return []byte(out), nil
}

// Document holding the value tree root written over the source of the receiver
func (d *YAMLDocument) withRoot(root *yaml.Node) *YAMLDocument {
	return &YAMLDocument{raw: d.raw, src: d.src, doc: d.doc, root: root}
}
