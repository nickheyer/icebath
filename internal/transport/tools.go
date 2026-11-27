package transport

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"maps"
	"slices"
)

// Prefix of the names tools are sent under when the provider keeps theirs for its own tools
const reservedPrefix = "icebath_"

// Name of a tool in either shape, the function of an OpenAI tool or the tool itself
func toolName(tool map[string]any) string {
	if function, ok := tool["function"].(map[string]any); ok {
		name, _ := function["name"].(string)
		return name
	}
	name, _ := tool["name"].(string)
	return name
}

// Tools whose names the provider keeps for its own sent under other names, icebath_ and the name, numbered when that is taken; with the
// names sent mapped to their own. The tools given are left as they were
func Reserved(tools []map[string]any, reserved []string) ([]map[string]any, map[string]string) {
	aliases := map[string]string{}
	if len(tools) == 0 || len(reserved) == 0 {
		return tools, aliases
	}
	taken := map[string]bool{}
	for _, tool := range tools {
		taken[toolName(tool)] = true
	}
	out := make([]map[string]any, 0, len(tools))
	for _, tool := range tools {
		name := toolName(tool)
		if !slices.Contains(reserved, name) {
			out = append(out, tool)
			continue
		}
		alias := reservedPrefix + name
		for n := 2; taken[alias]; n++ {
			alias = fmt.Sprintf("%s%s_%d", reservedPrefix, name, n)
		}
		taken[alias] = true
		aliases[alias] = name
		out = append(out, Renamed(tool, alias))
	}
	return out, aliases
}

// Copy of a tool under another name, in whichever shape it has
func Renamed(tool map[string]any, name string) map[string]any {
	copied := maps.Clone(tool)
	if function, ok := tool["function"].(map[string]any); ok {
		function = maps.Clone(function)
		function["name"] = name
		copied["function"] = function
		return copied
	}
	copied["name"] = name
	return copied
}

// Id of a tool call that came without one, the same for the same call
func CallID(name, arguments string, index int) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s:%s:%d", name, arguments, index)))
	return "call_" + hex.EncodeToString(sum[:])[:12]
}
