package config

import (
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// Raised for a write that would create a nested key beside an existing key holding a dot
var ErrPhantomSibling = errors.New("nested key would shadow a literal dotted key")

// Raised for a path that runs into a value it cannot descend into
var ErrNotNavigable = errors.New("key path runs into a value it cannot descend into")

// Segments of a dotted key path, a backslash before a dot keeping that dot inside the segment
func SplitKeyPath(key string) []string {
	var parts []string
	var cur strings.Builder
	for i := 0; i < len(key); i++ {
		switch {
		case key[i] == '\\' && i+1 < len(key) && key[i+1] == '.':
			cur.WriteByte('.')
			i++
		case key[i] == '.':
			parts = append(parts, cur.String())
			cur.Reset()
		default:
			cur.WriteByte(key[i])
		}
	}
	return append(parts, cur.String())
}

// Dotted path with each dot inside a segment escaped, the inverse of the split
func JoinKeyPath(parts ...string) string {
	escaped := make([]string, len(parts))
	for i, p := range parts {
		escaped[i] = strings.ReplaceAll(p, ".", "\\.")
	}
	return strings.Join(escaped, ".")
}

// Value index of key in a mapping value tree, minus one when absent
func mappingIndex(m *yaml.Node, key string) int {
	if m == nil || m.Kind != yaml.MappingNode {
		return -1
	}
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return i + 1
		}
	}
	return -1
}

// Longest dot joined run of the leading parts that names an existing key of m, with the parts it used
func greedyLiteralMatch(m *yaml.Node, parts []string) (string, int, bool) {
	if m == nil || m.Kind != yaml.MappingNode {
		return "", 0, false
	}
	for n := len(parts); n > 0; n-- {
		if k := strings.Join(parts[:n], "."); mappingIndex(m, k) >= 0 {
			return k, n, true
		}
	}
	return "", 0, false
}

// Existing key holding a dot that a new intermediate mapping named part would shadow
func phantomSibling(m *yaml.Node, part string) (string, bool) {
	prefix := part + "."
	for i := 0; i+1 < len(m.Content); i += 2 {
		if strings.HasPrefix(m.Content[i].Value, prefix) {
			return m.Content[i].Value, true
		}
	}
	return "", false
}

// List position a path segment names, read like an integer literal with negative positions counted from the end
func listIndex(seq *yaml.Node, part string) (int, bool) {
	text := strings.TrimSpace(part)
	if strings.Contains(text, "__") || strings.HasPrefix(strings.TrimLeft(text, "+-"), "_") || strings.HasSuffix(text, "_") {
		return 0, false
	}
	i, err := strconv.Atoi(strings.ReplaceAll(text, "_", ""))
	if err != nil {
		return 0, false
	}
	if i < 0 {
		i += len(seq.Content)
	}
	if i < 0 || i >= len(seq.Content) {
		return i, false
	}
	return i, true
}

// One step down a value tree, the container and the index of the value it holds
type pathHop struct {
	container *yaml.Node
	index     int
}

// Containers above and the slot of the value a split path names, ok false when any step is missing
func locateNested(root *yaml.Node, parts []string) ([]pathHop, pathHop, bool) {
	var parents []pathHop
	cur := root
	for i := 0; i < len(parts); {
		var hop pathHop
		consumed := 1
		switch {
		case cur == nil:
			return nil, pathHop{}, false
		case cur.Kind == yaml.SequenceNode:
			idx, ok := listIndex(cur, parts[i])
			if !ok {
				return nil, pathHop{}, false
			}
			hop = pathHop{cur, idx}
		case cur.Kind == yaml.MappingNode:
			key, n, ok := greedyLiteralMatch(cur, parts[i:])
			if !ok {
				return nil, pathHop{}, false
			}
			hop, consumed = pathHop{cur, mappingIndex(cur, key)}, n
		default:
			return nil, pathHop{}, false
		}
		i += consumed
		if i == len(parts) {
			return parents, hop, true
		}
		parents = append(parents, hop)
		cur = hop.container.Content[hop.index]
	}
	return nil, pathHop{}, false
}

// Value at a dotted path of a value tree, the greedy literal key match preferred over splitting
func getNested(root *yaml.Node, dotted string) (*yaml.Node, bool) {
	_, hop, ok := locateNested(root, SplitKeyPath(dotted))
	if !ok {
		return nil, false
	}
	return hop.container.Content[hop.index], true
}

// Removes the value at a dotted path and every mapping left empty above it, reporting whether it existed
func unsetNested(root *yaml.Node, dotted string) bool {
	parents, hop, ok := locateNested(root, SplitKeyPath(dotted))
	if !ok {
		return false
	}
	removeSlot(hop)
	current := hop.container
	for i := len(parents) - 1; i >= 0; i-- {
		if current.Kind != yaml.MappingNode || len(current.Content) > 0 {
			break
		}
		removeSlot(parents[i])
		current = parents[i].container
	}
	return true
}

// Deletes the value a hop names, the key with it for a mapping
func removeSlot(h pathHop) {
	c := h.container
	if c.Kind == yaml.MappingNode {
		c.Content = append(c.Content[:h.index-1], c.Content[h.index+1:]...)
		return
	}
	c.Content = append(c.Content[:h.index], c.Content[h.index+1:]...)
}

// Puts value under key of a mapping, keeping the position of an existing key
func putKey(m *yaml.Node, key string, value *yaml.Node) {
	if i := mappingIndex(m, key); i >= 0 {
		m.Content[i] = value
		return
	}
	m.Content = append(m.Content, keyNode(key), value)
}

// Empty mapping value tree
func emptyMapping() *yaml.Node { return &yaml.Node{Kind: yaml.MappingNode, Tag: tagMap} }

// Sets the value at a dotted path, creating mappings on the way, indexing existing list positions, refusing phantom siblings
func setNested(root *yaml.Node, dotted string, value *yaml.Node) error {
	parts := SplitKeyPath(dotted)
	cur := root
	for i := 0; i < len(parts); {
		remaining := parts[i:]
		leaf := len(remaining) == 1
		switch cur.Kind {
		case yaml.SequenceNode:
			idx, ok := listIndex(cur, remaining[0])
			if !ok {
				return fmt.Errorf("%w: segment %q of %q is not a position in the list", ErrNotNavigable, remaining[0], dotted)
			}
			if leaf {
				cur.Content[idx] = value
				return nil
			}
			cur = cur.Content[idx]
			i++
		case yaml.MappingNode:
			if key, n, ok := greedyLiteralMatch(cur, remaining); ok {
				at := mappingIndex(cur, key)
				if i+n == len(parts) {
					cur.Content[at] = value
					return nil
				}
				if next := cur.Content[at]; next.Kind != yaml.MappingNode && next.Kind != yaml.SequenceNode {
					cur.Content[at] = emptyMapping()
				}
				cur = cur.Content[at]
				i += n
				continue
			}
			if leaf {
				putKey(cur, remaining[0], value)
				return nil
			}
			if shadowed, ok := phantomSibling(cur, remaining[0]); ok {
				return fmt.Errorf("%w: refusing to create nested key %q in %q because the mapping already contains the literal key %q, "+
					"escape its dots with a backslash (e.g. %s) if that key was meant", ErrPhantomSibling, remaining[0], dotted, shadowed,
					strings.ReplaceAll(shadowed, ".", "\\."))
			}
			next := emptyMapping()
			putKey(cur, remaining[0], next)
			cur = next
			i++
		default:
			return fmt.Errorf("%w: %s at %q", ErrNotNavigable, kindName(cur), dotted)
		}
	}
	return nil
}

// Sets or removes one dotted key as single key writes do, false when a removal finds no parent to write
func writeNested(root *yaml.Node, dotted string, value *yaml.Node) bool {
	parts := SplitKeyPath(dotted)
	cur := root
	for i := 0; i < len(parts); {
		remaining := parts[i:]
		seg, n, ok := greedyLiteralMatch(cur, remaining)
		if !ok {
			seg, n = remaining[0], 1
		}
		if i+n == len(parts) {
			if value == nil {
				if at := mappingIndex(cur, seg); at >= 0 {
					removeSlot(pathHop{cur, at})
				}
				return true
			}
			putKey(cur, seg, value)
			return true
		}
		at := mappingIndex(cur, seg)
		if at < 0 || cur.Content[at].Kind != yaml.MappingNode {
			if value == nil {
				return false
			}
			next := emptyMapping()
			putKey(cur, seg, next)
			at = mappingIndex(cur, seg)
		}
		cur = cur.Content[at]
		i += n
	}
	return true
}

// Credential named keys a config set routes to the env file instead of config.yaml
func envConfigKeys() []string {
	return []string{
		"OPENROUTER_API_KEY", "OPENAI_API_KEY", "ANTHROPIC_API_KEY", "VOICE_TOOLS_OPENAI_KEY",
		"EXA_API_KEY", "PARALLEL_API_KEY", "FIRECRAWL_API_KEY", "FIRECRAWL_API_URL",
		"FIRECRAWL_GATEWAY_URL", "TOOL_GATEWAY_URL", "CONNECTOR_GATEWAY_URL",
		"TOOL_GATEWAY_DOMAIN", "TOOL_GATEWAY_SCHEME",
		"TOOL_GATEWAY_USER_TOKEN", "TAVILY_API_KEY", "PERPLEXITY_API_KEY", "API_SERVER_KEY",
		"BROWSERBASE_API_KEY", "BROWSERBASE_PROJECT_ID", "BROWSER_USE_API_KEY",
		"FAL_KEY", "TELEGRAM_BOT_TOKEN", "DISCORD_BOT_TOKEN",
		"TERMINAL_SSH_HOST", "TERMINAL_SSH_USER", "TERMINAL_SSH_KEY",
		"SUDO_PASSWORD", "SLACK_BOT_TOKEN", "SLACK_APP_TOKEN",
		"GITHUB_TOKEN", "HONCHO_API_KEY",
	}
}

// Reports a credential shaped key a config set stores in the env file, never a dotted config path
func IsEnvConfigKey(key string) bool {
	if strings.Contains(key, ".") {
		return false
	}
	upper := strings.ToUpper(key)
	return slices.Contains(envConfigKeys(), upper) || strings.HasSuffix(upper, "_API_KEY") || strings.HasSuffix(upper, "_TOKEN") ||
		strings.HasSuffix(upper, "_SECRET") || strings.HasPrefix(upper, "TERMINAL_SSH")
}

// Command line text of a config value, JSON when asked, booleans and null as words, collections in block YAML
func FormatValue(n *yaml.Node, asJSON bool) string {
	n = resolveAlias(n)
	if asJSON {
		return jsonText(orNull(n), false)
	}
	if n != nil && (n.Kind == yaml.MappingNode || n.Kind == yaml.SequenceNode) {
		return strings.TrimRight(safeStyle().renderDocument(n), " \t\n\r\v\f")
	}
	tag, text := scalarKind(orNull(n))
	switch tag {
	case tagNull:
		return "null"
	case tagBool:
		v, _ := resolveTagged(tagBool, text)
		return v
	}
	return displayText(n)
}
