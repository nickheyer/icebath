package state

import (
	"errors"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"reflect"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// Keys the message sanitizer handles explicitly, every other key is swept generically
var MessageCoreKeys = map[string]bool{"content": true, "name": true, "tool_calls": true, "role": true, DBRowSnapshot: true}

// True when every byte is ASCII
func isASCII(text string) bool {
	for i := range len(text) {
		if text[i] >= 0x80 {
			return false
		}
	}
	return true
}

// Replaces each lone surrogate code point with U+FFFD, returning ASCII text untouched
func SanitizeSurrogates(text string) string {
	if isASCII(text) {
		return text
	}
	var b strings.Builder
	b.Grow(len(text))
	for i := 0; i < len(text); {
		r, n := pystr.DecodeRune(text[i:])
		if pystr.IsSurrogate(r) {
			b.WriteRune(0xfffd)
		} else {
			b.WriteString(text[i : i+n])
		}
		i += n
	}
	return b.String()
}

// Drops every non ASCII character as the ascii codec with errors ignore does
func StripNonASCII(text string) string {
	if isASCII(text) {
		return text
	}
	out := make([]byte, 0, len(text))
	for i := range len(text) {
		if text[i] < 0x80 {
			out = append(out, text[i])
		}
	}
	return string(out)
}

// Applies fix to a string held at a dict key or list index and reports whether it changed
func fixStrField(container any, key any, fix func(string) string) bool {
	switch c := container.(type) {
	case map[string]any:
		k, _ := key.(string)
		value, ok := c[k].(string)
		if !ok {
			return false
		}
		if fixed := fix(value); fixed != value {
			c[k] = fixed
			return true
		}
	case *pyjson.Object:
		k, _ := key.(string)
		value, ok := c.Vals[k].(string)
		if !ok {
			return false
		}
		if fixed := fix(value); fixed != value {
			c.Set(k, fixed)
			return true
		}
	case []any:
		i, _ := key.(int)
		value, ok := c[i].(string)
		if !ok {
			return false
		}
		if fixed := fix(value); fixed != value {
			c[i] = fixed
			return true
		}
	}
	return false
}

// Applies fix in place to every string nested inside dict and list payloads
func SanitizeStructure(payload any, fix func(string) string) bool {
	found := false
	stack := []any{payload}
	for len(stack) > 0 {
		node := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		switch n := node.(type) {
		case map[string]any:
			for key, value := range n {
				switch value.(type) {
				case string:
					found = fixStrField(n, key, fix) || found
				case map[string]any, *pyjson.Object, []any:
					stack = append(stack, value)
				}
			}
		case *pyjson.Object:
			for _, key := range n.Keys {
				switch value := n.Vals[key]; value.(type) {
				case string:
					found = fixStrField(n, key, fix) || found
				case map[string]any, *pyjson.Object, []any:
					stack = append(stack, value)
				}
			}
		case []any:
			for i, value := range n {
				switch value.(type) {
				case string:
					found = fixStrField(n, i, fix) || found
				case map[string]any, *pyjson.Object, []any:
					stack = append(stack, value)
				}
			}
		}
	}
	return found
}

type strField struct {
	container any
	key       any
}

// Applies fix to the string fields of every message dict, deep adding tool call ids, names and nested extras
func SanitizeMessages(messages []any, fix func(string) string, deep bool) bool {
	found := false
	for _, raw := range messages {
		msg, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		msgFound := false
		var fields []strField
		if parts, isList := msg["content"].([]any); isList {
			fields = []strField{}
			for _, p := range parts {
				if pd, isDict := p.(map[string]any); isDict {
					fields = append(fields, strField{pd, "text"})
				}
			}
		} else {
			fields = []strField{{msg, "content"}}
		}
		fields = append(fields, strField{msg, "name"})
		if toolCalls, isList := msg["tool_calls"].([]any); isList {
			for _, tc := range toolCalls {
				tcd, isDict := tc, isMapping(tc)
				var fn any
				if isDict {
					if f, _ := mappingGet(tcd, "function"); isMapping(f) {
						fn = f
					}
				}
				if deep && isDict {
					fields = append(fields, strField{tcd, "id"})
				}
				if fn != nil {
					if deep {
						fields = append(fields, strField{fn, "name"})
					}
					fields = append(fields, strField{fn, "arguments"})
				}
			}
		}
		for _, f := range fields {
			msgFound = fixStrField(f.container, f.key, fix) || msgFound
		}
		for key, value := range msg {
			if MessageCoreKeys[key] {
				continue
			}
			switch value.(type) {
			case string:
				msgFound = fixStrField(msg, key, fix) || msgFound
			case map[string]any, *pyjson.Object, []any:
				if deep {
					msgFound = SanitizeStructure(value, fix) || msgFound
				}
			}
		}
		if msgFound {
			delete(msg, DBPersistedMarker)
			found = true
		}
	}
	return found
}

// Replaces lone surrogates in every string of a nested payload in place
func SanitizeStructureSurrogates(payload any) bool {
	return SanitizeStructure(payload, SanitizeSurrogates)
}

// Deep surrogate repair of every message dict in place
func SanitizeMessagesSurrogates(messages []any) bool {
	return SanitizeMessages(messages, SanitizeSurrogates, true)
}

// Strips non ASCII characters from every string of a nested payload in place
func SanitizeStructureNonASCII(payload any) bool { return SanitizeStructure(payload, StripNonASCII) }

// Shallow non ASCII strip of every message dict in place
func SanitizeMessagesNonASCII(messages []any) bool {
	return SanitizeMessages(messages, StripNonASCII, false)
}

// Strips non ASCII characters from tool schemas in place
var SanitizeToolsNonASCII = SanitizeStructureNonASCII

// ErrReadOnlyToolCallField is the setattr failure for an entry that cannot hold the key
var ErrReadOnlyToolCallField = errors.New("tool call entry cannot hold the attribute")

// Writes a key on a tool call dict or the matching field or Extra entry of a tool call struct
func ToolCallSet(tc any, key string, value any) error {
	switch m := tc.(type) {
	case map[string]any:
		m[key] = value
		return nil
	case *pyjson.Object:
		m.Set(key, value)
		return nil
	case *pyObject:
		for i := range *m {
			if (*m)[i].Key == key {
				(*m)[i].Value = value
				return nil
			}
		}
		*m = append(*m, pyKeyValue{key, value})
		return nil
	}
	rv := reflect.ValueOf(tc)
	if rv.Kind() != reflect.Pointer || rv.IsNil() || rv.Elem().Kind() != reflect.Struct {
		return ErrReadOnlyToolCallField
	}
	rv = rv.Elem()
	if f, ok := structField(rv, key); ok {
		val := reflect.ValueOf(value)
		if !val.IsValid() {
			f.Set(reflect.Zero(f.Type()))
			return nil
		}
		if !val.Type().AssignableTo(f.Type()) {
			return ErrReadOnlyToolCallField
		}
		f.Set(val)
		return nil
	}
	extra := rv.FieldByName("Extra")
	if !extra.IsValid() || extra.Type() != reflect.TypeOf(map[string]any(nil)) {
		return ErrReadOnlyToolCallField
	}
	if extra.IsNil() {
		extra.Set(reflect.ValueOf(map[string]any{}))
	}
	extra.SetMapIndex(reflect.ValueOf(key), reflect.ValueOf(&value).Elem())
	return nil
}
