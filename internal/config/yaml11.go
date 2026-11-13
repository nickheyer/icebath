package config

import (
	"errors"
	"math"
	"math/big"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

const (
	tagNull  = "!!null"
	tagBool  = "!!bool"
	tagInt   = "!!int"
	tagFloat = "!!float"
	tagStr   = "!!str"
	tagMap   = "!!map"
	tagSeq   = "!!seq"
)

// Raised when expanding the aliases of a document would grow it past its alias budget
var ErrExcessiveAliasing = errors.New("document contains excessive aliasing")

// Style bits that make a scalar a string or carry an explicit tag
const fixedStyles = yaml.TaggedStyle | yaml.DoubleQuotedStyle | yaml.SingleQuotedStyle | yaml.LiteralStyle | yaml.FoldedStyle

// YAML 1.1 type of a plain scalar and the text yaml.v3 reads as the same value under that type
func resolvePlain(s string) (tag, text string) {
	switch s {
	case "", "~", "null", "Null", "NULL":
		return tagNull, s
	case "y", "Y", "yes", "Yes", "YES", "true", "True", "TRUE", "on", "On", "ON":
		return tagBool, "true"
	case "n", "N", "no", "No", "NO", "false", "False", "FALSE", "off", "Off", "OFF":
		return tagBool, "false"
	case "<<":
		return "!!merge", s
	}
	if !strings.ContainsRune("-+0123456789.", rune(s[0])) {
		return tagStr, s
	}
	if isFloat11(s) {
		if f, ok := constructFloat(s); ok {
			return tagFloat, floatTextFor(s, f)
		}
		return tagStr, s
	}
	if isInt11(s) {
		if v, ok := constructInt(s); ok {
			return tagInt, intTextFor(s, v)
		}
		return tagStr, s
	}
	if isTimestamp11(s) {
		if probe := (&yaml.Node{Kind: yaml.ScalarNode, Value: s}); probe.ShortTag() == "!!timestamp" {
			return "!!timestamp", s
		}
	}
	return tagStr, s
}

// Text an explicitly tagged scalar reads as under YAML 1.1 construction, ok false when it cannot be built
func resolveTagged(tag, s string) (string, bool) {
	switch tag {
	case tagBool:
		switch strings.ToLower(s) {
		case "yes", "y", "true", "on":
			return "true", true
		case "no", "n", "false", "off":
			return "false", true
		}
		return s, false
	case tagInt:
		if v, ok := constructInt(s); ok {
			return intTextFor(s, v), true
		}
		return s, false
	case tagFloat:
		if f, ok := constructFloat(s); ok {
			return floatTextFor(s, f), true
		}
		return s, false
	case tagNull:
		return "null", true
	}
	return s, true
}

// Reports the YAML 1.1 integer forms binary, octal, decimal, hex and base 60
func isInt11(s string) bool {
	body := strings.TrimLeft(s[:1], "+-") + s[1:]
	if body == "" {
		return false
	}
	switch {
	case strings.HasPrefix(body, "0b"):
		return len(body) > 2 && allIn(body[2:], "01_")
	case strings.HasPrefix(body, "0x"):
		return len(body) > 2 && allIn(body[2:], "0123456789abcdefABCDEF_")
	case allIn(body, "01234567_"):
		return true
	case body == "0":
		return true
	case body[0] >= '1' && body[0] <= '9' && allIn(body, "0123456789_"):
		return true
	}
	head, tail, found := strings.Cut(body, ":")
	return found && head != "" && head[0] >= '1' && head[0] <= '9' && allIn(head, "0123456789_") && base60Tail(tail)
}

// Reports the YAML 1.1 float forms, base 60 floats and the infinity and not-a-number words included
func isFloat11(s string) bool {
	switch s {
	case ".nan", ".NaN", ".NAN":
		return true
	}
	body := s
	signed := s[0] == '-' || s[0] == '+'
	if signed {
		body = s[1:]
	}
	switch body {
	case ".inf", ".Inf", ".INF":
		return true
	case "":
		return false
	}
	if body[0] == '.' {
		if signed {
			return false
		}
		mant, exp, hasExp := strings.Cut(body[1:], "e")
		if !hasExp {
			mant, exp, hasExp = strings.Cut(body[1:], "E")
		}
		return mant != "" && allIn(mant, "0123456789_") && (!hasExp || signedDigits(exp, true))
	}
	if body[0] < '0' || body[0] > '9' {
		return false
	}
	if head, tail, found := strings.Cut(body, ":"); found {
		sixty, frac, dotted := strings.Cut(tail, ".")
		return dotted && allIn(head, "0123456789_") && allIn(frac, "0123456789_") && base60Tail(sixty)
	}
	mant, exp, hasExp := strings.Cut(body, "e")
	if !hasExp {
		mant, exp, hasExp = strings.Cut(body, "E")
	}
	if hasExp && !signedDigits(exp, false) {
		return false
	}
	whole, frac, dotted := strings.Cut(mant, ".")
	if !allIn(whole, "0123456789_") || !allIn(frac, "0123456789_") {
		return false
	}
	return dotted || hasExp
}

// Reports the YAML 1.1 date and date-time forms
func isTimestamp11(s string) bool {
	if len(s) < 10 || !allIn(s[:4], "0123456789") || s[4] != '-' {
		return false
	}
	if len(s) == 10 {
		return allIn(s[5:7], "0123456789") && s[7] == '-' && allIn(s[8:10], "0123456789")
	}
	return strings.ContainsAny(s, "Tt \t") && strings.Count(s, ":") >= 2
}

// Reports colon separated groups of one or two digits, each group a valid base 60 digit
func base60Tail(s string) bool {
	for part := range strings.SplitSeq(s, ":") {
		if part == "" || len(part) > 2 || !allIn(part, "0123456789") || (len(part) == 2 && part[0] > '5') {
			return false
		}
	}
	return true
}

// Reports digits with an optional sign, the sign required when strict
func signedDigits(s string, strict bool) bool {
	if s != "" && (s[0] == '-' || s[0] == '+') {
		s = s[1:]
	} else if strict {
		return false
	}
	return s != "" && allIn(s, "0123456789")
}

// Reports a string made only of characters from set
func allIn(s, set string) bool {
	for _, r := range s {
		if !strings.ContainsRune(set, r) {
			return false
		}
	}
	return true
}

// Integer a YAML 1.1 loader builds from the text, binary, hex, leading zero octal and base 60 included
func constructInt(s string) (*big.Int, bool) {
	t := strings.ReplaceAll(s, "_", "")
	neg := false
	if t != "" && (t[0] == '-' || t[0] == '+') {
		neg = t[0] == '-'
		t = t[1:]
	}
	if t == "" {
		return nil, false
	}
	v := new(big.Int)
	ok := true
	switch {
	case t == "0":
	case strings.HasPrefix(t, "0b"):
		_, ok = v.SetString(t[2:], 2)
	case strings.HasPrefix(t, "0x"):
		_, ok = v.SetString(t[2:], 16)
	case strings.HasPrefix(t, "0o"):
		_, ok = v.SetString(t[2:], 8)
	case t[0] == '0':
		_, ok = v.SetString(t, 8)
	case strings.Contains(t, ":"):
		for part := range strings.SplitSeq(t, ":") {
			d, err := strconv.ParseInt(part, 10, 64)
			if err != nil {
				return nil, false
			}
			v.Mul(v, big.NewInt(60))
			v.Add(v, big.NewInt(d))
		}
	default:
		_, ok = v.SetString(t, 10)
	}
	if !ok {
		return nil, false
	}
	if neg {
		v.Neg(v)
	}
	return v, true
}

// Float a YAML 1.1 loader builds from the text, base 60 and the infinity and not-a-number words included
func constructFloat(s string) (float64, bool) {
	t := strings.ToLower(strings.ReplaceAll(s, "_", ""))
	sign := 1.0
	if t != "" && (t[0] == '-' || t[0] == '+') {
		if t[0] == '-' {
			sign = -1
		}
		t = t[1:]
	}
	switch {
	case t == ".inf":
		return math.Inf(int(sign)), true
	case t == ".nan":
		return math.NaN(), true
	case strings.Contains(t, ":"):
		v := 0.0
		for part := range strings.SplitSeq(t, ":") {
			d, err := strconv.ParseFloat(part, 64)
			if err != nil {
				return 0, false
			}
			v = v*60 + d
		}
		return sign * v, true
	}
	f, err := strconv.ParseFloat(t, 64)
	if err != nil && !errors.Is(err, strconv.ErrRange) {
		return 0, false
	}
	return sign * f, true
}

// Original integer text when yaml.v3 reads it as the same number, else the decimal form
func intTextFor(s string, v *big.Int) string {
	plain := strings.ReplaceAll(s, "_", "")
	if got, err := strconv.ParseInt(plain, 0, 64); err == nil && v.IsInt64() && got == v.Int64() {
		return s
	}
	return v.String()
}

// Original float text when yaml.v3 reads it as the same number, else a canonical float form
func floatTextFor(s string, f float64) string {
	var got float64
	probe := yaml.Node{Kind: yaml.ScalarNode, Tag: tagFloat, Value: s}
	if probe.Decode(&got) == nil && (got == f || (math.IsNaN(got) && math.IsNaN(f))) {
		return s
	}
	return floatText(f)
}

// Float text both YAML 1.1 and yaml.v3 read as a float of the same value
func floatText(f float64) string {
	switch {
	case math.IsNaN(f):
		return ".nan"
	case math.IsInf(f, 1):
		return ".inf"
	case math.IsInf(f, -1):
		return "-.inf"
	}
	s := strconv.FormatFloat(f, 'g', -1, 64)
	if !strings.ContainsAny(s, ".e") {
		s += ".0"
	}
	return s
}

// Node under YAML 1.1 typing with aliases expanded, sharing every subtree that needs no change
func resolved(n *yaml.Node) (*yaml.Node, error) {
	if n == nil {
		return nil, nil
	}
	b := aliasBudget{root: n}
	out, _, err := b.resolve(n, false)
	return out, err
}

// Node allowance for alias expansion, ten times the document plus a floor, counted on first use
type aliasBudget struct {
	root  *yaml.Node
	left  int
	ready bool
}

// Resolved subtree and whether it differs from the input, keys keeping their text and tag
func (b *aliasBudget) resolve(n *yaml.Node, key bool) (*yaml.Node, bool, error) {
	if n.Kind == yaml.AliasNode {
		target := resolveAlias(n)
		if target == nil {
			return nullNode(), true, nil
		}
		if !b.ready {
			b.left, b.ready = 10*nodeCount(b.root, map[*yaml.Node]bool{})+10_000, true
		}
		if b.left -= nodeCount(target, map[*yaml.Node]bool{}); b.left < 0 {
			return nil, false, ErrExcessiveAliasing
		}
		out, _, err := b.resolve(target, key)
		if err != nil {
			return nil, false, err
		}
		c := *out
		c.Anchor = ""
		return &c, true, nil
	}
	if n.Kind == yaml.ScalarNode {
		tag, text := n.Tag, n.Value
		if !key {
			tag, text = scalarType(n)
		}
		if tag == n.Tag && text == n.Value && n.Anchor == "" {
			return n, false, nil
		}
		c := *n
		c.Tag, c.Value, c.Anchor = tag, text, ""
		return &c, true, nil
	}
	var content []*yaml.Node
	for i, child := range n.Content {
		r, changed, err := b.resolve(child, n.Kind == yaml.MappingNode && i%2 == 0)
		if err != nil {
			return nil, false, err
		}
		if changed && content == nil {
			content = make([]*yaml.Node, len(n.Content))
			copy(content, n.Content[:i])
		}
		if content != nil {
			content[i] = r
		}
	}
	if content == nil && n.Anchor == "" {
		return n, false, nil
	}
	c := *n
	c.Anchor = ""
	if content != nil {
		c.Content = content
	}
	return &c, true, nil
}

// Nodes of a tree counted once each, aliases not followed
func nodeCount(n *yaml.Node, seen map[*yaml.Node]bool) int {
	if n == nil || seen[n] {
		return 0
	}
	seen[n] = true
	total := 1
	for _, c := range n.Content {
		total += nodeCount(c, seen)
	}
	return total
}

// YAML 1.1 tag and value text of a scalar, honoring quoting, block styles and explicit tags
func scalarType(n *yaml.Node) (tag, text string) {
	switch {
	case n.Style&yaml.TaggedStyle != 0:
		tag = n.ShortTag()
		text, _ = resolveTagged(tag, n.Value)
		return tag, text
	case n.Style&fixedStyles != 0:
		return tagStr, n.Value
	}
	return resolvePlain(n.Value)
}

// Copy of n for writing, plain scalars untagged where YAML 1.1 reads them as their tag, else quoted or tagged
func emitted(n *yaml.Node) *yaml.Node {
	return emitTree(n, false)
}

// Writing form of one subtree, keys and untagged scalars written as they are, keys being read back as text
func emitTree(n *yaml.Node, key bool) *yaml.Node {
	n = resolveAlias(n)
	if n == nil {
		return nullNode()
	}
	c := *n
	c.Anchor, c.Alias = "", nil
	switch {
	case n.Kind == yaml.ScalarNode && (key || n.Tag == ""):
	case n.Kind == yaml.ScalarNode && n.Style&fixedStyles == 0:
		tag := n.ShortTag()
		got, text := resolvePlain(n.Value)
		switch {
		case got == tag && text == n.Value:
			c.Tag = ""
		case tag == tagStr:
			c.Style = yaml.DoubleQuotedStyle
		default:
			c.Value, _ = resolveTagged(tag, n.Value)
			if again, _ := resolvePlain(c.Value); again != tag {
				c.Style = yaml.TaggedStyle
			} else {
				c.Tag = ""
			}
		}
	case len(n.Content) > 0:
		c.Content = make([]*yaml.Node, len(n.Content))
		for i, child := range n.Content {
			c.Content[i] = emitTree(child, n.Kind == yaml.MappingNode && i%2 == 0)
		}
	}
	return &c
}

// Explicit null scalar
func nullNode() *yaml.Node {
	return &yaml.Node{Kind: yaml.ScalarNode, Tag: tagNull, Value: "null"}
}
