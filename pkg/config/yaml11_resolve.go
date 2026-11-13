package config

import (
	"errors"
	"fmt"
	"math/big"
	"regexp"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

var (
	yaml11Bool  = regexp.MustCompile(`^(?:y|Y|yes|Yes|YES|n|N|no|No|NO|true|True|TRUE|false|False|FALSE|on|On|ON|off|Off|OFF)$`)
	yaml11Int   = regexp.MustCompile(`^(?:[-+]?0b[0-1_]+|[-+]?0[0-7_]+|[-+]?(?:0|[1-9][0-9_]*)|[-+]?0x[0-9a-fA-F_]+|[-+]?[1-9][0-9_]*(?::[0-5]?[0-9])+)$`)
	yaml11Float = regexp.MustCompile(`^(?:[-+]?(?:[0-9][0-9_]*)\.[0-9_]*(?:[eE][-+]?[0-9]+)?|[-+]?(?:[0-9][0-9_]*)(?:[eE][-+]?[0-9]+)|\.[0-9_]+(?:[eE][-+][0-9]+)?|[-+]?[0-9][0-9_]*(?::[0-5]?[0-9])+\.[0-9_]*|[-+]?\.(?:inf|Inf|INF)|\.(?:nan|NaN|NAN))$`)
	yaml12Bool  = regexp.MustCompile(`^(?:true|True|TRUE|false|False|FALSE)$`)
	yaml12Int   = regexp.MustCompile(`^(?:[-+]?0b[0-1_]+|[-+]?0o?[0-7_]+|[-+]?[0-9_]+|[-+]?0x[0-9a-fA-F_]+)$`)
	yaml12Float = regexp.MustCompile(`^(?:[-+]?(?:[0-9][0-9_]*)\.[0-9_]*(?:[eE][-+]?[0-9]+)?|[-+]?(?:[0-9][0-9_]*)(?:[eE][-+]?[0-9]+)|[-+]?\.[0-9_]+(?:[eE][-+][0-9]+)?|[-+]?\.(?:inf|Inf|INF)|\.(?:nan|NaN|NAN))$`)
)

// Retags plain value scalars the way the safe loader resolves them under YAML 1.1 or 1.2, mapping keys keep their text
func resolveYAMLVersion(n *yaml.Node, isKey, v12 bool) {
	switch n.Kind {
	case yaml.DocumentNode, yaml.SequenceNode:
		for _, c := range n.Content {
			resolveYAMLVersion(c, false, v12)
		}
	case yaml.MappingNode:
		for i, c := range n.Content {
			resolveYAMLVersion(c, i%2 == 0, v12)
		}
	case yaml.ScalarNode:
		if isKey || n.Style != 0 {
			return
		}
		if v12 {
			resolveYAML12Scalar(n)
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
				n.Tag, n.Value = "!!float", floatNodeText(f)
			}
		case n.Tag == "!!int" || n.Tag == "!!float":
			n.Tag = "!!str"
		}
	}
}

// Retags one plain scalar by the YAML 1.2 resolvers, where leading zero ints stay decimal and 0o marks octal
func resolveYAML12Scalar(n *yaml.Node) {
	switch v := n.Value; {
	case yaml12Bool.MatchString(v):
		n.Tag, n.Value = "!!bool", strconv.FormatBool(strings.EqualFold(v, "true"))
	case yaml12Int.MatchString(v):
		if text, ok := yaml12IntText(v); ok {
			n.Tag, n.Value = "!!int", text
		}
	case yaml12Float.MatchString(v):
		f, err := strconv.ParseFloat(strings.ToLower(strings.ReplaceAll(v, "_", "")), 64)
		if err == nil {
			n.Tag, n.Value = "!!float", floatNodeText(f)
		}
	case n.Tag == "!!int" || n.Tag == "!!float":
		n.Tag = "!!str"
	}
}

// Decimal text of a YAML 1.2 int with underscores and 0b, 0o and 0x prefixes
func yaml12IntText(v string) (string, bool) {
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
	case strings.HasPrefix(s, "0b"):
		_, ok = n.SetString(s[2:], 2)
	case strings.HasPrefix(s, "0x"):
		_, ok = n.SetString(s[2:], 16)
	case strings.HasPrefix(s, "0o"):
		_, ok = n.SetString(s[2:], 8)
	default:
		_, ok = n.SetString(s, 10)
	}
	if !ok {
		return "", false
	}
	return sign + n.String(), true
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

var (
	yaml11TimestampParts = regexp.MustCompile(`^([0-9][0-9][0-9][0-9])-([0-9][0-9]?)-([0-9][0-9]?)(?:(?:[Tt]|[ \t]+)([0-9][0-9]?):([0-9][0-9]):([0-9][0-9])(?:\.[0-9]*)?(?:[ \t]*(?:Z|([-+])([0-9][0-9]?)(?::([0-9][0-9]))?))?)?$`)
)

// First ValueError a YAML 1.1 safe loader raises building date and datetime values from plain timestamp scalars
func yaml11TimestampError(n *yaml.Node) error {
	if n.Kind == yaml.ScalarNode {
		if n.Style != 0 || !yaml11Timestamp.MatchString(n.Value) {
			return nil
		}
		if msg := yaml11TimestampValueError(n.Value); msg != "" {
			return errors.New(msg)
		}
		return nil
	}
	for _, c := range n.Content {
		if err := yaml11TimestampError(c); err != nil {
			return err
		}
	}
	return nil
}

// Python datetime constructor message for an out of range timestamp field, empty for a real date
func yaml11TimestampValueError(v string) string {
	m := yaml11TimestampParts.FindStringSubmatch(v)
	num := func(i int) int { n, _ := strconv.Atoi(m[i]); return n }
	if m[7] != "" {
		minutes := num(8) * 60
		if m[9] != "" {
			minutes += num(9)
		}
		if minutes >= 24*60 {
			if m[7] == "-" {
				minutes = -minutes
			}
			return "offset must be a timedelta strictly between -timedelta(hours=24) and timedelta(hours=24), not " + pyTimedeltaRepr(minutes*60) + "."
		}
	}
	year, month, day := num(1), num(2), num(3)
	switch {
	case year < 1:
		return fmt.Sprintf("year %d is out of range", year)
	case month < 1 || month > 12:
		return "month must be in 1..12"
	case day < 1 || day > time.Date(year, time.Month(month)+1, 0, 0, 0, 0, 0, time.UTC).Day():
		return "day is out of range for month"
	case m[4] == "":
		return ""
	case num(4) > 23:
		return "hour must be in 0..23"
	case num(5) > 59:
		return "minute must be in 0..59"
	case num(6) > 59:
		return "second must be in 0..59"
	}
	return ""
}

// Python repr of a timedelta holding whole seconds
func pyTimedeltaRepr(seconds int) string {
	days := seconds / 86400
	if seconds%86400 < 0 {
		days--
	}
	secs := seconds - days*86400
	var parts []string
	if days != 0 {
		parts = append(parts, fmt.Sprintf("days=%d", days))
	}
	if secs != 0 {
		parts = append(parts, fmt.Sprintf("seconds=%d", secs))
	}
	if len(parts) == 0 {
		return "datetime.timedelta(0)"
	}
	return "datetime.timedelta(" + strings.Join(parts, ", ") + ")"
}

// Float text the decoder reads back as that same float, a fraction kept so negative zero and whole values stay float
func floatNodeText(f float64) string {
	s := strconv.FormatFloat(f, 'g', -1, 64)
	if !strings.ContainsAny(s, ".eEnN") {
		s += ".0"
	}
	return s
}
