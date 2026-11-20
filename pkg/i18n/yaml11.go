package i18n

import (
	"bytes"
	"fmt"
	"iter"
	"math"
	"math/big"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"

	"gopkg.in/yaml.v3"
)

// Ordered mapping node, the shape a YAML 1.1 safe loader hands back for a mapping
type pyMap []pyPair

type pyPair struct {
	Key   any
	Value any
}

// Timestamp scalar kept with its parsed time so str renders it like a Python date or datetime
type pyTimestamp struct {
	t      time.Time
	isDate bool
	hasTZ  bool
	frac   bool
}

var (
	yaml11Bool  = regexp.MustCompile(`^(?:yes|Yes|YES|no|No|NO|true|True|TRUE|false|False|FALSE|on|On|ON|off|Off|OFF|y|Y|n|N)$`)
	yaml11True  = map[string]bool{"yes": true, "Yes": true, "YES": true, "true": true, "True": true, "TRUE": true, "on": true, "On": true, "ON": true, "y": true, "Y": true}
	yaml11Null  = regexp.MustCompile(`^(?:~|null|Null|NULL|)$`)
	yaml11Int   = regexp.MustCompile(`^(?:[-+]?0b[0-1_]+|[-+]?0[0-7_]+|[-+]?(?:0|[1-9][0-9_]*)|[-+]?0x[0-9a-fA-F_]+|[-+]?[1-9][0-9_]*(?::[0-5]?[0-9])+)$`)
	yaml11Float = regexp.MustCompile(`^(?:[-+]?(?:[0-9][0-9_]*)\.[0-9_]*(?:[eE][-+]?[0-9]+)?|[-+]?(?:[0-9][0-9_]*)(?:[eE][-+]?[0-9]+)|\.[0-9_]+(?:[eE][-+][0-9]+)?|[-+]?[0-9][0-9_]*(?::[0-5]?[0-9])+\.[0-9_]*|[-+]?\.(?:inf|Inf|INF)|\.(?:nan|NaN|NAN))$`)
	yaml11Date  = regexp.MustCompile(`^([0-9]{4})-([0-9]{1,2})-([0-9]{1,2})(?:(?:[Tt]|[ \t]+)([0-9]{1,2}):([0-9]{2}):([0-9]{2})(?:\.([0-9]*))?(?:[ \t]*(Z|[-+][0-9]{1,2}(?::[0-9]{2})?))?)?$`)
)

// Parses one YAML document with YAML 1.1 scalar rules, nil for an empty document
func loadYAML11(data []byte) (any, error) {
	data = bytes.TrimPrefix(data, []byte("\xef\xbb\xbf"))
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, err
	}
	if len(doc.Content) == 0 {
		return nil, nil
	}
	return constructNode(doc.Content[0])
}

// Builds the Python value of a node, rejecting duplicate and unhashable keys like the safe constructor
func constructNode(n *yaml.Node) (any, error) {
	switch n.Kind {
	case yaml.AliasNode:
		return constructNode(n.Alias)
	case yaml.DocumentNode:
		if len(n.Content) == 0 {
			return nil, nil
		}
		return constructNode(n.Content[0])
	case yaml.SequenceNode:
		out := make([]any, 0, len(n.Content))
		for _, c := range n.Content {
			v, err := constructNode(c)
			if err != nil {
				return nil, err
			}
			out = append(out, v)
		}
		return out, nil
	case yaml.MappingNode:
		return constructMapping(n)
	}
	return constructScalar(n)
}

// Mapping with merge keys applied first and the node's own keys winning
func constructMapping(n *yaml.Node) (any, error) {
	var merged pyMap
	var own pyMap
	for i := 0; i+1 < len(n.Content); i += 2 {
		kn, vn := n.Content[i], n.Content[i+1]
		if kn.Kind == yaml.ScalarNode && kn.Value == "<<" && kn.Style&(yaml.SingleQuotedStyle|yaml.DoubleQuotedStyle) == 0 {
			src := vn
			if src.Kind == yaml.AliasNode {
				src = src.Alias
			}
			var sources []*yaml.Node
			switch src.Kind {
			case yaml.MappingNode:
				sources = []*yaml.Node{src}
			case yaml.SequenceNode:
				for _, s := range src.Content {
					if s.Kind == yaml.AliasNode {
						s = s.Alias
					}
					if s.Kind != yaml.MappingNode {
						return nil, fmt.Errorf("while constructing a mapping: expected a mapping for merging, but found %s", nodeKindName(s))
					}
					sources = append(sources, s)
				}
			default:
				return nil, fmt.Errorf("while constructing a mapping: expected a mapping or list of mappings for merging, but found %s", nodeKindName(src))
			}
			for _, s := range sources {
				m, err := constructMapping(s)
				if err != nil {
					return nil, err
				}
				for _, p := range m.(pyMap) {
					if merged.index(p.Key) < 0 {
						merged = append(merged, p)
					}
				}
			}
			continue
		}
		k, err := constructNode(kn)
		if err != nil {
			return nil, err
		}
		switch k.(type) {
		case []any, pyMap:
			return nil, fmt.Errorf("while constructing a mapping: found unhashable key at line %d", kn.Line)
		}
		if own.index(k) >= 0 {
			return nil, fmt.Errorf("while constructing a mapping: found duplicate key %q at line %d", pyrepr.Str(k), kn.Line)
		}
		v, err := constructNode(vn)
		if err != nil {
			return nil, err
		}
		own = append(own, pyPair{k, v})
	}
	out := pyMap{}
	for _, p := range merged {
		if own.index(p.Key) < 0 {
			out = append(out, p)
		}
	}
	return append(out, own...), nil
}

func nodeKindName(n *yaml.Node) string {
	switch n.Kind {
	case yaml.SequenceNode:
		return "sequence"
	case yaml.MappingNode:
		return "mapping"
	}
	return "scalar"
}

// Index of a key under Python equality, where True equals 1 and 1 equals 1.0
func (m pyMap) index(key any) int {
	for i, p := range m {
		if pyKeyEqual(p.Key, key) {
			return i
		}
	}
	return -1
}

func pyKeyEqual(a, b any) bool {
	if fa, ok := pyNumber(a); ok {
		fb, ok := pyNumber(b)
		return ok && fa.Cmp(fb) == 0
	}
	switch av := a.(type) {
	case string:
		bv, ok := b.(string)
		return ok && av == bv
	case nil:
		return b == nil
	case pyTimestamp:
		bv, ok := b.(pyTimestamp)
		return ok && av.t.Equal(bv.t) && av.isDate == bv.isDate
	}
	return false
}

// Numeric value of bools, ints and floats as a big float for cross type equality
func pyNumber(v any) (*big.Float, bool) {
	switch x := v.(type) {
	case bool:
		if x {
			return big.NewFloat(1), true
		}
		return big.NewFloat(0), true
	case int64:
		return new(big.Float).SetInt64(x), true
	case *big.Int:
		return new(big.Float).SetInt(x), true
	case float64:
		if math.IsNaN(x) || math.IsInf(x, 0) {
			return nil, false
		}
		return big.NewFloat(x), true
	}
	return nil, false
}

// Resolves a scalar by explicit tag, quoting style, then the implicit YAML 1.1 resolver
func constructScalar(n *yaml.Node) (any, error) {
	tag := ""
	if n.Style&yaml.TaggedStyle != 0 {
		tag = n.ShortTag()
	} else if n.Style&(yaml.SingleQuotedStyle|yaml.DoubleQuotedStyle|yaml.LiteralStyle|yaml.FoldedStyle) != 0 {
		return n.Value, nil
	}
	v := n.Value
	switch tag {
	case "!!str":
		return v, nil
	case "!!null":
		return nil, nil
	case "!!bool":
		if !yaml11Bool.MatchString(v) {
			return nil, fmt.Errorf("invalid boolean %q", v)
		}
		return yaml11True[v], nil
	case "!!int":
		return parseYAML11Int(v)
	case "!!float":
		return parseYAML11Float(v)
	case "!!timestamp":
		return parseYAML11Timestamp(v)
	case "":
	default:
		return nil, fmt.Errorf("could not determine a constructor for the tag %s", tag)
	}
	switch {
	case yaml11Null.MatchString(v):
		return nil, nil
	case yaml11Bool.MatchString(v):
		return yaml11True[v], nil
	case yaml11Int.MatchString(v):
		return parseYAML11Int(v)
	case yaml11Float.MatchString(v):
		return parseYAML11Float(v)
	case yaml11Date.MatchString(v):
		return parseYAML11Timestamp(v)
	}
	return v, nil
}

// Integer in YAML 1.1 binary, octal, hex, decimal or base 60 form
func parseYAML11Int(s string) (any, error) {
	s = strings.ReplaceAll(s, "_", "")
	sign := int64(1)
	if strings.HasPrefix(s, "-") {
		sign, s = -1, s[1:]
	} else if strings.HasPrefix(s, "+") {
		s = s[1:]
	}
	val := new(big.Int)
	var ok bool
	switch {
	case s == "0":
		ok = true
	case strings.HasPrefix(s, "0b"):
		_, ok = val.SetString(s[2:], 2)
	case strings.HasPrefix(s, "0x"):
		_, ok = val.SetString(s[2:], 16)
	case strings.HasPrefix(s, "0"):
		_, ok = val.SetString(s[1:], 8)
	case strings.Contains(s, ":"):
		ok = true
		for _, part := range strings.Split(s, ":") {
			d, err := strconv.ParseInt(part, 10, 64)
			if err != nil {
				return nil, err
			}
			val.Mul(val, big.NewInt(60)).Add(val, big.NewInt(d))
		}
	default:
		_, ok = val.SetString(s, 10)
	}
	if !ok {
		return nil, fmt.Errorf("invalid integer %q", s)
	}
	val.Mul(val, big.NewInt(sign))
	if val.IsInt64() {
		return val.Int64(), nil
	}
	return val, nil
}

// Float in YAML 1.1 decimal, exponent, base 60, infinity or NaN form
func parseYAML11Float(s string) (any, error) {
	s = strings.ReplaceAll(s, "_", "")
	sign := 1.0
	if strings.HasPrefix(s, "-") {
		sign, s = -1, s[1:]
	} else if strings.HasPrefix(s, "+") {
		s = s[1:]
	}
	low := strings.ToLower(s)
	switch {
	case low == ".inf":
		return sign * math.Inf(1), nil
	case low == ".nan":
		return math.NaN(), nil
	case strings.Contains(s, ":"):
		val := 0.0
		for _, part := range strings.Split(s, ":") {
			d, err := strconv.ParseFloat(part, 64)
			if err != nil {
				return nil, err
			}
			val = val*60 + d
		}
		return sign * val, nil
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return nil, err
	}
	return sign * f, nil
}

// Date or datetime scalar with its fraction and timezone preserved for rendering
func parseYAML11Timestamp(s string) (any, error) {
	m := yaml11Date.FindStringSubmatch(strings.TrimSpace(s))
	if m == nil {
		return nil, fmt.Errorf("invalid timestamp %q", s)
	}
	atoi := func(x string) int { v, _ := strconv.Atoi(x); return v }
	if m[4] == "" {
		return pyTimestamp{t: time.Date(atoi(m[1]), time.Month(atoi(m[2])), atoi(m[3]), 0, 0, 0, 0, time.UTC), isDate: true}, nil
	}
	nanos := 0
	if m[7] != "" {
		frac := (m[7] + "000000")[:6]
		nanos = atoi(frac) * 1000
	}
	loc := time.UTC
	hasTZ := m[8] != ""
	if hasTZ && m[8] != "Z" {
		parts := strings.SplitN(m[8][1:], ":", 2)
		off := atoi(parts[0]) * 3600
		if len(parts) == 2 {
			off += atoi(parts[1]) * 60
		}
		if m[8][0] == '-' {
			off = -off
		}
		loc = time.FixedZone("", off)
	}
	t := time.Date(atoi(m[1]), time.Month(atoi(m[2])), atoi(m[3]), atoi(m[4]), atoi(m[5]), atoi(m[6]), nanos, loc)
	return pyTimestamp{t: t, hasTZ: hasTZ, frac: nanos != 0}, nil
}

// Python str of a date or datetime
func (ts pyTimestamp) String() string {
	if ts.isDate {
		return ts.t.Format("2006-01-02")
	}
	out := ts.t.Format("2006-01-02 15:04:05")
	if ts.frac {
		out += fmt.Sprintf(".%06d", ts.t.Nanosecond()/1000)
	}
	if ts.hasTZ {
		out += ts.t.Format("-07:00")
	}
	return out
}

// Key and value pairs in insertion order so the mapping renders as a Python dict
func (m pyMap) PyItems() iter.Seq2[any, any] {
	return func(yield func(any, any) bool) {
		for _, p := range m {
			if !yield(p.Key, p.Value) {
				return
			}
		}
	}
}

// Python repr of a date or datetime with the timezone PyYAML attaches
func (ts pyTimestamp) PyRepr() string {
	t := ts.t
	if ts.isDate {
		return fmt.Sprintf("datetime.date(%d, %d, %d)", t.Year(), int(t.Month()), t.Day())
	}
	out := fmt.Sprintf("datetime.datetime(%d, %d, %d, %d, %d", t.Year(), int(t.Month()), t.Day(), t.Hour(), t.Minute())
	if us := t.Nanosecond() / 1000; us != 0 {
		out += fmt.Sprintf(", %d, %d", t.Second(), us)
	} else if t.Second() != 0 {
		out += fmt.Sprintf(", %d", t.Second())
	}
	if ts.hasTZ {
		_, off := t.Zone()
		if off == 0 {
			out += ", tzinfo=datetime.timezone.utc"
		} else {
			days, secs := off/86400, off%86400
			if secs < 0 {
				days, secs = days-1, secs+86400
			}
			delta := fmt.Sprintf("seconds=%d", secs)
			if days != 0 {
				delta = fmt.Sprintf("days=%d, %s", days, delta)
			}
			out += ", tzinfo=datetime.timezone(datetime.timedelta(" + delta + "))"
		}
	}
	return out + ")"
}
