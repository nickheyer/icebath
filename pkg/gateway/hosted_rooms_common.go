package gateway

import (
	"math/big"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

var roomCommonIdentifierRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]*$`)

// Error constructor of one hosted room module, its own ValueError subclass
type roomErrorFunc func(msg string) error

// Shape options of one bounded identifier check
type roomIdentifierSpec struct {
	label    string
	maxChars int
	pattern  *regexp.Regexp
	anyShape bool
	invalid  string
}

// Stripped bounded string, the pattern defaulting to the common identifier shape unless anyShape is set
func roomIdentifier(value any, spec roomIdentifierSpec, fail roomErrorFunc) (string, error) {
	text, ok := value.(string)
	if !ok {
		return "", fail(spec.label + " must be a string")
	}
	text = pystr.Strip(text)
	maxChars := spec.maxChars
	if maxChars == 0 {
		maxChars = 128
	}
	pattern := spec.pattern
	if pattern == nil && !spec.anyShape {
		pattern = roomCommonIdentifierRE
	}
	if text == "" || utf8.RuneCountInString(text) > maxChars || (pattern != nil && !pattern.MatchString(text)) {
		if spec.invalid != "" {
			return "", fail(spec.invalid)
		}
		return "", fail("invalid " + spec.label)
	}
	return text, nil
}

// Integer that is no bool and lies within low and the optional high, message being the exact refusal
func roomBoundedInt(value any, fail roomErrorFunc, message string, low int64, high *big.Int) (*big.Int, error) {
	if _, isBool := value.(bool); !isBool {
		if n, ok := roomInt(value); ok && n.Cmp(big.NewInt(low)) >= 0 && (high == nil || n.Cmp(high) <= 0) {
			return n, nil
		}
	}
	return nil, fail(message)
}

// Python int value of a decoded JSON integer, false for floats and every other type
func roomInt(value any) (*big.Int, bool) {
	switch v := value.(type) {
	case int:
		return big.NewInt(int64(v)), true
	case int64:
		return big.NewInt(v), true
	case int32:
		return big.NewInt(int64(v)), true
	case *big.Int:
		return v, v != nil
	}
	return nil, false
}

// Message formats of one exact field check
type roomFieldFormats struct {
	notObject string
	missing   string
	unknown   string
}

// Requires exactly the required keys plus any optional ones, naming the offenders sorted
func roomExactFields(value any, label string, required, optional map[string]bool, fail roomErrorFunc, formats roomFieldFormats) (map[string]any, error) {
	fields, ok := hostedMapping(value)
	if !ok {
		if formats.notObject != "" {
			return nil, fail(formats.notObject)
		}
		return nil, fail(label + " must be an object")
	}
	var missing, unknown []string
	for key := range required {
		if _, present := fields[key]; !present {
			missing = append(missing, key)
		}
	}
	for key := range fields {
		if !required[key] && !optional[key] {
			unknown = append(unknown, key)
		}
	}
	sort.Strings(missing)
	sort.Strings(unknown)
	format := func(template string, names []string) string {
		return strings.NewReplacer("{label}", label, "{fields}", strings.Join(names, ", ")).Replace(template)
	}
	if len(missing) > 0 {
		template := formats.missing
		if template == "" {
			template = "{label} is missing fields: {fields}"
		}
		return nil, fail(format(template, missing))
	}
	if len(unknown) > 0 {
		template := formats.unknown
		if template == "" {
			template = "{label} has unknown fields: {fields}"
		}
		return nil, fail(format(template, unknown))
	}
	return fields, nil
}

// Sorted key separator free JSON, the digest and storage canonical form
func roomCompactJSON(value any, ensureASCII bool) (string, error) {
	return pyjson.Dumps(value, pyjson.SortKeys(true), pyjson.Separators(",", ":"), pyjson.EnsureASCII(ensureASCII))
}
