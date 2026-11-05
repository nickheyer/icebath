package config

import (
	"math/big"
	"regexp"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

var (
	yaml11Bool  = regexp.MustCompile(`^(?:y|Y|yes|Yes|YES|n|N|no|No|NO|true|True|TRUE|false|False|FALSE|on|On|ON|off|Off|OFF)$`)
	yaml11Int   = regexp.MustCompile(`^(?:[-+]?0b[0-1_]+|[-+]?0[0-7_]+|[-+]?(?:0|[1-9][0-9_]*)|[-+]?0x[0-9a-fA-F_]+|[-+]?[1-9][0-9_]*(?::[0-5]?[0-9])+)$`)
	yaml11Float = regexp.MustCompile(`^(?:[-+]?(?:[0-9][0-9_]*)\.[0-9_]*(?:[eE][-+]?[0-9]+)?|[-+]?(?:[0-9][0-9_]*)(?:[eE][-+]?[0-9]+)|\.[0-9_]+(?:[eE][-+][0-9]+)?|[-+]?[0-9][0-9_]*(?::[0-5]?[0-9])+\.[0-9_]*|[-+]?\.(?:inf|Inf|INF)|\.(?:nan|NaN|NAN))$`)
)

// Retags plain value scalars the way a YAML 1.1 safe loader resolves them, mapping keys keep their text
func resolveYAML11(n *yaml.Node, isKey bool) {
	switch n.Kind {
	case yaml.DocumentNode, yaml.SequenceNode:
		for _, c := range n.Content {
			resolveYAML11(c, false)
		}
	case yaml.MappingNode:
		for i, c := range n.Content {
			resolveYAML11(c, i%2 == 0)
		}
	case yaml.ScalarNode:
		if isKey || n.Style != 0 {
			return
		}
		switch v := n.Value; {
		case yaml11Bool.MatchString(v):
			n.Tag, n.Value = "!!bool", strconv.FormatBool(strings.ContainsAny(v[:1], "yYtT") || strings.EqualFold(v, "on"))
		case yaml11Int.MatchString(v):
			if text, ok := yaml11IntText(v); ok {
				n.Tag, n.Value = "!!int", text
			}
		case yaml11Float.MatchString(v):
			if f, ok := yaml11FloatValue(v); ok {
				n.Tag, n.Value = "!!float", strconv.FormatFloat(f, 'g', -1, 64)
			}
		case n.Tag == "!!int" || n.Tag == "!!float":
			n.Tag = "!!str"
		}
	}
}

// Decimal text of a YAML 1.1 int with underscores, base prefixes, leading zero octal and base 60 parts
func yaml11IntText(v string) (string, bool) {
	s := strings.ReplaceAll(v, "_", "")
	sign := ""
	if s != "" && (s[0] == '-' || s[0] == '+') {
		if s[0] == '-' {
			sign = "-"
		}
		s = s[1:]
	}
	n := new(big.Int)
	var ok bool
	switch {
	case strings.Contains(s, ":"):
		ok = true
		for _, part := range strings.Split(s, ":") {
			d, err := strconv.Atoi(part)
			if err != nil {
				return "", false
			}
			n.Mul(n, big.NewInt(60)).Add(n, big.NewInt(int64(d)))
		}
	case strings.HasPrefix(s, "0b"):
		_, ok = n.SetString(s[2:], 2)
	case strings.HasPrefix(s, "0x"):
		_, ok = n.SetString(s[2:], 16)
	case len(s) > 1 && s[0] == '0':
		_, ok = n.SetString(s[1:], 8)
	default:
		_, ok = n.SetString(s, 10)
	}
	if !ok {
		return "", false
	}
	return sign + n.String(), true
}

// Value of a YAML 1.1 float with underscores and base 60 parts
func yaml11FloatValue(v string) (float64, bool) {
	s := strings.ReplaceAll(v, "_", "")
	sign := 1.0
	if s != "" && (s[0] == '-' || s[0] == '+') {
		if s[0] == '-' {
			sign = -1
		}
		s = s[1:]
	}
	if !strings.Contains(s, ":") {
		f, err := strconv.ParseFloat(strings.ToLower(s), 64)
		return sign * f, err == nil
	}
	total := 0.0
	for _, part := range strings.Split(s, ":") {
		d, err := strconv.ParseFloat(part, 64)
		if err != nil {
			return 0, false
		}
		total = total*60 + d
	}
	return sign * total, true
}
