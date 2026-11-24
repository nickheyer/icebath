package mcp

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"strings"
	"sync"

	"github.com/nickheyer/icebath/pkg/internal/fnmatch"
	"github.com/nickheyer/icebath/pkg/tools"
	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
)

// Prompt injection indicators in tool descriptions, logged and never blocking
var injectionPatterns = []struct {
	re     *regexp.Regexp
	reason string
}{
	{regexp.MustCompile(`(?i)ignore\s+(all\s+)?previous\s+instructions`), "prompt override attempt ('ignore previous instructions')"},
	{regexp.MustCompile(`(?i)you\s+are\s+now\s+a`), "identity override attempt ('you are now a...')"},
	{regexp.MustCompile(`(?i)your\s+new\s+(task|role|instructions?)\s+(is|are)`), "task override attempt"},
	{regexp.MustCompile(`(?i)system\s*:\s*`), "system prompt injection attempt"},
	{regexp.MustCompile(`(?i)<\s*(system|human|assistant)\s*>`), "role tag injection attempt"},
	{regexp.MustCompile(`(?i)do\s+not\s+(tell|inform|mention|reveal)`), "concealment instruction"},
	{regexp.MustCompile(`(?i)(curl|wget|fetch)\s+https?://`), "network command in description"},
	{regexp.MustCompile(`(?i)base64\.(b64decode|decodebytes)`), "base64 decode reference"},
	{regexp.MustCompile(`(?i)exec\s*\(|eval\s*\(`), "code execution reference"},
	{regexp.MustCompile(`(?i)import\s+(subprocess|os|shutil|socket)`), "dangerous import reference"},
}

// Injection findings in a tool description, warning with the first 200 characters when any match
func scanDescription(serverName, toolName, description string) []string {
	if description == "" {
		return nil
	}
	var findings []string
	for _, p := range injectionPatterns {
		if p.re.MatchString(description) {
			findings = append(findings, p.reason)
		}
	}
	if len(findings) > 0 {
		shown := []rune(description)
		if len(shown) > 200 {
			shown = shown[:200]
		}
		pkgLog().Warn(fmt.Sprintf("MCP server '%s' tool '%s': suspicious description content \u2014 %s. Description: %s",
			serverName, toolName, strings.Join(findings, "; "), string(shown)))
	}
	return findings
}

// Fresh empty object schema
func emptyObjectSchema() *jsonutil.Object {
	return jsonutil.ObjectOf("type", "object", "properties", jsonutil.NewObject())
}

// Ordered view of a dict value, a plain map entering in sorted key order
func asOrdered(v any) (*jsonutil.Object, bool) {
	return jsonutil.AsObject(v)
}

// Legacy definitions promoted to $defs where it is a keyword, never a property name
func rewriteLocalRefs(node any) any {
	switch n := node.(type) {
	case []any:
		out := make([]any, len(n))
		for i, item := range n {
			out[i] = rewriteLocalRefs(item)
		}
		return out
	case *jsonutil.Object, map[string]any:
		src, _ := asOrdered(n)
		normalized := jsonutil.NewObject()
		for _, key := range src.Keys() {
			value := src.Value(key)
			if m, ok := asOrdered(value); ok && (key == "properties" || key == "patternProperties") {
				props := jsonutil.NewObject()
				for _, name := range m.Keys() {
					props.Set(name, rewriteLocalRefs(m.Value(name)))
				}
				normalized.Set(key, props)
				continue
			}
			k := key
			if key == "definitions" {
				k = "$defs"
			}
			normalized.Set(k, rewriteLocalRefs(value))
		}
		if ref, ok := normalized.Value("$ref").(string); ok && strings.HasPrefix(ref, "#/definitions/") {
			normalized.Set("$ref", "#/$defs/"+ref[len("#/definitions/"):])
		}
		return normalized
	}
	return node
}

// Keywords whose value maps entry names to schemas and is never a schema node itself
var schemaMapKeys = map[string]bool{"properties": true, "patternProperties": true, "$defs": true, "definitions": true, "dependentSchemas": true}

// Fills a missing object type, ensures properties and prunes required, leaving constraint fragments untouched
func repairObjectShape(node any, objectRoot bool) any {
	switch n := node.(type) {
	case []any:
		out := make([]any, len(n))
		for i, item := range n {
			out[i] = repairObjectShape(item, false)
		}
		return out
	case *jsonutil.Object, map[string]any:
		src, _ := asOrdered(n)
		repaired := jsonutil.NewObject()
		for _, key := range src.Keys() {
			value := src.Value(key)
			if m, ok := asOrdered(value); ok && schemaMapKeys[key] {
				entries := jsonutil.NewObject()
				for _, name := range m.Keys() {
					entries.Set(name, repairObjectShape(m.Value(name), false))
				}
				repaired.Set(key, entries)
				continue
			}
			repaired.Set(key, repairObjectShape(value, false))
		}
		_, hasProps := repaired.Get("properties")
		_, hasType := src.Get("type")
		if objectRoot || hasProps || hasType {
			_, hasRequired := repaired.Get("required")
			if !pyTruthy(repaired.Value("type")) && (hasProps || hasRequired) {
				repaired.Set("type", "object")
			}
			if repaired.Value("type") == "object" {
				if _, ok := jsonutil.AsMap(repaired.Value("properties")); !ok {
					repaired.Set("properties", jsonutil.NewObject())
				}
				props, _ := jsonutil.AsMap(repaired.Value("properties"))
				kept := []any{}
				if required, ok := repaired.Value("required").([]any); ok {
					for _, r := range required {
						if s, isStr := r.(string); isStr {
							if _, in := props[s]; in {
								kept = append(kept, s)
							}
						}
					}
				}
				repaired.Set("required", kept)
			}
		}
		return repaired
	}
	return node
}

// Input schema normalized so one form is valid on every provider
func normalizeInputSchema(schema *jsonutil.Object) *jsonutil.Object {
	if schema == nil || schema.Len() == 0 {
		return emptyObjectSchema()
	}
	var normalized any = rewriteLocalRefs(schema)
	normalized = tools.StripNullableUnions(normalized, true)
	normalized = tools.CollapseConstUnions(normalized)
	normalized = repairObjectShape(normalized, true)
	m, ok := normalized.(*jsonutil.Object)
	if !ok {
		return emptyObjectSchema()
	}
	if m.Value("type") == "object" {
		if _, has := m.Get("properties"); !has {
			m = m.Clone()
			m.Set("properties", jsonutil.NewObject())
		}
	}
	return m
}

var nameComponentRe = regexp.MustCompile(`[^A-Za-z0-9_]`)

// Name component with every character outside letters digits and underscore replaced by underscore
func SanitizeNameComponent(value string) string {
	return nameComponentRe.ReplaceAllString(value, "_")
}

// ToolNamePrefix starts every native MCP tool name
const ToolNamePrefix = "mcp__"

// Provider limit on function names and the hash suffix length used to clamp longer ones
const (
	toolNameMaxLength  = 64
	toolNameHashLength = 8
)

var (
	clampedWarnedMu sync.Mutex
	clampedWarned   = map[string]bool{}
)

// Registry and wire name mcp__server__tool clamped to 64 characters with a stable hash suffix
func PrefixedToolName(serverName, toolName string) string {
	full := ToolNamePrefix + SanitizeNameComponent(serverName) + "__" + SanitizeNameComponent(toolName)
	if len(full) <= toolNameMaxLength {
		return full
	}
	sum := sha256.Sum256([]byte(full))
	suffix := "_" + hex.EncodeToString(sum[:])[:toolNameHashLength]
	clampedWarnedMu.Lock()
	if !clampedWarned[full] {
		clampedWarned[full] = true
		pkgLog().Warn(fmt.Sprintf("MCP tool name %s (%d chars) exceeds the %d-char provider limit; shortened to a deterministic hash-suffixed name",
			pyReprStr(full), len(full), toolNameMaxLength))
	}
	clampedWarnedMu.Unlock()
	return full[:toolNameMaxLength-len(suffix)] + suffix
}

// Registry schema of one MCP tool with its prefixed name, tag stripped description and normalized parameters
func convertToolSchema(serverName string, t *Tool) *jsonutil.Object {
	desc := t.Description
	if desc == "" {
		desc = fmt.Sprintf("MCP tool %s from %s", t.Name, serverName)
	}
	return jsonutil.ObjectOf(
		"name", PrefixedToolName(serverName, t.Name),
		"description", tools.StripUnicodeTags(desc),
		"parameters", normalizeInputSchema(t.InputSchema),
	)
}

// One generated utility tool, its handler key, description template, properties and required names
type utilitySpec struct {
	key         string
	description string
	properties  func() *jsonutil.Object
	required    []string
}

// Free form object argument with a description
func freeObject(description string) *jsonutil.Object {
	return jsonutil.ObjectOf("type", "object", "description", description, "properties", jsonutil.NewObject(), "additionalProperties", true)
}

// String argument with a description
func stringArg(description string) *jsonutil.Object {
	return jsonutil.ObjectOf("type", "string", "description", description)
}

var utilityToolSpecs = []utilitySpec{
	{"list_resources", "List available resources from MCP server '%s'", jsonutil.NewObject, nil},
	{"read_resource", "Read a resource by URI from MCP server '%s'", func() *jsonutil.Object {
		return jsonutil.ObjectOf("uri", stringArg("URI of the resource to read"))
	}, []string{"uri"}},
	{"list_prompts", "List available prompts from MCP server '%s'", jsonutil.NewObject, nil},
	{"get_prompt", "Get a prompt by name from MCP server '%s'", func() *jsonutil.Object {
		return jsonutil.ObjectOf(
			"name", stringArg("Name of the prompt to retrieve"),
			"arguments", freeObject("Optional arguments to pass to the prompt"),
		)
	}, []string{"name"}},
	{"list_event_types", "List the event types MCP server '%s' can notify about, and your active subscriptions to them",
		jsonutil.NewObject, nil},
	{"subscribe_event", "Subscribe to an event from MCP server '%s': each occurrence starts a new agent run with your instructions and delivers its result to the chosen target",
		func() *jsonutil.Object {
			return jsonutil.ObjectOf(
				"name", stringArg("Event type name from list_event_types"),
				"arguments", freeObject("Subscription arguments matching the event's inputSchema"),
				"instructions", stringArg("What to do each time the event arrives (up to 2000 characters)"),
				"deliver", stringArg("Where each run's result goes: log, telegram, discord, slack, ..."),
				"deliver_chat_id", stringArg("Optional chat id on the deliver platform"),
			)
		}, []string{"name"}},
	{"unsubscribe_event", "Cancel a subscription to an event from MCP server '%s'", func() *jsonutil.Object {
		return jsonutil.ObjectOf(
			"name", stringArg("Event type name"),
			"arguments", freeObject("The same arguments the subscription was made with"),
		)
	}, []string{"name"}},
}

// UtilitySchema is one generated resource, prompt or event tool and the handler it dispatches to
type UtilitySchema struct {
	Schema     *jsonutil.Object
	HandlerKey string
}

// Schemas of the resource, prompt and event utility tools of one server in spec order
func buildUtilitySchemas(serverName string) []UtilitySchema {
	out := make([]UtilitySchema, 0, len(utilityToolSpecs))
	for _, spec := range utilityToolSpecs {
		params := jsonutil.ObjectOf("type", "object", "properties", spec.properties())
		if spec.required != nil {
			req := make([]any, len(spec.required))
			for i, r := range spec.required {
				req[i] = r
			}
			params.Set("required", req)
		}
		out = append(out, UtilitySchema{
			Schema: jsonutil.ObjectOf(
				"name", PrefixedToolName(serverName, spec.key),
				"description", fmt.Sprintf(spec.description, serverName),
				"parameters", params,
			),
			HandlerKey: spec.key,
		})
	}
	return out
}

// Include or exclude config as a set of exact names or globs, warning and ignoring other shapes
func normalizeNameFilter(value any, label string) map[string]bool {
	set := map[string]bool{}
	switch v := value.(type) {
	case nil:
	case string:
		set[v] = true
	case []any:
		for _, item := range v {
			set[pyStr(item)] = true
		}
	case []string:
		for _, item := range v {
			set[item] = true
		}
	default:
		pkgLog().Warn(fmt.Sprintf("MCP config %s must be a string or list of strings; ignoring %s", label, pyRepr(value)))
	}
	return set
}

// Reports whether a tool name matches an exact entry or a case sensitive glob entry
func MatchesNameFilter(toolName string, patterns map[string]bool) bool {
	if len(patterns) == 0 {
		return false
	}
	if patterns[toolName] {
		return true
	}
	for p := range patterns {
		if strings.ContainsAny(p, "*?[") && fnmatch.FnmatchCase(toolName, p) {
			return true
		}
	}
	return false
}

// Capability family a utility handler needs the server to advertise before it registers
var utilityCapabilityAttrs = map[string]string{
	"list_resources": "resources", "read_resource": "resources",
	"list_prompts": "prompts", "get_prompt": "prompts",
	"list_event_types": "events", "subscribe_event": "events", "unsubscribe_event": "events",
}
