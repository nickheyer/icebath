package tool

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/nickheyer/icebath/internal/model/wire"
)

var (
	unionKeys     = []string{"anyOf", "oneOf"}
	unionMetaKeys = []string{"title", "description", "default", "examples"}
)

func propKeyByte(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '.' || c == '-'
}

func propKeyConforms(key string) bool {
	body := key
	if strings.HasSuffix(body, "\n") {
		trimmed := strings.TrimSuffix(body, "\n")
		if conformingBody(trimmed) {
			return true
		}
	}
	return conformingBody(body)
}

func conformingBody(s string) bool {
	if s == "" || len(s) > 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		if !propKeyByte(s[i]) {
			return false
		}
	}
	return true
}

func sanitizePropertyKey(key string) string {
	var b strings.Builder
	n := 0
	for _, r := range key {
		if n == 64 {
			break
		}
		if r < utf8.RuneSelf && propKeyByte(byte(r)) {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
		n++
	}
	if b.Len() == 0 {
		return "param"
	}
	return b.String()
}

func renamePropertyKeys(props *jnode, path string) map[string]string {
	renames := map[string]string{}
	taken := map[string]bool{}
	for _, k := range props.keys {
		if propKeyConforms(k) {
			taken[k] = true
		}
	}
	for _, key := range props.keys {
		if propKeyConforms(key) {
			continue
		}
		base := sanitizePropertyKey(key)
		candidate, i := base, 2
		for taken[candidate] {
			suffix := "_" + strconv.Itoa(i)
			cut := 64 - len(suffix)
			if cut > len(base) {
				cut = len(base)
			}
			if cut < 0 {
				cut = 0
			}
			candidate = base[:cut] + suffix
			i++
		}
		taken[candidate] = true
		renames[key] = candidate
		slog.Debug(fmt.Sprintf("schema_sanitizer[%s]: renamed property key %q -> %q (provider key-pattern compat)", path, key, candidate))
	}
	return renames
}

func percentDecode(s string) string {
	if !strings.Contains(s, "%") {
		return s
	}
	var out []byte
	for i := 0; i < len(s); i++ {
		if s[i] == '%' && i+2 < len(s) {
			if hi, ok := unhex(s[i+1]); ok {
				if lo, ok := unhex(s[i+2]); ok {
					out = append(out, hi<<4|lo)
					i += 2
					continue
				}
			}
		}
		out = append(out, s[i])
	}
	return strings.ToValidUTF8(string(out), "�")
}

func unhex(c byte) (byte, bool) {
	switch {
	case c >= '0' && c <= '9':
		return c - '0', true
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10, true
	case c >= 'A' && c <= 'F':
		return c - 'A' + 10, true
	}
	return 0, false
}

func pointerIndex(token string) (int, bool) {
	if token == "0" {
		return 0, true
	}
	if token == "" || token[0] < '1' || token[0] > '9' {
		return 0, false
	}
	for i := 1; i < len(token); i++ {
		if token[i] < '0' || token[i] > '9' {
			return 0, false
		}
	}
	n, err := strconv.Atoi(token)
	return n, err == nil
}

func badPointerEscape(token string) bool {
	for i := 0; i < len(token); i++ {
		if token[i] == '~' && (i+1 >= len(token) || token[i+1] != '0' && token[i+1] != '1') {
			return true
		}
	}
	return false
}

func resolveLocalRef(schema, root *jnode, seen map[string]bool) *jnode {
	if !schema.isObj() || !root.isObj() {
		return schema
	}
	ref, ok := schema.get("$ref").text()
	if !ok || !strings.HasPrefix(ref, "#") {
		return schema
	}
	pointer := percentDecode(ref[1:])
	if pointer != "" && !strings.HasPrefix(pointer, "/") {
		return schema
	}
	if seen[ref] {
		return schema
	}
	target := root
	if pointer != "" {
		for _, raw := range strings.Split(pointer, "/")[1:] {
			if badPointerEscape(raw) {
				return schema
			}
			token := strings.ReplaceAll(strings.ReplaceAll(raw, "~1", "/"), "~0", "~")
			switch {
			case target.isObj() && target.has(token):
				target = target.get(token)
			case target.isArr():
				i, ok := pointerIndex(token)
				if !ok || i >= len(target.arr) {
					return schema
				}
				target = target.arr[i]
			default:
				return schema
			}
		}
	}
	if !target.isObj() {
		return schema
	}
	next := map[string]bool{ref: true}
	for k := range seen {
		next[k] = true
	}
	resolved := resolveLocalRef(target, root, next)
	if len(schema.keys) == 1 || !resolved.isObj() {
		return resolved
	}
	combined := resolved.shallow()
	for i, k := range schema.keys {
		if k == "$ref" {
			continue
		}
		combined.setDefault(k, schema.vals[i])
	}
	return combined
}

func ResolveSchemaRef(schema, root json.RawMessage) json.RawMessage {
	s, err := parseNode(schema)
	if err != nil {
		return schema
	}
	r, err := parseNode(root)
	if err != nil {
		return schema
	}
	return resolveLocalRef(s, r, nil).compact()
}

func unrenameArgs(params *jnode, args map[string]any, root *jnode) map[string]any {
	if root == nil {
		root = params
	}
	params = resolveLocalRef(params, root, nil)
	props := params.get("properties")
	if !params.isObj() || !props.isObj() || args == nil {
		return args
	}
	reverse := map[string]string{}
	for orig, renamed := range renamePropertyKeys(props, "<unrename>") {
		reverse[renamed] = orig
	}
	out := make(map[string]any, len(args))
	for key, value := range args {
		orig := key
		if o, ok := reverse[key]; ok {
			orig = o
		}
		var sub *jnode
		if p := props.get(orig); p.isObj() {
			sub = resolveLocalRef(p, root, nil)
		}
		if m, ok := asObject(value); ok && sub.truthy() {
			value = unrenameArgs(sub, m, root)
		} else if list, ok := value.([]any); ok && sub.isObj() && sub.get("items").isObj() {
			items := make([]any, len(list))
			for i, item := range list {
				if m, ok := asObject(item); ok {
					items[i] = unrenameArgs(sub.get("items"), m, root)
				} else {
					items[i] = item
				}
			}
			value = items
		}
		out[orig] = value
	}
	return out
}

func asObject(v any) (map[string]any, bool) {
	switch t := v.(type) {
	case map[string]any:
		return t, true
	case Args:
		return map[string]any(t), true
	}
	return nil, false
}

func rewriteNode(n *jnode, fn func(*jnode) *jnode) *jnode {
	switch {
	case n.isArr():
		out := newArr()
		for _, item := range n.arr {
			out.arr = append(out.arr, rewriteNode(item, fn))
		}
		return out
	case n.isObj():
		out := newObj()
		for i, k := range n.keys {
			out.set(k, rewriteNode(n.vals[i], fn))
		}
		return fn(out)
	}
	return n
}

func sanitizeParameters(name string, params json.RawMessage) json.RawMessage {
	trimmed := strings.TrimSpace(string(params))
	if trimmed == "" || trimmed == "null" {
		return emptyObjectSchema().compact()
	}
	node, err := parseNode(params)
	if err != nil || !node.isObj() {
		return emptyObjectSchema().compact()
	}
	if name == "" {
		name = "<tool>"
	}
	top := sanitizeNode(node, name)
	if !top.isObj() {
		top = newObj()
	}
	top.set("type", newStr("object"))
	if !top.get("properties").isObj() {
		top.set("properties", newObj())
	}
	if !top.get("required").isArr() {
		top.set("required", newArr())
	}
	top = stripNullableUnions(top, true)
	top = stripTopLevelCombinators(top, name)
	return stripRefSiblings(top).compact()
}

func stripRefSiblings(n *jnode) *jnode {
	return rewriteNode(n, func(out *jnode) *jnode {
		if out.has("$ref") {
			out.del("default")
		}
		return out
	})
}

func stripTopLevelCombinators(params *jnode, path string) *jnode {
	if !params.isObj() {
		return params
	}
	out := params.shallow()
	for _, key := range []string{"allOf", "anyOf", "oneOf", "enum", "not"} {
		if out.del(key) {
			slog.Debug(fmt.Sprintf("schema_sanitizer[%s]: stripped top-level %q combinator from tool parameters (strict-backend compat)", path, key))
		}
	}
	return out
}

func isNullBranch(item *jnode) bool {
	return item.isObj() && item.get("type").strEq("null")
}

func carryUnionMeta(outer, replacement *jnode, skipDefaultOnRef bool) {
	for _, key := range unionMetaKeys {
		if outer.has(key) && !replacement.has(key) && !(skipDefaultOnRef && key == "default" && replacement.has("$ref")) {
			replacement.set(key, outer.get(key))
		}
	}
}

func stripNullableUnions(schema *jnode, keepHint bool) *jnode {
	var collapse func(*jnode) *jnode
	collapse = func(stripped *jnode) *jnode {
		for _, key := range unionKeys {
			variants := stripped.get(key)
			if !variants.isArr() {
				continue
			}
			var nonNull []*jnode
			for _, item := range variants.arr {
				if !isNullBranch(item) {
					nonNull = append(nonNull, item)
				}
			}
			if len(nonNull) == 1 && len(nonNull) != len(variants.arr) {
				replacement := newObj()
				if nonNull[0].isObj() {
					replacement = nonNull[0].shallow()
				}
				if keepHint {
					replacement.setDefault("nullable", newBool(true))
				}
				carryUnionMeta(stripped, replacement, true)
				return rewriteNode(replacement, collapse)
			}
		}
		return stripped
	}
	return rewriteNode(schema, collapse)
}

func StripNullableUnions(schema json.RawMessage, keepHint bool) json.RawMessage {
	n, err := parseNode(schema)
	if err != nil {
		return schema
	}
	return stripNullableUnions(n, keepHint).compact()
}

func constBranchType(branch *jnode) string {
	if !branch.isObj() || !branch.has("const") {
		return ""
	}
	for _, k := range branch.keys {
		switch k {
		case "const", "type", "title", "description":
		default:
			return ""
		}
	}
	c := branch.get("const")
	var kind string
	switch c.kind {
	case jBool:
		kind = "boolean"
	case jStr:
		kind = "string"
	case jNum:
		if strings.ContainsAny(c.num, ".eE") {
			kind = "number"
		} else {
			kind = "integer"
		}
	default:
		return ""
	}
	if t := branch.get("type"); t == nil || t.strEq(kind) {
		return kind
	}
	return ""
}

func collapseConstUnions(schema *jnode) *jnode {
	return rewriteNode(schema, func(out *jnode) *jnode {
		for _, key := range unionKeys {
			variants := out.get(key)
			if !variants.isArr() || len(variants.arr) == 0 {
				continue
			}
			var nulls []*jnode
			for _, item := range variants.arr {
				if isNullBranch(item) && !item.has("const") {
					nulls = append(nulls, item)
				}
			}
			var consts []*jnode
			for _, item := range variants.arr {
				in := false
				for _, nb := range nulls {
					if nodesEqual(item, nb) {
						in = true
						break
					}
				}
				if !in {
					consts = append(consts, item)
				}
			}
			if len(nulls) > 1 || len(consts) == 0 {
				continue
			}
			kind := constBranchType(consts[0])
			uniform := kind != ""
			for _, item := range consts[1:] {
				if constBranchType(item) != kind {
					uniform = false
					break
				}
			}
			if !uniform {
				continue
			}
			enum := newArr()
			for _, item := range consts {
				enum.arr = append(enum.arr, item.get("const"))
			}
			replacement := newObj().with("type", newStr(kind)).with("enum", enum)
			if len(nulls) > 0 {
				replacement.set("nullable", newBool(true))
			}
			carryUnionMeta(out, replacement, false)
			return replacement
		}
		return out
	})
}

func CollapseConstUnions(schema json.RawMessage) json.RawMessage {
	n, err := parseNode(schema)
	if err != nil {
		return schema
	}
	return collapseConstUnions(n).compact()
}

var bareTypeNames = map[string]bool{"object": true, "string": true, "number": true, "integer": true, "boolean": true, "array": true, "null": true}

var nonSchemaListKeys = map[string]bool{"required": true, "enum": true, "examples": true, "dependentRequired": true}

var schemaMapKeys = map[string]bool{"properties": true, "$defs": true, "definitions": true, "patternProperties": true, "dependentSchemas": true}

var schemaChildKeys = map[string]bool{
	"items": true, "additionalItems": true, "additionalProperties": true, "unevaluatedItems": true, "unevaluatedProperties": true,
	"contains": true, "propertyNames": true, "not": true, "if": true, "then": true, "else": true, "anyOf": true, "oneOf": true,
	"allOf": true, "prefixItems": true,
}

func normalizeTypeArray(value *jnode, out *jnode) {
	hasNull := false
	var nonNull []string
	for _, t := range value.arr {
		if t.strEq("null") {
			hasNull = true
			continue
		}
		if s, ok := t.text(); ok {
			nonNull = append(nonNull, s)
		}
	}
	if len(nonNull) == 0 {
		if hasNull {
			out.set("type", newStr("null"))
		} else {
			out.set("type", newStr("object"))
		}
		return
	}
	if len(nonNull) == 1 {
		out.set("type", newStr(nonNull[0]))
	} else {
		branches := newArr()
		for _, t := range nonNull {
			branches.arr = append(branches.arr, newObj().with("type", newStr(t)))
		}
		out.set("anyOf", branches)
	}
	if hasNull {
		out.setDefault("nullable", newBool(true))
	}
}

func sanitizeNode(node *jnode, path string) *jnode {
	if s, ok := node.text(); ok {
		if bareTypeNames[s] {
			slog.Debug(fmt.Sprintf("schema_sanitizer[%s]: replacing bare-string schema %q with {'type': %q}", path, s, s))
			if s == "object" {
				return emptyObjectSchema()
			}
			return newObj().with("type", newStr(s))
		}
		slog.Debug(fmt.Sprintf("schema_sanitizer[%s]: replacing non-schema string %q with empty object schema", path, s))
		return emptyObjectSchema()
	}
	if node.isArr() {
		out := newArr()
		for i, item := range node.arr {
			out.arr = append(out.arr, sanitizeNode(item, fmt.Sprintf("%s[%d]", path, i)))
		}
		return out
	}
	if !node.isObj() {
		return node
	}
	propsIn := node.get("properties")
	renames := map[string]string{}
	if propsIn.isObj() {
		renames = renamePropertyKeys(propsIn, path+".properties")
	}
	out := newObj()
	for i, key := range node.keys {
		value := node.vals[i]
		switch {
		case key == "type" && value.isArr():
			normalizeTypeArray(value, out)
		case schemaMapKeys[key] && value.isObj():
			m := newObj()
			for j, k := range value.keys {
				renamed := k
				if key == "properties" {
					if r, ok := renames[k]; ok {
						renamed = r
					}
				}
				m.set(renamed, sanitizeNode(value.vals[j], path+"."+key+"."+renamed))
			}
			out.set(key, m)
		case key == "dependencies" && value.isObj():
			m := newObj()
			for j, k := range value.keys {
				if value.vals[j].isObj() {
					m.set(k, sanitizeNode(value.vals[j], path+"."+key+"."+k))
				} else {
					m.set(k, value.vals[j].clone())
				}
			}
			out.set(key, m)
		case key == "items" || key == "additionalProperties":
			if value.kind == jBool {
				out.set(key, value)
			} else {
				out.set(key, sanitizeNode(value, path+"."+key))
			}
		case nonSchemaListKeys[key]:
			if key == "required" && value.kind == jBool {
				continue
			}
			if key == "required" && len(renames) > 0 && value.isArr() {
				list := newArr()
				for _, r := range value.arr {
					if s, ok := r.text(); ok {
						if rn, ok := renames[s]; ok {
							list.arr = append(list.arr, newStr(rn))
							continue
						}
					}
					list.arr = append(list.arr, r)
				}
				out.set(key, list)
			} else {
				out.set(key, value.clone())
			}
		case schemaChildKeys[key]:
			out.set(key, sanitizeNode(value, path+"."+key))
		default:
			out.set(key, value.clone())
		}
	}
	if propsIn.isObj() {
		var lifted []string
		for j, k := range propsIn.keys {
			v := propsIn.vals[j]
			if v.isObj() && v.get("required") != nil && v.get("required").kind == jBool && v.get("required").b {
				if r, ok := renames[k]; ok {
					k = r
				}
				lifted = append(lifted, k)
			}
		}
		if len(lifted) > 0 {
			required := out.get("required")
			list := newArr()
			if required.isArr() {
				list.arr = append(list.arr, required.arr...)
			}
			for _, k := range lifted {
				present := false
				for _, r := range list.arr {
					if r.strEq(k) {
						present = true
						break
					}
				}
				if !present {
					list.arr = append(list.arr, newStr(k))
				}
			}
			out.set("required", list)
		}
	}
	if out.get("type").strEq("object") {
		if !out.get("properties").isObj() {
			out.set("properties", newObj())
		}
		props := out.get("properties")
		required := out.get("required")
		kept := newArr()
		if required.isArr() {
			for _, r := range required.arr {
				if s, ok := r.text(); ok && props.has(s) {
					kept.arr = append(kept.arr, r)
				}
			}
		}
		out.set("required", kept)
	}
	return out
}

var schemaMarkers = []string{"type", "anyOf", "oneOf", "allOf"}

func eachObject(n *jnode, fn func(*jnode)) {
	if n.isObj() {
		fn(n)
		for _, v := range n.vals {
			eachObject(v, fn)
		}
		return
	}
	if n.isArr() {
		for _, v := range n.arr {
			eachObject(v, fn)
		}
	}
}

func reactiveStrip(tools []wire.Tool, strip func(*jnode) int, logMsg string) int {
	stripped := 0
	for i := range tools {
		params, err := parseNode(tools[i].Parameters)
		if err != nil || !params.isObj() {
			continue
		}
		n := 0
		eachObject(params, func(node *jnode) { n += strip(node) })
		if n > 0 {
			tools[i].Parameters = params.compact()
			stripped += n
		}
	}
	if stripped > 0 {
		slog.Info(fmt.Sprintf(logMsg, stripped))
	}
	return stripped
}

func StripPatternAndFormat(tools []wire.Tool) int {
	return reactiveStrip(tools, func(node *jnode) int {
		schema := false
		for _, k := range schemaMarkers {
			if node.has(k) {
				schema = true
				break
			}
		}
		if !schema {
			return 0
		}
		hits := 0
		for _, k := range []string{"pattern", "format"} {
			if node.del(k) {
				hits++
			}
		}
		return hits
	}, "schema_sanitizer: stripped %d pattern/format keyword(s) from tool schemas (llama.cpp grammar-parse recovery)")
}

func StripSlashEnum(tools []wire.Tool) int {
	return reactiveStrip(tools, func(node *jnode) int {
		enum := node.get("enum")
		if !enum.isArr() {
			return 0
		}
		for _, v := range enum.arr {
			if s, ok := v.text(); ok && strings.Contains(s, "/") {
				node.del("enum")
				return 1
			}
		}
		return 0
	}, "schema_sanitizer: stripped %d enum keyword(s) containing '/' from tool schemas (xAI Responses grammar-compile recovery)")
}
