package ruamelyaml

import (
	"regexp"
	"strings"
)

type implicitResolver struct {
	versions []version
	tag      string
	re       *regexp.Regexp
	first    string
}

// Implicit resolvers in ruamel's declaration order, each with its versions and first characters
var implicitResolvers = []implicitResolver{
	{[]version{v12}, "tag:yaml.org,2002:bool", regexp.MustCompile(`^(?:true|True|TRUE|false|False|FALSE)$`), "tTfF"},
	{[]version{v11}, "tag:yaml.org,2002:bool", regexp.MustCompile(`^(?:y|Y|yes|Yes|YES|n|N|no|No|NO|true|True|TRUE|false|False|FALSE|on|On|ON|off|Off|OFF)$`), "yYnNtTfFoO"},
	{[]version{v12}, "tag:yaml.org,2002:float", regexp.MustCompile(`^(?:[-+]?(?:[0-9][0-9_]*)\.[0-9_]*(?:[eE][-+]?[0-9]+)?|[-+]?(?:[0-9][0-9_]*)(?:[eE][-+]?[0-9]+)|[-+]?\.[0-9_]+(?:[eE][-+][0-9]+)?|[-+]?\.(?:inf|Inf|INF)|\.(?:nan|NaN|NAN))$`), "-+0123456789."},
	{[]version{v11}, "tag:yaml.org,2002:float", regexp.MustCompile(`^(?:[-+]?(?:[0-9][0-9_]*)\.[0-9_]*(?:[eE][-+]?[0-9]+)?|[-+]?(?:[0-9][0-9_]*)(?:[eE][-+]?[0-9]+)|\.[0-9_]+(?:[eE][-+][0-9]+)?|[-+]?[0-9][0-9_]*(?::[0-5]?[0-9])+\.[0-9_]*|[-+]?\.(?:inf|Inf|INF)|\.(?:nan|NaN|NAN))$`), "-+0123456789."},
	{[]version{v12}, "tag:yaml.org,2002:int", regexp.MustCompile(`^(?:[-+]?0b[0-1_]+|[-+]?0o?[0-7_]+|[-+]?[0-9_]+|[-+]?0x[0-9a-fA-F_]+)$`), "-+0123456789"},
	{[]version{v11}, "tag:yaml.org,2002:int", regexp.MustCompile(`^(?:[-+]?0b[0-1_]+|[-+]?0?[0-7_]+|[-+]?(?:0|[1-9][0-9_]*)|[-+]?0x[0-9a-fA-F_]+|[-+]?[1-9][0-9_]*(?::[0-5]?[0-9])+)$`), "-+0123456789"},
	{[]version{v12, v11}, "tag:yaml.org,2002:merge", regexp.MustCompile(`^(?:<<)$`), "<"},
	{[]version{v12, v11}, "tag:yaml.org,2002:null", regexp.MustCompile(`^(?:~|null|Null|NULL|)$`), "~nN\x00"},
	{[]version{v12, v11}, "tag:yaml.org,2002:timestamp", regexp.MustCompile(`^(?:[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]|[0-9][0-9][0-9][0-9]-[0-9][0-9]?-[0-9][0-9]?(?:[Tt]|[ \t]+)[0-9][0-9]?:[0-9][0-9]:[0-9][0-9](?:\.[0-9]*)?(?:[ \t]*(?:Z|[-+][0-9][0-9]?(?::[0-9][0-9])?))?)$`), "0123456789"},
	{[]version{v12, v11}, "tag:yaml.org,2002:value", regexp.MustCompile(`^(?:=)$`), "="},
	{[]version{v12, v11}, "tag:yaml.org,2002:yaml", regexp.MustCompile(`^(?:!|&|\*)$`), "!&*"},
}

// Python re.match of an anchored pattern, its $ also matching before one trailing newline
func PyFullMatch(re *regexp.Regexp, s string) bool {
	return re.MatchString(s) || (strings.HasSuffix(s, "\n") && re.MatchString(s[:len(s)-1]))
}

// Implicit tag of a plain scalar, keyed by first character the way VersionedResolver selects resolvers
func resolveScalar(v version, value string) (string, bool) {
	first := "\x00"
	if value != "" {
		first = string([]rune(value)[0])
	}
	for _, ir := range implicitResolvers {
		hasVersion := false
		for _, iv := range ir.versions {
			if iv.eq(v) {
				hasVersion = true
			}
		}
		if !hasVersion || !strings.Contains(ir.first, first) || (first == "\x00" && value != "") {
			continue
		}
		if PyFullMatch(ir.re, value) {
			return ir.tag, true
		}
	}
	return "", false
}

const (
	TagStr = "tag:yaml.org,2002:str"
	TagSeq = "tag:yaml.org,2002:seq"
	TagMap = "tag:yaml.org,2002:map"
)

// Tag the resolver gives a node of kind, plain scalars matched against the version's implicit resolvers
func resolve(v version, kind NodeKind, value string, implicit [2]bool) string {
	if kind == ScalarNode && implicit[0] {
		if !v.eq(v11) && !v.eq(v12) {
			panic(Raise("KeyError", v.String()))
		}
		if tag, ok := resolveScalar(v, value); ok {
			return tag
		}
	}
	switch kind {
	case SequenceNode:
		return TagSeq
	case MappingNode:
		return TagMap
	}
	return TagStr
}
