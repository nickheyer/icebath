package config

import (
	"math/big"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

// Version directive line that asks for YAML 1.2, the version text in the first group
var versionDirective12 = regexp.MustCompile(`^%YAML[ \t]+(1\.2)(?:[ \t]+#.*|[ \t]*)$`)

// Number forms a YAML 1.2 document resolves, binary, octal, decimal and hex integers and dotted or exponent floats
var (
	int12   = regexp.MustCompile(`^(?:[-+]?0b[0-1_]+|[-+]?0o?[0-7_]+|[-+]?[0-9_]+|[-+]?0x[0-9a-fA-F_]+)$`)
	float12 = regexp.MustCompile(`^(?:[-+]?[0-9][0-9_]*\.[0-9_]*(?:[eE][-+]?[0-9]+)?|[-+]?[0-9][0-9_]*[eE][-+]?[0-9]+|[-+]?\.[0-9_]+(?:[eE][-+][0-9]+)?|[-+]?\.(?:inf|Inf|INF)|\.(?:nan|NaN|NAN))$`)
)

// Byte offset of the version text in a leading directive that declares YAML 1.2, ok false without one
func version12Directive(text string) (int, bool) {
	for at := 0; at < len(text); {
		end := strings.IndexByte(text[at:], '\n')
		line := text[at:]
		if end >= 0 {
			line = text[at : at+end]
		}
		switch trimmed := strings.TrimLeft(line, " \t"); {
		case trimmed == "" || strings.HasPrefix(trimmed, "#"):
		case strings.HasPrefix(line, "%"):
			if m := versionDirective12.FindStringSubmatchIndex(line); m != nil {
				return at + m[2], true
			}
		default:
			return 0, false
		}
		if end < 0 {
			break
		}
		at += end + 1
	}
	return 0, false
}

// Text yaml.v3 accepts for a YAML 1.2 document, the directive version rewritten in place so every position holds
func parserText(text string) (string, bool) {
	at, ok := version12Directive(text)
	if !ok {
		return text, false
	}
	return text[:at] + "1.1" + text[at+3:], true
}

// Integer a YAML 1.2 loader builds from the text, underscores dropped and a leading zero kept decimal, ok false otherwise
func constructInt12(s string) (*big.Int, bool) {
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
	var ok bool
	switch {
	case strings.HasPrefix(t, "0b"):
		_, ok = v.SetString(t[2:], 2)
	case strings.HasPrefix(t, "0x"):
		_, ok = v.SetString(t[2:], 16)
	case strings.HasPrefix(t, "0o"):
		_, ok = v.SetString(t[2:], 8)
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

// YAML 1.2 type of a plain scalar and the text YAML 1.1 readers take as that value
func resolvePlain12(s string) (tag, text string) {
	switch s {
	case "", "~", "null", "Null", "NULL":
		return tagNull, s
	case "true", "True", "TRUE":
		return tagBool, "true"
	case "false", "False", "FALSE":
		return tagBool, "false"
	case "<<":
		return mergeTag, s
	}
	if !strings.ContainsRune("-+0123456789.", rune(s[0])) {
		return tagStr, s
	}
	if s[0] != '.' && int12.MatchString(s) {
		if v, ok := constructInt12(s); ok {
			return tagInt, v.String()
		}
		return tagStr, s
	}
	if float12.MatchString(s) {
		if _, ok := constructFloat(s); ok {
			return tagFloat, s
		}
		return tagStr, s
	}
	if tag, text := resolvePlain(s); tag == "!!timestamp" {
		return tag, text
	}
	return tagStr, s
}

// Reports a plain scalar a YAML 1.2 document reads as another type or number than YAML 1.1 does
func differsUnder12(s string) (tag, text string, differs bool) {
	tag11, text11 := resolvePlain(s)
	tag12, text12 := resolvePlain12(s)
	if tag11 != tag12 {
		return tag12, text12, true
	}
	if tag12 == tagInt {
		a, _ := constructInt(text11)
		b, _ := constructInt(text12)
		return tag12, text12, a == nil || b == nil || a.Cmp(b) != 0
	}
	return tag12, text12, false
}

// Pins every plain scalar of a YAML 1.2 document to its 1.2 type for the YAML 1.1 readers
func pinCoreSchema(n *yaml.Node, key bool, seen map[*yaml.Node]bool) {
	if n == nil || seen[n] || n.Kind == yaml.AliasNode {
		return
	}
	seen[n] = true
	if n.Kind == yaml.ScalarNode {
		if n.Style&fixedStyles != 0 {
			return
		}
		tag, text, differs := differsUnder12(n.Value)
		tag11, _ := resolvePlain(n.Value)
		switch {
		case !differs || tag == mergeTag || tag11 == mergeTag:
		case key && tag == tag11:
		case key:
			n.Tag, n.Style = tag, n.Style|yaml.TaggedStyle
		default:
			n.Tag, n.Value, n.Style = tag, text, n.Style|yaml.TaggedStyle
		}
		return
	}
	for i, c := range n.Content {
		pinCoreSchema(c, n.Kind == yaml.MappingNode && i%2 == 0, seen)
	}
}
