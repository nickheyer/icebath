package config

import (
	"fmt"
	"math"
	"math/big"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// Reports text that plausibly spells a YAML or JSON list or mapping, a bare leading dash never counting
func looksStructured(value string) bool {
	stripped := strings.TrimLeftFunc(value, isEnvSpace)
	if strings.HasPrefix(stripped, "[") || strings.HasPrefix(stripped, "{") {
		return true
	}
	if !strings.Contains(value, "\n") {
		return false
	}
	for _, line := range tokenizerRows(value) {
		item := trimEnvSpace(line)
		if item == "-" || strings.HasPrefix(item, "- ") {
			return true
		}
		if head, _, found := strings.Cut(item, ": "); found && head != "" && !strings.Contains(head, " ") && !strings.HasPrefix(head, "#") {
			return true
		}
		if body, ok := strings.CutSuffix(item, ":"); ok && body != "" && !strings.Contains(body, " ") {
			return true
		}
	}
	return false
}

// Shape of a whole number literal once its digits are ASCII and its underscores are dropped
var wholeNumberShape = regexp.MustCompile(`^[+-]?[0-9]+$`)

// Shape of a decimal number literal once its digits are ASCII and its underscores are dropped
var decimalNumberShape = regexp.MustCompile(`^[+-]?(?:[0-9]+\.?[0-9]*|\.[0-9]+)(?:[eE][+-]?[0-9]+)?$`)

// Whole number a clean integer literal spells, signs, surrounding space and underscores between digits allowed
func wholeNumberLiteral(text string) (*big.Int, bool) {
	plain, ok := plainNumberText(trimEnvSpace(text))
	if !ok || !wholeNumberShape.MatchString(plain) {
		return nil, false
	}
	return new(big.Int).SetString(strings.TrimPrefix(plain, "+"), 10)
}

// Float a decimal literal spells when the conversion keeps its exact decimal value, never an infinity or not a number
func exactFloatLiteral(text string) (float64, bool) {
	plain, ok := plainNumberText(trimEnvSpace(text))
	if !ok || !decimalNumberShape.MatchString(plain) {
		return 0, false
	}
	f, err := strconv.ParseFloat(plain, 64)
	if err != nil || math.IsNaN(f) || math.IsInf(f, 0) {
		return 0, false
	}
	exact, ok := new(big.Rat).SetString(plain)
	if !ok {
		return 0, false
	}
	shortest, ok := new(big.Rat).SetString(strconv.FormatFloat(f, 'g', -1, 64))
	if !ok || exact.Cmp(shortest) != 0 {
		return 0, false
	}
	return f, true
}

// Words a config set reads as a boolean or as null, compared in lower case
func scalarWord(lower string) (*yaml.Node, bool) {
	switch lower {
	case "true", "yes", "on":
		return &yaml.Node{Kind: yaml.ScalarNode, Tag: tagBool, Value: "true"}, true
	case "false", "no", "off":
		return &yaml.Node{Kind: yaml.ScalarNode, Tag: tagBool, Value: "false"}, true
	case "null", "none", "~":
		return nullNode(), true
	}
	return nil, false
}

// Value text shaped like a list or mapping that does not parse, refused so no reader ignores it
type InvalidLiteralError struct {
	Key    string
	Detail string
}

// The refusal and the two ways out
func (e *InvalidLiteralError) Error() string {
	return fmt.Sprintf("✗ Value for '%s' looks like a list/mapping but is not valid YAML/JSON (%s) — nothing was written.\n"+
		"  Fix the literal, or quote it (e.g. \"'[text'\") to store a plain string.", e.Key, e.Detail)
}

// Matches ErrWriteRefused
func (e *InvalidLiteralError) Unwrap() error { return ErrWriteRefused }

// Value a command line text takes under a key, text for string settings and booleans, null, numbers and containers otherwise
func coerceSetValue(defaults *yaml.Node, key, value string) (*yaml.Node, error) {
	def := treeGet(defaults, SplitKeyPath(key)...)
	if isStringNode(def) && !(key == "model" && looksStructured(value)) {
		return stringNode(value), nil
	}
	stripped := trimEnvSpace(value)
	if n, ok := scalarWord(strings.ToLower(stripped)); ok {
		return n, nil
	}
	if v, ok := wholeNumberLiteral(stripped); ok {
		return &yaml.Node{Kind: yaml.ScalarNode, Tag: tagInt, Value: v.String()}, nil
	}
	if f, ok := exactFloatLiteral(stripped); ok {
		return &yaml.Node{Kind: yaml.ScalarNode, Tag: tagFloat, Value: floatText(f)}, nil
	}
	if !looksStructured(value) {
		return stringNode(value), nil
	}
	var parsed yaml.Node
	if err := LoadYAML([]byte(value), &parsed); err != nil {
		detail, _, _ := strings.Cut(errorDetails(err), "\n")
		return nil, &InvalidLiteralError{Key: key, Detail: detail}
	}
	if isMapping(&parsed) || isSequence(&parsed) {
		return &parsed, nil
	}
	return stringNode(value), nil
}

// Container kind the defaults, known container table or disk value fix a key to, empty for scalars
func expectedContainer(defaults, user *yaml.Node, key string) containerKind {
	parts := SplitKeyPath(key)
	schema := treeGet(defaults, parts...)
	if len(parts) == 1 && isMapping(schema) {
		schema = nil
	}
	existing, _ := getNested(user, key)
	kindOf := func(n *yaml.Node) containerKind {
		switch {
		case isMapping(n):
			return containerMapping
		case isSequence(n):
			return containerList
		}
		return ""
	}
	if k := kindOf(schema); k != "" {
		return k
	}
	if k, ok := knownContainerTypes()[key]; ok {
		return k
	}
	return kindOf(existing)
}

// Value of the wrong shape for a key the schema fixes to a list or a mapping
type ContainerTypeError struct {
	Key      string
	Expected string
	Got      string
}

// The refusal with a literal to pass instead
func (e *ContainerTypeError) Error() string {
	literal := "{key: value}"
	if e.Expected == string(containerList) {
		literal = "[item, ...]"
	}
	return fmt.Sprintf("✗ Cannot set '%s': it must be a %s, got a %s — nothing was written.\n"+
		"  Pass a YAML/JSON literal, e.g.:\n    icebath config set %s '%s'\n  or edit config.yaml directly.",
		e.Key, e.Expected, e.Got, e.Key, literal)
}

// Matches ErrWriteRefused
func (e *ContainerTypeError) Unwrap() error { return ErrWriteRefused }

// Value to store under a container key, a bare name becoming a one item list, a wrong shape refused
func guardContainerType(defaults, user *yaml.Node, key string, value *yaml.Node, force bool) (*yaml.Node, error) {
	expected := expectedContainer(defaults, user, key)
	if expected == "" {
		return value, nil
	}
	if expected == containerList && isStringNode(value) && slices.Contains(scalarAsOneItemListKeys(), key) {
		return &yaml.Node{Kind: yaml.SequenceNode, Tag: tagSeq, Content: []*yaml.Node{value}}, nil
	}
	ok := isMapping(value)
	if expected == containerList {
		ok = isSequence(value)
	}
	if ok || (expected == containerMapping && force) {
		return value, nil
	}
	got := "string"
	if !isStringNode(value) {
		got = typeName(value)
	}
	return nil, &ContainerTypeError{Key: key, Expected: string(expected), Got: got}
}

// Container given for the bare model key, which only takes a model id
type ModelShorthandError struct {
	Kind string
}

// The refusal with the keys to set instead
func (e *ModelShorthandError) Error() string {
	return fmt.Sprintf("✗ Cannot set 'model' to a %s — the bare 'model' shorthand takes a model id, and a container value has no slot there.\n"+
		"  Set the section's keys individually instead:\n"+
		"    icebath config set model.provider <provider>\n"+
		"    icebath config set model.default <model-id>\n"+
		"  Or replace the whole section deliberately:\n"+
		"    icebath config set --force model '{provider: <provider>, default: <model-id>}'", e.Kind)
}

// Matches ErrWriteRefused
func (e *ModelShorthandError) Unwrap() error { return ErrWriteRefused }

// Scalar given for a single segment key that names a whole config section
type SectionOverwriteError struct {
	Key     string
	SubKeys []string
	Value   string
}

// The refusal with the sub keys of the section and the two ways to proceed
func (e *SectionOverwriteError) Error() string {
	lines := []string{fmt.Sprintf("✗ Cannot set '%s' to a scalar — '%s' is a configuration section with %d sub-key(s).", e.Key, e.Key, len(e.SubKeys))}
	if len(e.SubKeys) > 0 {
		lines = append(lines, "  Sub-keys: "+strings.Join(e.SubKeys[:min(8, len(e.SubKeys))], ", "))
		if len(e.SubKeys) > 8 {
			lines = append(lines, fmt.Sprintf("  ... and %d more", len(e.SubKeys)-8))
		}
	}
	lines = append(lines,
		"  Use a dotted path to set a specific leaf key:",
		fmt.Sprintf("    icebath config set %s.<sub-key> <value>", e.Key),
		"  Or use --force to replace the entire section:",
		fmt.Sprintf("    icebath config set --force %s %s", e.Key, e.Value))
	return strings.Join(lines, "\n")
}

// Matches ErrWriteRefused
func (e *SectionOverwriteError) Unwrap() error { return ErrWriteRefused }

// Key to write over an existing section, bare model redirected to model.default, with the line to print
func guardSectionOverwrite(user *yaml.Node, key string, value *yaml.Node, force bool) (string, string, error) {
	existing := childOf(user, key)
	kind := "list"
	if isMapping(value) {
		kind = "mapping"
	}
	if key == "model" && !force && (isSequence(value) || (isMapping(value) && isMapping(existing))) {
		return "", "", &ModelShorthandError{Kind: kind}
	}
	if strings.Contains(key, ".") || !isMapping(existing) {
		return key, "", nil
	}
	count := len(existing.Content) / 2
	if key == "model" {
		if force {
			what := "a scalar"
			if isMapping(value) || isSequence(value) {
				what = "the given " + kind
			}
			return key, fmt.Sprintf("⚠ Replacing entire 'model' section with %s (discarding %d existing sub-key(s))", what, count), nil
		}
		return "model.default", fmt.Sprintf("✓ Redirecting bare 'model' to 'model.default' (preserving %d existing model sub-key(s))", count), nil
	}
	if force {
		return key, "", nil
	}
	return "", "", &SectionOverwriteError{Key: key, SubKeys: mappingKeys(existing), Value: literalText(value)}
}
