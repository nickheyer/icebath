package skills

import (
	"fmt"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"go.yaml.in/yaml/v3"

	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/pkg/strs"
)

// Frontmatter of a SKILL.md: YAML read with YAML 1.1 scalars, mappings as map[string]any, lists as []any
type Frontmatter map[string]any

// Keys the icebath metadata of a frontmatter is read from, in order, the second the spelling skills written before the rename carry
var metadataKeys = []string{"icebath", "hermes"}

var (
	frontmatterEnd = regexp.MustCompile(`\n---[ \t]*\n`)
	yamlTrue       = regexp.MustCompile(`^(?i:y|yes|true|on)$`)
	yamlFalse      = regexp.MustCompile(`^(?i:n|no|false|off)$`)
	yamlInt        = regexp.MustCompile(`^[-+]?(?:0|[1-9][0-9_]*)$`)
	yamlFloat      = regexp.MustCompile(`^[-+]?(?:[0-9][0-9_]*)?\.[0-9_]*(?:[eE][-+]?[0-9]+)?$`)
)

// Decodes YAML into plain maps, lists and scalars, plain scalars typed by YAML 1.1 rules so yes and on are true as skills expect
func decodeYAML(text string) (any, error) {
	var node yaml.Node
	if err := yaml.Unmarshal([]byte(text), &node); err != nil {
		return nil, err
	}
	if node.Kind == 0 {
		return nil, nil
	}
	return yamlValue(&node), nil
}

func yamlValue(n *yaml.Node) any {
	switch n.Kind {
	case yaml.DocumentNode:
		if len(n.Content) == 0 {
			return nil
		}
		return yamlValue(n.Content[0])
	case yaml.MappingNode:
		out := map[string]any{}
		for i := 0; i+1 < len(n.Content); i += 2 {
			out[text(yamlValue(n.Content[i]))] = yamlValue(n.Content[i+1])
		}
		return out
	case yaml.SequenceNode:
		out := make([]any, 0, len(n.Content))
		for _, child := range n.Content {
			out = append(out, yamlValue(child))
		}
		return out
	case yaml.AliasNode:
		if n.Alias != nil {
			return yamlValue(n.Alias)
		}
		return nil
	case yaml.ScalarNode:
		return yamlScalar(n)
	}
	return nil
}

// One scalar: quoted ones stay text, plain ones follow YAML 1.1 types
func yamlScalar(n *yaml.Node) any {
	if n.Style&(yaml.DoubleQuotedStyle|yaml.SingleQuotedStyle|yaml.LiteralStyle|yaml.FoldedStyle) != 0 {
		return n.Value
	}
	v := n.Value
	switch n.Tag {
	case "!!int":
		if i, err := strconv.ParseInt(strings.ReplaceAll(v, "_", ""), 0, 64); err == nil {
			return i
		}
		return v
	case "!!float":
		if f, err := strconv.ParseFloat(strings.ReplaceAll(v, "_", ""), 64); err == nil {
			return f
		}
		return v
	}
	switch {
	case v == "" || v == "~" || strings.EqualFold(v, "null"):
		return nil
	case yamlTrue.MatchString(v):
		return true
	case yamlFalse.MatchString(v):
		return false
	case n.Tag == "!!str":
		return v
	case yamlInt.MatchString(v):
		if i, err := strconv.ParseInt(strings.ReplaceAll(v, "_", ""), 10, 64); err == nil {
			return i
		}
	case yamlFloat.MatchString(v):
		if f, err := strconv.ParseFloat(strings.ReplaceAll(v, "_", ""), 64); err == nil {
			return f
		}
	}
	return v
}

// Splits YAML frontmatter from a markdown body; YAML that does not parse falls back to key: value lines
func Parse(content string) (Frontmatter, string) {
	content = strings.TrimPrefix(content, "\uFEFF")
	if !strings.HasPrefix(content, "---") {
		return Frontmatter{}, content
	}
	end := frontmatterEnd.FindStringIndex(content[3:])
	if end == nil {
		return Frontmatter{}, content
	}
	head, body := content[3:end[0]+3], content[end[1]+3:]
	parsed, err := decodeYAML(head)
	if err == nil {
		fm, _ := parsed.(map[string]any)
		if fm == nil {
			fm = map[string]any{}
		}
		return fm, body
	}
	fm := Frontmatter{}
	for _, line := range strings.Split(strings.TrimSpace(head), "\n") {
		if key, value, ok := strings.Cut(line, ":"); ok {
			fm[strings.TrimSpace(key)] = strings.TrimSpace(value)
		}
	}
	return fm, body
}

// Text of a scalar: a string, a number or a bool as YAML prints it; nothing for null, a list or a mapping
func text(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case bool:
		return strconv.FormatBool(t)
	case int64:
		return strconv.FormatInt(t, 10)
	case int:
		return strconv.Itoa(t)
	case float64:
		return strconv.FormatFloat(t, 'g', -1, 64)
	}
	return ""
}

// Text of a string value, empty for anything else
func str(v any) string {
	s, _ := v.(string)
	return s
}

// Mapping of a value, nil when it is not one
func mapping(v any) map[string]any {
	switch t := v.(type) {
	case map[string]any:
		return t
	case Frontmatter:
		return t
	}
	return nil
}

// Stripped non empty items of a scalar or list value
func list(v any) []string {
	switch t := v.(type) {
	case string:
		if s := strings.TrimSpace(t); s != "" {
			return []string{s}
		}
	case []any:
		out := make([]string, 0, len(t))
		for _, item := range t {
			if s := strings.TrimSpace(text(item)); s != "" {
				out = append(out, s)
			}
		}
		return out
	case []string:
		out := make([]string, 0, len(t))
		for _, item := range t {
			if s := strings.TrimSpace(item); s != "" {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

// The icebath metadata mapping, the older spelling accepted
func (fm Frontmatter) Meta() map[string]any {
	meta := mapping(fm["metadata"])
	for _, key := range metadataKeys {
		if m := mapping(meta[key]); m != nil {
			return m
		}
	}
	return map[string]any{}
}

// Declared name, the fallback when there is none
func (fm Frontmatter) Name(fallback string) string {
	if v, ok := fm["name"]; ok {
		return text(v)
	}
	return fallback
}

// Description stripped of quotes, empty when unset
func (fm Frontmatter) Description() string {
	if !config.Truthy(fm["description"]) {
		return ""
	}
	return strings.Trim(strings.TrimSpace(text(fm["description"])), `'"`)
}

// Keys of the icebath metadata that make a skill show only when tools are or aren't there, or on some surfaces
var conditionKeys = []string{"fallback_for_toolsets", "requires_toolsets", "fallback_for_tools", "requires_tools", "session_platforms"}

// Conditional activation lists, absent keys empty
func (fm Frontmatter) conditions() map[string][]string {
	meta := fm.Meta()
	out := map[string][]string{}
	for _, key := range conditionKeys {
		out[key] = list(meta[key])
	}
	return out
}

// Tags from a list, a bracketed string or a comma separated one
func tags(v any) []string {
	out := []string{}
	if !config.Truthy(v) {
		return out
	}
	if items, ok := v.([]any); ok {
		for _, item := range items {
			if config.Truthy(item) {
				out = append(out, strings.TrimSpace(text(item)))
			}
		}
		return out
	}
	s := strings.TrimSpace(text(v))
	if strings.HasPrefix(s, "[") && strings.HasSuffix(s, "]") {
		s = s[1 : len(s)-1]
	}
	for _, tag := range strings.Split(s, ",") {
		if tag = strings.TrimSpace(tag); tag != "" {
			out = append(out, strings.Trim(tag, `"'`))
		}
	}
	return out
}

// Width of a description in the skills index of the system prompt
const IndexDescription = 60

// Longest name and description a listing shows
const (
	MaxName        = 64
	MaxDescription = 1024
)

const spaceClass = `\t\n\v\f\r \x1c-\x1f\x85\p{Z}`

var (
	lastSpace = regexp.MustCompile(`[` + spaceClass + `][^` + spaceClass + `]*\n?\z`)
	clipTrail = regexp.MustCompile(`[` + spaceClass + `.,;:\-–—(/]+\n?\z`)
)

// Description fitted to the index width, cut at a word with an ellipsis
func clip(description string) string {
	runes := []rune(description)
	if len(runes) <= IndexDescription {
		return description
	}
	head := string(runes[:IndexDescription-2])
	at := lastSpace.FindStringIndex(head)
	if at == nil || at[0] == 0 {
		return string(runes[:IndexDescription-3]) + "..."
	}
	return clipTrail.ReplaceAllString(head[:at[0]], "") + "..."
}

// Description capped at the listing limit
func capDescription(description string) string {
	if utf8.RuneCountInString(description) <= MaxDescription {
		return description
	}
	return string([]rune(description)[:MaxDescription-3]) + "..."
}

// First n runes of the text and the ellipsis when it was longer
func clipText(s string, n int, ellipsis string) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n]) + ellipsis
}

// Text of a file with a byte order mark dropped and bytes that are not UTF-8 replaced
func readText(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return decodeText(data), nil
}

func decodeText(data []byte) string {
	return strings.TrimPrefix(strings.ToValidUTF8(string(data), "\uFFFD"), "\uFEFF")
}

// Text of a file that must be UTF-8, a byte order mark dropped
func readStrict(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return strictText(data, path)
}

// Text of a file's bytes that must be UTF-8, a byte order mark dropped
func strictText(data []byte, path string) (string, error) {
	content := strings.TrimPrefix(string(data), "\uFEFF")
	if utf8.ValidString(content) {
		return content, nil
	}
	at := 0
	for at < len(content) {
		r, size := utf8.DecodeRuneInString(content[at:])
		if r == utf8.RuneError && size <= 1 {
			break
		}
		at += size
	}
	return "", fmt.Errorf("%s holds bytes that are not UTF-8 text, the first at byte %d", path, at)
}

// Declared name of a SKILL.md read from its first 4000 characters, the fallback when it has none
func readName(skillMD, fallback string) string {
	data, err := os.ReadFile(skillMD)
	if err != nil {
		return fallback
	}
	return nameIn(decodeText(data), fallback)
}

// Declared name in the first 4000 characters of a SKILL.md text, read line by line so a broken frontmatter still names its skill
func nameIn(content, fallback string) string {
	lines := strings.Split(strs.Head(content, 4000), "\n")
	for i := range lines {
		lines[i] = strings.TrimSpace(lines[i])
	}
	start := slices.Index(lines, "---")
	if start < 0 {
		return fallback
	}
	block := lines[start+1:]
	if end := slices.Index(block, "---"); end >= 0 {
		block = block[:end]
	}
	for _, line := range block {
		if value, ok := strings.CutPrefix(line, "name:"); ok {
			if value = strings.Trim(strings.TrimSpace(value), `"'`); value != "" {
				return value
			}
		}
	}
	return fallback
}
