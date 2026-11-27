package transport

import (
	"maps"
	"slices"
	"strings"

	"github.com/nickheyer/icebath/internal/transport"
)

// Tools of the request shaped for the route: those whose names the kind keeps for its own sent under other names, and Moonshot models given
// the schema subset they take; with the names sent mapped to their own
func (c *client) tools(p *prepared, tools []map[string]any) ([]map[string]any, map[string]string) {
	if len(tools) == 0 {
		return nil, nil
	}
	tools, aliases := transport.Reserved(tools, slices.Concat(p.spec.ReservedTools, p.hosted.ReservedTools))
	if moonshot(c.route.Model) {
		repaired := make([]map[string]any, 0, len(tools))
		for _, tool := range tools {
			if _, ok := tool["function"].(map[string]any); !ok {
				repaired = append(repaired, tool)
				continue
			}
			repaired = append(repaired, withFunction(tool, func(function map[string]any) {
				function["parameters"] = moonshotParameters(function["parameters"])
			}))
		}
		tools = repaired
	}
	return tools, aliases
}

// Copy of a tool with its function changed, the original left as it was
func withFunction(tool map[string]any, change func(map[string]any)) map[string]any {
	copied := maps.Clone(tool)
	function, _ := tool["function"].(map[string]any)
	function = maps.Clone(function)
	if function == nil {
		function = map[string]any{}
	}
	change(function)
	copied["function"] = function
	return copied
}

// Whether the model is a Moonshot Kimi model, which takes a subset of JSON schema in its tools
func moonshot(model string) bool {
	if model == "" {
		return false
	}
	name := strings.ToLower(strings.TrimSpace(model))
	tail := name[strings.LastIndex(name, "/")+1:]
	return strings.HasPrefix(tail, "kimi-") || tail == "kimi" || tail == "k3" || strings.HasPrefix(tail, "k3.") || strings.HasPrefix(tail, "k3-") ||
		strings.Contains(name, "moonshot") || strings.Contains(name, "/kimi") || strings.HasPrefix(name, "kimi")
}

var (
	schemaMaps    = []string{"properties", "patternProperties", "$defs", "definitions"}
	schemaLists   = []string{"anyOf", "oneOf", "allOf", "prefixItems"}
	schemaNodes   = []string{"items", "contains", "not", "additionalProperties", "propertyNames", "if", "then", "else"}
	scalarSchemas = []string{"string", "integer", "number", "boolean"}
)

// Parameters of a tool in the subset of JSON schema Moonshot takes, an object always; the original left as it was
func moonshotParameters(parameters any) map[string]any {
	object, ok := transport.DeepCopy(parameters).(map[string]any)
	if !ok {
		return map[string]any{"type": "object", "properties": map[string]any{}, "required": []any{}}
	}
	repaired, _ := moonshotRepair(object).(map[string]any)
	repaired["type"] = "object"
	if _, ok := repaired["properties"]; !ok {
		repaired["properties"] = map[string]any{}
	}
	return moonshotRequired(repaired)
}

// One schema node rewritten into Moonshot's subset: nullable unions collapsed, a type filled in where it is missing, empty enum values
// dropped and object nodes given their required list
func moonshotRepair(node any) any {
	switch n := node.(type) {
	case []any:
		out := make([]any, len(n))
		for i, item := range n {
			out[i] = moonshotRepair(item)
		}
		return out
	case map[string]any:
		repaired := map[string]any{}
		for key, value := range n {
			_, isMap := value.(map[string]any)
			_, isList := value.([]any)
			switch {
			case slices.Contains(schemaMaps, key) && isMap:
				sub := map[string]any{}
				for name, child := range value.(map[string]any) {
					sub[name] = moonshotRepair(child)
				}
				repaired[key] = sub
			case slices.Contains(schemaLists, key) && isList, slices.Contains(schemaNodes, key) && isMap:
				repaired[key] = moonshotRepair(value)
			default:
				repaired[key] = value
			}
		}
		if anyOf, ok := repaired["anyOf"].([]any); ok {
			delete(repaired, "type")
			var branches []any
			for _, branch := range anyOf {
				object, ok := branch.(map[string]any)
				if !ok {
					continue
				}
				if kind, _ := object["type"].(string); kind == "null" {
					continue
				}
				branches = append(branches, branch)
			}
			switch {
			case len(branches) == 0 || len(branches) == len(anyOf):
				return repaired
			case len(branches) > 1:
				repaired["anyOf"] = branches
				return repaired
			}
			delete(repaired, "anyOf")
			if only, ok := branches[0].(map[string]any); ok {
				maps.Copy(repaired, only)
			}
		}
		delete(repaired, "nullable")
		if _, ref := repaired["$ref"]; !ref {
			moonshotType(repaired)
		}
		if enum, ok := repaired["enum"].([]any); ok && slices.Contains(scalarSchemas, transport.Text(repaired["type"])) {
			var cleaned []any
			for _, value := range enum {
				if value != nil && value != "" {
					cleaned = append(cleaned, value)
				}
			}
			if len(cleaned) > 0 {
				repaired["enum"] = cleaned
			} else {
				delete(repaired, "enum")
			}
		}
		if transport.Text(repaired["type"]) == "object" {
			return moonshotRequired(repaired)
		}
		return repaired
	}
	return node
}

// Object node whose required list names only properties it has, an empty one when it had none
func moonshotRequired(node map[string]any) map[string]any {
	properties, hasProperties := node["properties"].(map[string]any)
	required, ok := node["required"].([]any)
	if !ok {
		node["required"] = []any{}
		return node
	}
	if hasProperties {
		kept := []any{}
		for _, name := range required {
			if s, ok := name.(string); ok {
				if _, in := properties[s]; in {
					kept = append(kept, s)
				}
			}
		}
		node["required"] = kept
	}
	return node
}

// Type of a node that has none, told from what it holds: an object, an array, its enum values, else a string
func moonshotType(node map[string]any) {
	if list, ok := node["type"].([]any); ok {
		concrete := "string"
		for _, t := range list {
			if s, ok := t.(string); ok && s != "" && s != "null" {
				concrete = s
				break
			}
		}
		node["type"] = concrete
		return
	}
	if t, present := node["type"]; present && t != nil && t != "" {
		return
	}
	has := func(keys ...string) bool {
		return slices.ContainsFunc(keys, func(key string) bool { _, ok := node[key]; return ok })
	}
	switch {
	case has("properties", "required", "additionalProperties"):
		node["type"] = "object"
		return
	case has("items", "prefixItems"):
		node["type"] = "array"
		return
	}
	if enum, ok := node["enum"].([]any); ok && len(enum) > 0 {
		switch value := enum[0].(type) {
		case bool:
			node["type"] = "boolean"
		case float64:
			if value == float64(int64(value)) {
				node["type"] = "integer"
			} else {
				node["type"] = "number"
			}
		default:
			node["type"] = "string"
		}
		return
	}
	if has("if", "then", "else") {
		return
	}
	node["type"] = "string"
}

// Name a tool call came back under, as the agent knows the tool
func (p *prepared) unaliased(name string) string {
	if original, ok := p.aliases[name]; ok {
		return original
	}
	return name
}
