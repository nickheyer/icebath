package config

import (
	"fmt"
	"math"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// Reports a value the readers count as set, anything but null, false, zero, empty text and empty collections
func isSet(n *yaml.Node) bool {
	n = resolveAlias(n)
	if n == nil {
		return false
	}
	if n.Kind != yaml.ScalarNode {
		return len(n.Content) > 0
	}
	tag, text := scalarKind(n)
	switch tag {
	case tagNull:
		return false
	case tagBool:
		v, _ := resolveTagged(tagBool, text)
		return v == "true"
	case tagInt, tagFloat:
		f, ok := numericValue(tag, text)
		return !ok || f.Sign() != 0
	}
	return text != ""
}

// Text a value has once formatted as a string, booleans as True and False, numbers canonical, collections as literals
func displayText(n *yaml.Node) string {
	n = resolveAlias(n)
	switch {
	case n == nil || isNull(n):
		return "None"
	case n.Kind == yaml.MappingNode:
		parts := make([]string, 0, len(n.Content)/2)
		for i := 0; i+1 < len(n.Content); i += 2 {
			parts = append(parts, literalText(n.Content[i])+": "+literalText(n.Content[i+1]))
		}
		return "{" + strings.Join(parts, ", ") + "}"
	case n.Kind == yaml.SequenceNode:
		parts := make([]string, 0, len(n.Content))
		for _, it := range n.Content {
			parts = append(parts, literalText(it))
		}
		return "[" + strings.Join(parts, ", ") + "]"
	}
	tag, text := scalarKind(n)
	switch tag {
	case tagBool:
		if v, _ := resolveTagged(tagBool, text); v == "true" {
			return "True"
		}
		return "False"
	case tagInt:
		if v, ok := constructInt(text); ok {
			return v.String()
		}
	case tagFloat:
		if f, ok := constructFloat(text); ok {
			return floatWord(f)
		}
	case "!!timestamp":
		return timestampWord(text)
	}
	return text
}

// Text of a value written inside a collection literal, strings in quotes
func literalText(n *yaml.Node) string {
	if isStringNode(n) {
		_, text := scalarKind(resolveAlias(n))
		return quoted(text)
	}
	return displayText(n)
}

// Text of a value read with an empty fallback, a value the readers count as unset giving the empty string
func orText(n *yaml.Node) string {
	if !isSet(n) {
		return ""
	}
	return displayText(n)
}

// Float formatted as a string, infinities and not a number as inf, -inf and nan
func floatWord(f float64) string {
	switch {
	case math.IsNaN(f):
		return "nan"
	case math.IsInf(f, 1):
		return "inf"
	case math.IsInf(f, -1):
		return "-inf"
	}
	return reprFloat(f)
}

// Date or date and time a YAML 1.1 timestamp builds, formatted with a space separator and a numeric zone
func timestampWord(s string) string {
	m := timestampText.FindStringSubmatch(s)
	if m == nil {
		return s
	}
	num := func(t string) int { v, _ := strconv.Atoi(t); return v }
	out := fmt.Sprintf("%04d-%02d-%02d", num(m[1]), num(m[2]), num(m[3]))
	if m[4] == "" {
		return out
	}
	out += fmt.Sprintf(" %02d:%02d:%02d", num(m[4]), num(m[5]), num(m[6]))
	if frac := (m[7] + "000000")[:6]; num(frac) != 0 {
		out += "." + frac
	}
	switch zone := m[8]; {
	case zone == "Z":
		out += "+00:00"
	case zone != "":
		h, mm, _ := strings.Cut(zone[1:], ":")
		sign := zone[0]
		if num(h) == 0 && num(mm) == 0 {
			sign = '+'
		}
		out += fmt.Sprintf("%c%02d:%02d", sign, num(h), num(mm))
	}
	return out
}

// String value tree holding text
func stringNode(text string) *yaml.Node {
	return &yaml.Node{Kind: yaml.ScalarNode, Tag: tagStr, Value: text}
}

// Removes key from a mapping value tree when present
func dropKey(m *yaml.Node, key string) {
	if at := mappingIndex(m, key); at >= 0 {
		removeSlot(pathHop{m, at})
	}
}

// User tree with a root max_turns moved under agent while agent sets none, the move loading makes before merging
func moveRootMaxTurns(user *yaml.Node) *yaml.Node {
	turns := childOf(user, "max_turns")
	if mappingIndex(user, "max_turns") < 0 {
		return user
	}
	out := plainCopy(user)
	agent := childOf(out, "agent")
	if isMapping(agent) {
		agent = plainCopy(agent)
	} else {
		agent = emptyMapping()
	}
	if isNull(childOf(agent, "max_turns")) {
		putKey(agent, "max_turns", plainCopy(turns))
	}
	putKey(out, "agent", agent)
	dropKey(out, "max_turns")
	return out
}

// Load and save normalization, max_turns relocation then the model section canon
func canonicalize(config *yaml.Node) *yaml.Node {
	return normalizeRootModelKeys(normalizeMaxTurns(config))
}

// Config with a root max_turns moved under agent when agent names none, a sparse save never growing agent
func normalizeMaxTurns(config *yaml.Node) *yaml.Node {
	if !isMapping(config) {
		return config
	}
	agent := childOf(config, "agent")
	if isSet(agent) && !isMapping(agent) {
		return config
	}
	out := plainCopy(config)
	agentConfig := emptyMapping()
	if isMapping(agent) {
		agentConfig = plainCopy(agent)
	}
	if at := mappingIndex(out, "max_turns"); at >= 0 && mappingIndex(agentConfig, "max_turns") < 0 {
		putKey(agentConfig, "max_turns", plainCopy(out.Content[at]))
	}
	if len(agentConfig.Content) > 0 || mappingIndex(out, "agent") >= 0 {
		putKey(out, "agent", agentConfig)
	}
	dropKey(out, "max_turns")
	return out
}

// Config whose model section is canonical, root routing keys moved under model without overriding and aliases folded into default
func normalizeRootModelKeys(config *yaml.Node) *yaml.Node {
	if !isMapping(config) {
		return config
	}
	modelIn := childOf(config, "model")
	var provider *yaml.Node
	if isMapping(modelIn) {
		provider = childOf(modelIn, "provider")
	}
	needs := provider != nil && !isNull(provider) && !isStringNode(provider)
	if isMapping(modelIn) {
		needs = needs || isSet(childOf(modelIn, "api_base")) || isSet(childOf(modelIn, "model")) || isSet(childOf(modelIn, "name"))
		for _, k := range []string{"default", "model", "name"} {
			needs = needs || isMapping(childOf(modelIn, k))
		}
	}
	hasRoot := false
	for _, k := range []string{"provider", "base_url", "context_length", "api_base"} {
		hasRoot = hasRoot || isSet(childOf(config, k))
	}
	if !hasRoot && !needs {
		return config
	}
	out := plainCopy(config)
	model := emptyMapping()
	switch {
	case isMapping(modelIn):
		model = plainCopy(modelIn)
	case isSet(modelIn):
		putKey(model, "default", plainCopy(modelIn))
	}
	putKey(out, "model", model)
	for _, k := range []string{"default", "model", "name"} {
		v := childOf(model, k)
		if !isMapping(v) {
			continue
		}
		nested := childOf(v, "model")
		if !isSet(nested) {
			nested = childOf(v, "default")
		}
		nestedProvider := strings.TrimSpace(orText(childOf(v, "provider")))
		putKey(model, k, stringNode(strings.TrimSpace(orText(nested))))
		if nestedProvider != "" {
			if outer := strings.TrimSpace(orText(childOf(model, "provider"))); outer == "" || outer == "auto" {
				putKey(model, "provider", stringNode(nestedProvider))
			}
		}
	}
	for _, k := range []string{"provider", "base_url", "context_length"} {
		if root := childOf(out, k); isSet(root) && !isSet(childOf(model, k)) {
			putKey(model, k, plainCopy(root))
		}
		dropKey(out, k)
	}
	if p := childOf(model, "provider"); p != nil && !isNull(p) {
		putKey(model, "provider", stringNode(strings.TrimSpace(displayText(p))))
	}
	for _, alias := range []*yaml.Node{childOf(out, "api_base"), childOf(model, "api_base")} {
		if isSet(alias) && !isSet(childOf(model, "base_url")) {
			putKey(model, "base_url", plainCopy(alias))
		}
	}
	dropKey(out, "api_base")
	dropKey(model, "api_base")
	alias := childOf(model, "model")
	if !isSet(alias) {
		alias = childOf(model, "name")
	}
	if !isSet(childOf(model, "default")) && isSet(alias) {
		putKey(model, "default", plainCopy(alias))
	}
	if isSet(childOf(model, "default")) {
		dropKey(model, "model")
		dropKey(model, "name")
	}
	return out
}

// Null node standing in for an absent value
func orNull(n *yaml.Node) *yaml.Node {
	if n == nil {
		return nullNode()
	}
	return n
}

// Reports a string scalar
func isStringNode(n *yaml.Node) bool {
	n = resolveAlias(n)
	if n == nil || n.Kind != yaml.ScalarNode {
		return false
	}
	tag, _ := scalarKind(n)
	return tag == tagStr
}

// Clears inline endpoint credentials of an earlier route, the key with its pointers, the wire mode and endpoint as asked
func (m *ModelConfig) ClearEndpointCredentials(apiKey, apiMode, baseURL bool) {
	if apiKey {
		m.APIKey, m.API, m.KeyEnv, m.APIKeyEnv = "", "", "", ""
	}
	if apiMode {
		m.APIMode = ""
	}
	if baseURL {
		m.BaseURL = ""
	}
}
