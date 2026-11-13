package config

import (
	"errors"
	"fmt"
	"io"
	"math"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

	"gopkg.in/yaml.v3"
)

// Parse failure of a YAML document at the line the parser names, zero when it names none
type YAMLError struct {
	Line    int
	Problem string
}

// Line and problem in one line
func (e *YAMLError) Error() string {
	if e.Line > 0 {
		return fmt.Sprintf("line %d: %s", e.Line, e.Problem)
	}
	return e.Problem
}

// Line and problem of a yaml.v3 parse error text, which leaves out the first line number
var parserErrorText = regexp.MustCompile(`^yaml: (?:line (\d+): )?(.*)$`)

// Parse failure of yaml.v3 as a YAMLError, a missing line meaning the first line
func yamlErrorOf(err error) error {
	m := parserErrorText.FindStringSubmatch(err.Error())
	if m == nil {
		return &YAMLError{Problem: err.Error()}
	}
	line := 1
	if m[1] != "" {
		line, _ = strconv.Atoi(m[1])
	}
	return &YAMLError{Line: line, Problem: m[2]}
}

// Document text with a leading byte order mark dropped and every CRLF or CR read as LF, refusing invalid UTF-8
func yamlText(data []byte) (string, error) {
	text := strings.TrimPrefix(string(data), "\xef\xbb\xbf")
	if !utf8.ValidString(text) {
		return "", &YAMLError{Problem: fmt.Sprintf("invalid UTF-8 byte at offset %d", firstInvalidByte(text))}
	}
	return strings.ReplaceAll(strings.ReplaceAll(text, "\r\n", "\n"), "\r", "\n"), nil
}

// Byte offset of the first invalid UTF-8 sequence
func firstInvalidByte(s string) int {
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		if r == utf8.RuneError && size <= 1 {
			return i
		}
		i += size
	}
	return len(s)
}

// Document node of the single YAML document in text, nil when there is none, refusing more documents and duplicate keys
func parseYAMLText(text string) (*yaml.Node, error) {
	dec := yaml.NewDecoder(strings.NewReader(text))
	var doc yaml.Node
	if err := dec.Decode(&doc); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, nil
		}
		return nil, yamlErrorOf(err)
	}
	var extra yaml.Node
	switch err := dec.Decode(&extra); {
	case err == nil:
		return nil, &YAMLError{Line: extra.Line, Problem: "expected a single document in the stream but found another document"}
	case !errors.Is(err, io.EOF):
		return nil, yamlErrorOf(err)
	}
	if err := refuseDuplicateKeys(&doc, map[*yaml.Node]bool{}); err != nil {
		return nil, err
	}
	if err := refuseUnconstructible(&doc, map[*yaml.Node]bool{}); err != nil {
		return nil, err
	}
	if len(doc.Content) == 0 {
		return nil, nil
	}
	return &doc, nil
}

// Tags the safe constructor builds a value for, any other explicit tag failing the load
func safeTags() map[string]bool {
	return map[string]bool{tagNull: true, tagBool: true, tagInt: true, tagFloat: true, "!!binary": true, "!!timestamp": true,
		"!!omap": true, "!!pairs": true, "!!set": true, tagStr: true, tagSeq: true, tagMap: true, mergeTag: true}
}

// First node below n the safe constructor cannot build, an unknown tag, a malformed ordered map or an impossible date
func refuseUnconstructible(n *yaml.Node, seen map[*yaml.Node]bool) error {
	if n == nil || seen[n] || n.Kind == yaml.AliasNode {
		return nil
	}
	seen[n] = true
	tagged := n.Style&yaml.TaggedStyle != 0
	if tagged && !safeTags()[n.ShortTag()] {
		tag := n.Tag
		if rest, ok := strings.CutPrefix(tag, "!!"); ok {
			tag = "tag:yaml.org,2002:" + rest
		}
		return &YAMLError{Line: n.Line, Problem: fmt.Sprintf("could not determine a constructor for the tag '%s'", tag)}
	}
	switch tag := n.ShortTag(); {
	case tagged && (tag == "!!omap" || tag == "!!pairs"):
		if err := orderedPairsShape(n, tag == "!!omap"); err != nil {
			return err
		}
	case n.Kind == yaml.ScalarNode && !tagged && n.Style&fixedStyles == 0 && n.Value == "=":
		return &YAMLError{Line: n.Line, Problem: "could not determine a constructor for the tag 'tag:yaml.org,2002:value'"}
	case n.Kind == yaml.ScalarNode && (tag == "!!timestamp" || (!tagged && n.Style&fixedStyles == 0)):
		if err := timestampProblem(n.Value); err != nil {
			return &YAMLError{Problem: err.Error()}
		}
	}
	for _, c := range n.Content {
		if err := refuseUnconstructible(c, seen); err != nil {
			return err
		}
	}
	return nil
}

// Failure of an ordered map or pairs node that is not a sequence of single entry mappings
func orderedPairsShape(n *yaml.Node, omap bool) error {
	what := "while constructing pairs"
	if omap {
		what = "while constructing an ordered map"
	}
	if n.Kind != yaml.SequenceNode {
		return &YAMLError{Line: n.Line, Problem: fmt.Sprintf("%s expected a sequence, but found %s", what, nodeID(n))}
	}
	for _, sub := range n.Content {
		sub = resolveAlias(sub)
		switch {
		case sub.Kind != yaml.MappingNode:
			return &YAMLError{Line: sub.Line, Problem: fmt.Sprintf("%s expected a mapping of length 1, but found %s", what, nodeID(sub))}
		case len(sub.Content) != 2:
			return &YAMLError{Line: sub.Line, Problem: fmt.Sprintf("%s expected a single mapping item, but found %d items", what, len(sub.Content)/2)}
		}
	}
	return nil
}

// Node class name constructor messages print
func nodeID(n *yaml.Node) string {
	switch n.Kind {
	case yaml.MappingNode:
		return "mapping"
	case yaml.SequenceNode:
		return "sequence"
	}
	return "scalar"
}

// Date and time a YAML 1.1 timestamp spells, with the optional fraction and zone
var timestampText = regexp.MustCompile(`^([0-9]{4})-([0-9]{1,2})-([0-9]{1,2})(?:(?:[Tt]|[ \t]+)([0-9]{1,2}):([0-9]{2}):([0-9]{2})(?:\.([0-9]*))?(?:[ \t]*(Z|[-+][0-9]{1,2}(?::[0-9]{2})?))?)?$`)

// Range failure of a scalar spelled as a YAML 1.1 timestamp naming no real date or time, nil otherwise
func timestampProblem(s string) error {
	m := timestampText.FindStringSubmatch(s)
	if m == nil || (m[4] == "" && (len(m[2]) != 2 || len(m[3]) != 2)) {
		return nil
	}
	num := func(t string) int { v, _ := strconv.Atoi(t); return v }
	year, month, day := num(m[1]), num(m[2]), num(m[3])
	switch {
	case year < 1:
		return fmt.Errorf("year %d is out of range", year)
	case month < 1 || month > 12:
		return errors.New("month must be in 1..12")
	case day < 1 || day > daysIn(year, month):
		return errors.New("day is out of range for month")
	}
	if m[4] == "" {
		return nil
	}
	switch {
	case num(m[4]) > 23:
		return errors.New("hour must be in 0..23")
	case num(m[5]) > 59:
		return errors.New("minute must be in 0..59")
	case num(m[6]) > 59:
		return errors.New("second must be in 0..59")
	}
	return nil
}

// Days of a month in the proleptic Gregorian calendar
func daysIn(year, month int) int {
	switch month {
	case 2:
		if year%4 == 0 && (year%100 != 0 || year%400 == 0) {
			return 29
		}
		return 28
	case 4, 6, 9, 11:
		return 30
	}
	return 31
}

// First key written twice in one mapping below n, keys compared by the value they construct
func refuseDuplicateKeys(n *yaml.Node, seen map[*yaml.Node]bool) error {
	if n == nil || seen[n] || n.Kind == yaml.AliasNode {
		return nil
	}
	seen[n] = true
	if n.Kind == yaml.MappingNode {
		first := map[string]int{}
		for i := 0; i+1 < len(n.Content); i += 2 {
			k := resolveAlias(n.Content[i])
			if k == nil {
				continue
			}
			if k.Kind == yaml.ScalarNode && k.ShortTag() == mergeTag {
				continue
			}
			if k.Kind != yaml.ScalarNode {
				return &YAMLError{Line: k.Line, Problem: "found unhashable key"}
			}
			id := keyIdentity(k)
			if at, dup := first[id]; dup {
				return &YAMLError{Line: k.Line, Problem: fmt.Sprintf("found duplicate key \"%s\" with value \"%s\" (original value: \"%s\")",
					nodeText(k), nodeText(n.Content[i+1]), nodeText(n.Content[at+1]))}
			}
			first[id] = i
		}
	}
	for _, c := range n.Content {
		if err := refuseDuplicateKeys(c, seen); err != nil {
			return err
		}
	}
	return nil
}

// Equality class of a key under YAML 1.1 construction, so true and 1 or 1 and 1.0 collide
func keyIdentity(k *yaml.Node) string {
	tag, text := scalarType(k)
	switch tag {
	case tagBool:
		v, _ := resolveTagged(tagBool, text)
		if v == "true" {
			return "n1"
		}
		return "n0"
	case tagInt:
		if v, ok := constructInt(text); ok {
			return "n" + v.String()
		}
	case tagFloat:
		if f, ok := constructFloat(text); ok && f == math.Trunc(f) && math.Abs(f) < 1<<53 {
			return "n" + strconv.FormatInt(int64(f), 10)
		}
	case tagNull:
		return "null"
	}
	return tag + "\x00" + text
}

// Text of a node in a diagnostic, the value it builds formatted as a string
func nodeText(n *yaml.Node) string {
	v, err := valueTree(n)
	if err != nil {
		return n.Value
	}
	return displayText(v)
}

// Value tree of a parsed node, YAML 1.1 typed with aliases expanded within the alias budget and merge keys folded
func valueTree(n *yaml.Node) (*yaml.Node, error) {
	r, err := resolved(n)
	if err != nil {
		return nil, err
	}
	return foldOrdered(plainCopy(r)), nil
}

// Tree with ordered maps folded into mappings and pairs into lists of two item lists, as the safe constructor builds
func foldOrdered(n *yaml.Node) *yaml.Node {
	for _, c := range n.Content {
		foldOrdered(c)
	}
	if n.Kind != yaml.SequenceNode || n.Style&yaml.TaggedStyle == 0 {
		return n
	}
	switch n.ShortTag() {
	case "!!omap":
		var content []*yaml.Node
		for _, sub := range n.Content {
			content = append(content, sub.Content...)
		}
		n.Kind, n.Tag, n.Content, n.Style = yaml.MappingNode, tagMap, content, n.Style&^yaml.TaggedStyle
	case "!!pairs":
		for i, sub := range n.Content {
			n.Content[i] = &yaml.Node{Kind: yaml.SequenceNode, Tag: tagSeq, Content: sub.Content}
		}
		n.Tag, n.Style = tagSeq, n.Style&^yaml.TaggedStyle
	}
	return n
}

// Decodes the single document of data into v under the product YAML dialect, leaving v untouched for an empty document
func LoadYAML(data []byte, v any) error {
	text, err := yamlText(data)
	if err != nil {
		return err
	}
	doc, err := parseYAMLText(text)
	if err != nil || doc == nil {
		return err
	}
	tree, err := valueTree(doc.Content[0])
	if err != nil {
		return err
	}
	if target, ok := v.(*yaml.Node); ok {
		*target = *tree
		return nil
	}
	rv := reflect.ValueOf(v)
	if rv.Kind() != reflect.Pointer || rv.IsNil() {
		return fmt.Errorf("%w: decode target must be a non-nil pointer", ErrShape)
	}
	_, err = decodeValue(tree, rv.Elem(), coerceDefault)
	return err
}

// Reads the stream and decodes its single document into v under the product YAML dialect
func LoadYAMLFrom(r io.Reader, v any) error {
	data, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	return LoadYAML(data, v)
}

// Text the product YAML dialect writes for v, block style with keys in the order given, folding at eighty columns
func DumpYAML(v any) ([]byte, error) {
	n, err := nodeFor(v)
	if err != nil {
		return nil, err
	}
	return []byte(safeStyle().renderDocument(n)), nil
}

// Value tree for a Go value, a node copied as it is and anything else encoded field by field
func nodeFor(v any) (*yaml.Node, error) {
	switch t := v.(type) {
	case *yaml.Node:
		if t == nil {
			return nullNode(), nil
		}
		if t.Kind == yaml.DocumentNode {
			if len(t.Content) == 0 {
				return nullNode(), nil
			}
			return plainCopy(t.Content[0]), nil
		}
		return plainCopy(t), nil
	case yaml.Node:
		return nodeFor(&t)
	}
	n, err := encodeValue(reflect.ValueOf(v))
	if err != nil {
		return nil, err
	}
	return plainCopy(n), nil
}
