package tools

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"math"
	"math/big"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"gopkg.in/yaml.v3"
)

// Implicit scalar resolvers of one YAML version in the order ruamel registers them
type yamlResolvers struct {
	version                 [2]int
	boolRe, floatRe, intRe  *regexp.Regexp
	mergeRe, nullRe, timeRe *regexp.Regexp
	valueRe                 *regexp.Regexp
}

const pyDollar = `\n?\z`

var (
	yamlCommonMergeRe = regexp.MustCompile(`\A<<` + pyDollar)
	yamlCommonNullRe  = regexp.MustCompile(`\A(?:~|null|Null|NULL|)` + pyDollar)
	yamlCommonTimeRe  = regexp.MustCompile(`\A(?:[0-9]{4}-[0-9][0-9]-[0-9][0-9]|[0-9]{4}-[0-9][0-9]?-[0-9][0-9]?(?:[Tt]|[ \t]+)[0-9][0-9]?:[0-9][0-9]:[0-9][0-9](?:\.[0-9]*)?(?:[ \t]*(?:Z|[-+][0-9][0-9]?(?::[0-9][0-9])?))?)` + pyDollar)
	yamlCommonValueRe = regexp.MustCompile(`\A=` + pyDollar)

	yaml11Resolvers = yamlResolvers{
		version: [2]int{1, 1},
		boolRe:  regexp.MustCompile(`\A(?:y|Y|yes|Yes|YES|n|N|no|No|NO|true|True|TRUE|false|False|FALSE|on|On|ON|off|Off|OFF)` + pyDollar),
		floatRe: regexp.MustCompile(`\A(?:[-+]?(?:[0-9][0-9_]*)\.[0-9_]*(?:[eE][-+]?[0-9]+)?|[-+]?(?:[0-9][0-9_]*)(?:[eE][-+]?[0-9]+)|\.[0-9_]+(?:[eE][-+][0-9]+)?|[-+]?[0-9][0-9_]*(?::[0-5]?[0-9])+\.[0-9_]*|[-+]?\.(?:inf|Inf|INF)|\.(?:nan|NaN|NAN))` + pyDollar),
		intRe:   regexp.MustCompile(`\A(?:[-+]?0b[0-1_]+|[-+]?0?[0-7_]+|[-+]?(?:0|[1-9][0-9_]*)|[-+]?0x[0-9a-fA-F_]+|[-+]?[1-9][0-9_]*(?::[0-5]?[0-9])+)` + pyDollar),
		mergeRe: yamlCommonMergeRe, nullRe: yamlCommonNullRe, timeRe: yamlCommonTimeRe, valueRe: yamlCommonValueRe,
	}
	yaml12Resolvers = yamlResolvers{
		version: [2]int{1, 2},
		boolRe:  regexp.MustCompile(`\A(?:true|True|TRUE|false|False|FALSE)` + pyDollar),
		floatRe: regexp.MustCompile(`\A(?:[-+]?(?:[0-9][0-9_]*)\.[0-9_]*(?:[eE][-+]?[0-9]+)?|[-+]?(?:[0-9][0-9_]*)(?:[eE][-+]?[0-9]+)|[-+]?\.[0-9_]+(?:[eE][-+][0-9]+)?|[-+]?\.(?:inf|Inf|INF)|\.(?:nan|NaN|NAN))` + pyDollar),
		intRe:   regexp.MustCompile(`\A(?:[-+]?0b[0-1_]+|[-+]?0o?[0-7_]+|[-+]?[0-9_]+|[-+]?0x[0-9a-fA-F_]+)` + pyDollar),
		mergeRe: yamlCommonMergeRe, nullRe: yamlCommonNullRe, timeRe: yamlCommonTimeRe, valueRe: yamlCommonValueRe,
	}

	yamlTimestampRe  = regexp.MustCompile(`\A([0-9]{4})-([0-9][0-9]?)-([0-9][0-9]?)(?:(?:[Tt]|[ \t]+)([0-9][0-9]?):([0-9][0-9]):([0-9][0-9])(?:\.([0-9]*))?(?:[ \t]*(Z|([-+])([0-9][0-9]?)(?::([0-9][0-9]))?))?)?` + pyDollar)
	yamlDirectiveRe  = regexp.MustCompile(`\A((?:[ \t\r]*(?:#[^\n]*)?\n|%[^\n]*\n)*?)%YAML[ \t]+([0-9]+)\.([0-9]+)`)
	pyFloatTextRe    = regexp.MustCompile(`\A[+-]?(?:(?:[0-9]+\.?[0-9]*|\.[0-9]+)(?:e[+-]?[0-9]+)?|inf|infinity|nan)\z`)
	yamlBoolValues   = map[string]bool{"yes": true, "no": false, "y": true, "n": false, "true": true, "false": false, "on": true, "off": false}
	errYAMLDuplicate = errors.New("while constructing a mapping found duplicate key")
)

// Python value kinds the safe loader builds that the probe only needs to tell apart
type (
	yamlDate struct{ year, month, day int }
	yamlTime struct {
		t     time.Time
		aware bool
	}
	yamlSet      struct{}
	yamlTuple    []any
	yamlNaN      struct{ id int }
	yamlCycleRef struct{}
)

// One document loaded with the hermes YAML policy, ruamel safe types under YAML 1.1 rules, an error where it raises
func hermesYAMLSafeLoad(text string) (any, error) {
	src := []byte(text)
	res := yaml11Resolvers
	if m := yamlDirectiveRe.FindSubmatchIndex(src); m != nil {
		major, minor := string(src[m[4]:m[5]]), string(src[m[6]:m[7]])
		switch {
		case major == "1" && minor == "1":
		case major == "1" && minor == "2":
			if ruamelCParseError(text) != "" {
				res = yaml12Resolvers
			}
			patched := append([]byte{}, src[:m[6]]...)
			patched = append(patched, '1')
			src = append(patched, src[m[7]:]...)
		default:
			return nil, fmt.Errorf("found incompatible YAML document (version %s.%s)", major, minor)
		}
	}
	dec := yaml.NewDecoder(bytes.NewReader(src))
	var doc yaml.Node
	if err := dec.Decode(&doc); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, nil
		}
		return nil, err
	}
	var extra yaml.Node
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		if err != nil {
			return nil, err
		}
		return nil, errors.New("expected a single document in the stream")
	}
	if len(doc.Content) == 0 {
		return nil, nil
	}
	c := &yamlConstructor{res: res, src: src, built: map[*yaml.Node]any{}, building: map[*yaml.Node]bool{}}
	return c.object(doc.Content[0])
}

// Builder of Python values from a node tree, caching each node the way constructed_objects does
type yamlConstructor struct {
	res      yamlResolvers
	src      []byte
	built    map[*yaml.Node]any
	building map[*yaml.Node]bool
	nans     int
}

// Node an alias points at, the node itself otherwise
func yamlDeref(n *yaml.Node) *yaml.Node {
	for n != nil && n.Kind == yaml.AliasNode {
		n = n.Alias
	}
	return n
}

// Tag the composer gives a node, explicit tags kept and implicit ones resolved under the loader's version
func (c *yamlConstructor) tag(n *yaml.Node) string {
	if n.Style&yaml.TaggedStyle != 0 {
		return n.Tag
	}
	switch n.Kind {
	case yaml.MappingNode:
		return "!!map"
	case yaml.SequenceNode:
		return "!!seq"
	}
	if n.Style&(yaml.DoubleQuotedStyle|yaml.SingleQuotedStyle|yaml.LiteralStyle|yaml.FoldedStyle) != 0 && !yamlNonSpecificTag(c.src, n) {
		return "!!str"
	}
	v := n.Value
	switch {
	case c.res.boolRe.MatchString(v):
		return "!!bool"
	case c.res.floatRe.MatchString(v):
		return "!!float"
	case c.res.intRe.MatchString(v):
		return "!!int"
	case c.res.mergeRe.MatchString(v):
		return "!!merge"
	case c.res.nullRe.MatchString(v):
		return "!!null"
	case c.res.timeRe.MatchString(v):
		return "!!timestamp"
	case c.res.valueRe.MatchString(v):
		return "!!value"
	}
	return "!!str"
}

// Reports whether a quoted or block scalar carries the non-specific tag among the properties its position starts at
func yamlNonSpecificTag(src []byte, n *yaml.Node) bool {
	pos := yamlOffset(src, n.Line, n.Column)
	if pos < 0 {
		return false
	}
	for pos < len(src) {
		end := pos
		for end < len(src) && !strings.ContainsRune(" \t\r\n,[]{}", rune(src[end])) {
			end++
		}
		tok := string(src[pos:end])
		switch {
		case tok == "!":
			return true
		case len(tok) > 1 && (tok[0] == '&' || tok[0] == '!'):
			pos = yamlSkipBlank(src, end)
		default:
			return false
		}
	}
	return false
}

// Position after the blanks, line breaks and comments that follow a position
func yamlSkipBlank(src []byte, pos int) int {
	for pos < len(src) {
		switch c := src[pos]; {
		case c == ' ' || c == '\t' || c == '\r' || c == '\n':
			pos++
		case c == '#':
			for pos < len(src) && src[pos] != '\n' && src[pos] != '\r' {
				pos++
			}
		default:
			return pos
		}
	}
	return pos
}

// Byte offset of a one based line and character column the way the scanner counts them, -1 past the end
func yamlOffset(src []byte, line, column int) int {
	off := 0
	for l := 1; l < line; l++ {
		for {
			if off >= len(src) {
				return -1
			}
			r, size := utf8.DecodeRune(src[off:])
			off += size
			if r == '\r' && off < len(src) && src[off] == '\n' {
				off++
			}
			if r == '\r' || r == '\n' || r == 0x85 || r == 0x2028 || r == 0x2029 {
				break
			}
		}
	}
	for col := 1; col < column; col++ {
		if off >= len(src) {
			return -1
		}
		_, size := utf8.DecodeRune(src[off:])
		off += size
	}
	return off
}

// Python value of a node, an alias to a node still being built answering with a cycle reference
func (c *yamlConstructor) object(n *yaml.Node) (any, error) {
	n = yamlDeref(n)
	if v, ok := c.built[n]; ok {
		return v, nil
	}
	if c.building[n] {
		return yamlCycleRef{}, nil
	}
	c.building[n] = true
	v, err := c.construct(n)
	delete(c.building, n)
	if err != nil {
		return nil, err
	}
	c.built[n] = v
	return v, nil
}

// Value of one node through the safe constructor its tag selects
func (c *yamlConstructor) construct(n *yaml.Node) (any, error) {
	tag := c.tag(n)
	switch tag {
	case "!!null":
		if _, err := c.scalar(n); err != nil {
			return nil, err
		}
		return nil, nil
	case "!!bool":
		s, err := c.scalar(n)
		if err != nil {
			return nil, err
		}
		b, ok := yamlBoolValues[strings.ToLower(s)]
		if !ok {
			return nil, fmt.Errorf("no boolean value %q", s)
		}
		return b, nil
	case "!!int":
		s, err := c.scalar(n)
		if err != nil {
			return nil, err
		}
		return c.yamlInt(s)
	case "!!float":
		s, err := c.scalar(n)
		if err != nil {
			return nil, err
		}
		return c.yamlFloat(s)
	case "!!binary":
		s, err := c.scalar(n)
		if err != nil {
			return nil, err
		}
		for _, r := range s {
			if r > 0x7f {
				return nil, errors.New("failed to convert base64 data into ascii")
			}
		}
		return pyA2BBase64(s)
	case "!!timestamp":
		if n.Kind != yaml.ScalarNode {
			return nil, errors.New("failed to construct timestamp")
		}
		return yamlTimestamp(n.Value)
	case "!!str":
		return c.scalar(n)
	case "!!seq":
		if n.Kind != yaml.SequenceNode {
			return nil, errors.New("expected a sequence node")
		}
		items := make([]any, 0, len(n.Content))
		for _, child := range n.Content {
			v, err := c.object(child)
			if err != nil {
				return nil, err
			}
			items = append(items, v)
		}
		return items, nil
	case "!!map":
		return c.mapping(n)
	case "!!set":
		if _, err := c.mapping(n); err != nil {
			return nil, err
		}
		return yamlSet{}, nil
	case "!!omap", "!!pairs":
		return c.pairs(n, tag == "!!omap")
	}
	return nil, fmt.Errorf("could not determine a constructor for the tag %q", tag)
}

// Scalar text of a node, a mapping answering through its value key, an error for any other collection
func (c *yamlConstructor) scalar(n *yaml.Node) (string, error) {
	n = yamlDeref(n)
	if n.Kind == yaml.MappingNode {
		for i := 0; i+1 < len(n.Content); i += 2 {
			if k := yamlDeref(n.Content[i]); k.Kind == yaml.ScalarNode && c.tag(k) == "!!value" {
				return c.scalar(n.Content[i+1])
			}
		}
	}
	if n.Kind != yaml.ScalarNode {
		return "", errors.New("expected a scalar node")
	}
	return n.Value, nil
}

// Key value pairs of a mapping with merge keys flattened in front, true when a merge contributed any pair
func (c *yamlConstructor) flatten(n *yaml.Node) ([][2]*yaml.Node, bool, error) {
	var merge, own [][2]*yaml.Node
	for i := 0; i+1 < len(n.Content); i += 2 {
		k, v := yamlDeref(n.Content[i]), yamlDeref(n.Content[i+1])
		if k.Kind != yaml.ScalarNode || c.tag(k) != "!!merge" {
			own = append(own, [2]*yaml.Node{k, v})
			continue
		}
		if len(merge) > 0 {
			return nil, false, errors.New("found duplicate merge key")
		}
		switch v.Kind {
		case yaml.MappingNode:
			pairs, _, err := c.flatten(v)
			if err != nil {
				return nil, false, err
			}
			merge = append(merge, pairs...)
		case yaml.SequenceNode:
			var sub [][][2]*yaml.Node
			for _, s := range v.Content {
				s = yamlDeref(s)
				if s.Kind != yaml.MappingNode {
					return nil, false, errors.New("expected a mapping for merging")
				}
				pairs, _, err := c.flatten(s)
				if err != nil {
					return nil, false, err
				}
				sub = append(sub, pairs)
			}
			for i := len(sub) - 1; i >= 0; i-- {
				merge = append(merge, sub[i]...)
			}
		default:
			return nil, false, errors.New("expected a mapping or list of mappings for merging")
		}
	}
	return append(merge, own...), len(merge) > 0, nil
}

// Python dict of a mapping node with merges applied and duplicate keys refused, keeping its string keys
func (c *yamlConstructor) mapping(n *yaml.Node) (map[string]any, error) {
	if n.Kind != yaml.MappingNode {
		return nil, errors.New("expected a mapping node")
	}
	pairs, merged, err := c.flatten(n)
	if err != nil {
		return nil, err
	}
	out := map[string]any{}
	seen := map[string]bool{}
	for _, p := range pairs {
		var key any
		if p[0].Kind == yaml.ScalarNode && c.tag(p[0]) == "!!value" {
			key = p[0].Value
		} else if key, err = c.object(p[0]); err != nil {
			return nil, err
		}
		if list, ok := key.([]any); ok {
			key = yamlTuple(list)
		}
		id, err := c.keyID(key)
		if err != nil {
			return nil, err
		}
		value, err := c.object(p[1])
		if err != nil {
			return nil, err
		}
		if !merged {
			if seen[id] {
				return nil, errYAMLDuplicate
			}
			seen[id] = true
		}
		if s, ok := key.(string); ok {
			out[s] = value
		}
	}
	return out, nil
}

// Ordered map or pairs from a sequence of single entry mappings, an ordered map refusing a repeated key
func (c *yamlConstructor) pairs(n *yaml.Node, omap bool) (any, error) {
	if n.Kind != yaml.SequenceNode {
		return nil, errors.New("expected a sequence")
	}
	out := map[string]any{}
	var list []any
	seen := map[string]bool{}
	for _, s := range n.Content {
		s = yamlDeref(s)
		if s.Kind != yaml.MappingNode {
			return nil, errors.New("expected a mapping of length 1")
		}
		if len(s.Content) != 2 {
			return nil, errors.New("expected a single mapping item")
		}
		key, err := c.object(s.Content[0])
		if err != nil {
			return nil, err
		}
		if omap {
			id, err := c.keyID(key)
			if err != nil {
				return nil, err
			}
			if seen[id] {
				return nil, errYAMLDuplicate
			}
			seen[id] = true
		}
		value, err := c.object(s.Content[1])
		if err != nil {
			return nil, err
		}
		if k, ok := key.(string); ok && omap {
			out[k] = value
		}
		list = append(list, yamlTuple{key, value})
	}
	if omap {
		return out, nil
	}
	return list, nil
}

// Identity of a hashable key under Python equality, numbers of equal value sharing one, an error when unhashable
func (c *yamlConstructor) keyID(v any) (string, error) {
	switch t := v.(type) {
	case nil:
		return "none", nil
	case bool:
		if t {
			return "n:1", nil
		}
		return "n:0", nil
	case *big.Int:
		return "n:" + t.String(), nil
	case float64:
		switch {
		case math.IsInf(t, 1):
			return "f:+inf", nil
		case math.IsInf(t, -1):
			return "f:-inf", nil
		case t == math.Trunc(t):
			i, _ := new(big.Float).SetFloat64(t).Int(nil)
			return "n:" + i.String(), nil
		}
		return "f:" + strconv.FormatFloat(t, 'g', -1, 64), nil
	case yamlNaN:
		return "nan:" + strconv.Itoa(t.id), nil
	case string:
		return "s:" + t, nil
	case []byte:
		return fmt.Sprintf("b:%x", t), nil
	case yamlDate:
		return fmt.Sprintf("d:%04d-%02d-%02d", t.year, t.month, t.day), nil
	case yamlTime:
		if t.aware {
			return "tz:" + strconv.FormatInt(t.t.UnixMicro(), 10), nil
		}
		return "tn:" + t.t.Format("2006-01-02T15:04:05.000000"), nil
	case yamlTuple:
		parts := make([]string, len(t))
		for i, e := range t {
			id, err := c.keyID(e)
			if err != nil {
				return "", err
			}
			parts[i] = id
		}
		return "t(" + strings.Join(parts, ",") + ")", nil
	}
	return "", fmt.Errorf("found unhashable key of type %T", v)
}

// Integer of a YAML int scalar the way construct_yaml_int reads it under the loader's version
func (c *yamlConstructor) yamlInt(text string) (any, error) {
	s := strings.ReplaceAll(text, "_", "")
	if s == "" {
		return nil, errors.New("string index out of range")
	}
	sign := int64(1)
	if s[0] == '-' {
		sign = -1
	}
	if s[0] == '+' || s[0] == '-' {
		s = s[1:]
	}
	var n *big.Int
	var err error
	switch {
	case s == "0":
		return big.NewInt(0), nil
	case strings.HasPrefix(s, "0b"):
		n, err = pyIntFromString(s[2:], 2)
	case strings.HasPrefix(s, "0x"):
		n, err = pyIntFromString(s[2:], 16)
	case strings.HasPrefix(s, "0o"):
		n, err = pyIntFromString(s[2:], 8)
	case c.res.version == [2]int{1, 1} && s == "":
		return nil, errors.New("string index out of range")
	case c.res.version == [2]int{1, 1} && s[0] == '0':
		n, err = pyIntFromString(s, 8)
	case c.res.version == [2]int{1, 1} && strings.Contains(s, ":"):
		parts := strings.Split(s, ":")
		n = new(big.Int)
		base := big.NewInt(1)
		for i := len(parts) - 1; i >= 0; i-- {
			d, err := pyIntFromString(parts[i], 10)
			if err != nil {
				return nil, err
			}
			n.Add(n, new(big.Int).Mul(d, base))
			base.Mul(base, big.NewInt(60))
		}
	default:
		n, err = pyIntFromString(s, 10)
	}
	if err != nil {
		return nil, err
	}
	return n.Mul(n, big.NewInt(sign)), nil
}

// Float of a YAML float scalar the way construct_yaml_float reads it under the loader's version
func (c *yamlConstructor) yamlFloat(text string) (any, error) {
	s := strings.ToLower(strings.ReplaceAll(text, "_", ""))
	if s == "" {
		return nil, errors.New("string index out of range")
	}
	sign := 1.0
	if s[0] == '-' {
		sign = -1
	}
	if s[0] == '+' || s[0] == '-' {
		s = s[1:]
	}
	switch {
	case s == ".inf":
		return sign * math.Inf(1), nil
	case s == ".nan":
		return yamlNaN{id: 0}, nil
	case c.res.version != [2]int{1, 2} && strings.Contains(s, ":"):
		parts := strings.Split(s, ":")
		digits := make([]float64, len(parts))
		for i, part := range parts {
			d, err := pyFloatFromString(part)
			if err != nil {
				return nil, err
			}
			digits[i] = d
		}
		value, base := 0.0, 1.0
		for i := len(digits) - 1; i >= 0; i-- {
			value += digits[i] * base
			base *= 60
		}
		if math.IsNaN(value) {
			c.nans++
			return yamlNaN{id: c.nans}, nil
		}
		return sign * value, nil
	}
	f, err := pyFloatFromString(s)
	if err != nil {
		return nil, err
	}
	if math.IsNaN(f) {
		c.nans++
		return yamlNaN{id: c.nans}, nil
	}
	return sign * f, nil
}

// Integer text in a base the way int parses it, allowing whitespace, a sign, a base prefix and Unicode digits
func pyIntFromString(text string, base int) (*big.Int, error) {
	s := strings.TrimFunc(text, pyIsSpace)
	neg := false
	if s != "" && (s[0] == '+' || s[0] == '-') {
		neg = s[0] == '-'
		s = s[1:]
	}
	prefixes := map[int]string{2: "0b", 8: "0o", 16: "0x"}
	if p, ok := prefixes[base]; ok && len(s) > 2 && strings.EqualFold(s[:2], p) {
		s = s[2:]
	}
	if s == "" {
		return nil, fmt.Errorf("invalid literal for int() with base %d: %q", base, text)
	}
	n := new(big.Int)
	for _, r := range s {
		d := -1
		switch {
		case unicode.IsDigit(r):
			start := r
			for unicode.IsDigit(start - 1) {
				start--
			}
			d = int(r-start) % 10
		case r >= 'a' && r <= 'z':
			d = int(r-'a') + 10
		case r >= 'A' && r <= 'Z':
			d = int(r-'A') + 10
		}
		if d < 0 || d >= base {
			return nil, fmt.Errorf("invalid literal for int() with base %d: %q", base, text)
		}
		n.Mul(n, big.NewInt(int64(base)))
		n.Add(n, big.NewInt(int64(d)))
	}
	if neg {
		n.Neg(n)
	}
	return n, nil
}

// Float text the way float() parses it, surrounding whitespace and Unicode digits allowed and no hex form
func pyFloatFromString(text string) (float64, error) {
	var b strings.Builder
	for _, r := range strings.TrimFunc(text, pyIsSpace) {
		if unicode.IsDigit(r) && r > 0x7f {
			start := r
			for unicode.IsDigit(start - 1) {
				start--
			}
			r = '0' + (r-start)%10
		}
		b.WriteRune(unicode.ToLower(r))
	}
	s := b.String()
	if !pyFloatTextRe.MatchString(s) {
		return 0, fmt.Errorf("could not convert string to float: %q", text)
	}
	switch strings.TrimLeft(s, "+-") {
	case "inf", "infinity":
		if strings.HasPrefix(s, "-") {
			return math.Inf(-1), nil
		}
		return math.Inf(1), nil
	case "nan":
		return math.NaN(), nil
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil && !errors.Is(err, strconv.ErrRange) {
		return 0, err
	}
	return f, nil
}

// Bytes of base64 text the way binascii.a2b_base64 decodes it without strict mode
func pyA2BBase64(s string) ([]byte, error) {
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"
	var out []byte
	quad, pads := 0, 0
	var left byte
	for i := 0; i < len(s); i++ {
		ch := s[i]
		if ch == '=' {
			if quad >= 2 {
				pads++
				if quad+pads >= 4 {
					return out, nil
				}
			}
			continue
		}
		v := strings.IndexByte(alphabet, ch)
		if v < 0 {
			continue
		}
		pads = 0
		b := byte(v)
		switch quad {
		case 0:
			quad, left = 1, b
		case 1:
			quad = 2
			out = append(out, left<<2|b>>4)
			left = b & 0x0f
		case 2:
			quad = 3
			out = append(out, left<<4|b>>2)
			left = b & 0x03
		case 3:
			quad = 0
			out = append(out, left<<6|b)
			left = 0
		}
	}
	switch quad {
	case 0:
		return out, nil
	case 1:
		return nil, fmt.Errorf("Invalid base64-encoded string: number of data characters (%d) cannot be 1 more than a multiple of 4", len(out)/3*4+1)
	}
	return nil, errors.New("Incorrect padding")
}

// Date or datetime of a timestamp scalar the way create_timestamp builds it, an error where Python raises
func yamlTimestamp(text string) (any, error) {
	m := yamlTimestampRe.FindStringSubmatch(text)
	if m == nil {
		return nil, fmt.Errorf("failed to construct timestamp from %q", text)
	}
	atoi := func(s string) int { n, _ := strconv.Atoi(s); return n }
	year, month, day := atoi(m[1]), atoi(m[2]), atoi(m[3])
	if year < 1 || year > 9999 {
		return nil, fmt.Errorf("year %d is out of range", year)
	}
	if month < 1 || month > 12 {
		return nil, fmt.Errorf("month must be in 1..12, not %d", month)
	}
	if last := time.Date(year, time.Month(month)+1, 0, 0, 0, 0, 0, time.UTC).Day(); day < 1 || day > last {
		return nil, fmt.Errorf("day %d must be in range 1..%d for month %d in year %d", day, last, month, year)
	}
	if m[4] == "" {
		return yamlDate{year, month, day}, nil
	}
	hour, minute, second := atoi(m[4]), atoi(m[5]), atoi(m[6])
	if hour > 23 {
		return nil, fmt.Errorf("hour must be in 0..23, not %d", hour)
	}
	if minute > 59 {
		return nil, fmt.Errorf("minute must be in 0..59, not %d", minute)
	}
	if second > 59 {
		return nil, fmt.Errorf("second must be in 0..59, not %d", second)
	}
	frac, carry := 0, false
	if f := m[7]; f != "" {
		digits := (f + "000000")[:6]
		frac = atoi(digits)
		if len(f) > 6 && f[6] > '4' {
			frac++
		}
		if frac > 999999 {
			frac, carry = 0, true
		}
	}
	loc, aware := time.UTC, false
	if m[9] != "" {
		offset := (atoi(m[10])*60 + atoi(m[11])) * 60
		if offset >= 24*3600 {
			return nil, errors.New("offset must be a timedelta strictly between -timedelta(hours=24) and timedelta(hours=24)")
		}
		if m[9] == "-" {
			offset = -offset
		}
		loc, aware = time.FixedZone(m[8], offset), true
	} else if m[8] == "Z" {
		loc, aware = time.FixedZone("Z", 0), true
	}
	t := time.Date(year, time.Month(month), day, hour, minute, second, frac*1000, loc)
	if carry {
		t = t.Add(time.Second)
		if t.Year() > 9999 {
			return nil, errors.New("date value out of range")
		}
	}
	return yamlTime{t: t, aware: aware}, nil
}
