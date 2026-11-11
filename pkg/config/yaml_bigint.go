package config

import (
	"math/big"
	"strings"

	"gopkg.in/yaml.v3"
)

const yamlBigIntSentinel = "\x00icebath-yaml-bigint\x00"

// Integer a YAML 1.1 int scalar constructs the way PyYAML construct_yaml_int does, false when it is not one
func yaml11IntValue(text string) (*big.Int, bool) {
	value := strings.ReplaceAll(text, "_", "")
	sign := 1
	if value != "" && (value[0] == '-' || value[0] == '+') {
		if value[0] == '-' {
			sign = -1
		}
		value = value[1:]
	}
	n := new(big.Int)
	ok := true
	switch {
	case value == "0":
		n.SetInt64(0)
	case strings.HasPrefix(value, "0b"):
		_, ok = n.SetString(value[2:], 2)
	case strings.HasPrefix(value, "0x"):
		_, ok = n.SetString(value[2:], 16)
	case strings.HasPrefix(value, "0"):
		_, ok = n.SetString(value, 8)
	case strings.Contains(value, ":"):
		base := big.NewInt(1)
		parts := strings.Split(value, ":")
		for i := len(parts) - 1; i >= 0; i-- {
			digit, good := new(big.Int).SetString(parts[i], 10)
			if !good {
				return nil, false
			}
			n.Add(n, digit.Mul(digit, base))
			base = new(big.Int).Mul(base, big.NewInt(60))
		}
	default:
		_, ok = n.SetString(value, 10)
	}
	if !ok {
		return nil, false
	}
	if sign < 0 {
		n.Neg(n)
	}
	return n, true
}

// Rewrites int scalars past int64 as sentinel strings so the decode keeps them for restoreYAMLBigInts
func markYAMLBigInts(n *yaml.Node) {
	if n == nil {
		return
	}
	if n.Kind == yaml.ScalarNode && n.Tag == "!!int" {
		if v, ok := yaml11IntValue(n.Value); ok && !v.IsInt64() {
			n.Tag, n.Value, n.Style = "!!str", yamlBigIntSentinel+v.String(), 0
		}
		return
	}
	for _, c := range n.Content {
		markYAMLBigInts(c)
	}
}

// Turns the sentinel strings of a decoded document back into big integers
func restoreYAMLBigInts(v any) any {
	switch c := v.(type) {
	case string:
		if strings.HasPrefix(c, yamlBigIntSentinel) {
			n, _ := new(big.Int).SetString(strings.TrimPrefix(c, yamlBigIntSentinel), 10)
			return n
		}
	case map[string]any:
		for k, val := range c {
			c[k] = restoreYAMLBigInts(val)
		}
	case []any:
		for i, val := range c {
			c[i] = restoreYAMLBigInts(val)
		}
	}
	return v
}
