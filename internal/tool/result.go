package tool

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"unicode/utf8"

	"github.com/nickheyer/icebath/internal/fileutil"
	"github.com/nickheyer/icebath/internal/model/wire"
)

type Result struct {
	Text      string
	Parts     []wire.Part
	Summary   string
	DataUse   []string
	Effect    Effect
	Recreated bool
}

type Effect string

const (
	EffectUnset   Effect = ""
	EffectNone    Effect = "none"
	EffectUnknown Effect = "unknown"
)

type Field struct {
	Key   string
	Value any
}

const (
	maxErrorChars       = 2048
	maxLoggedErrorChars = 8192
	errorTruncated      = "… [truncated]"
)

func Failure(msg string, extra ...Field) Result {
	text, err := failureText(msg, extra)
	if err != nil {
		text, _ = failureText("[TOOL_ERROR] Tool execution failed: "+err.Error(), nil)
	}
	return Result{Text: text}
}

func failureText(msg string, extra []Field) (string, error) {
	members := []wire.Member{{Key: "error", Value: wire.Quote(boundError(strings.ToValidUTF8(msg, "�")))}}
	for _, f := range extra {
		v, err := fileutil.EncodeJSON(f.Value, fileutil.JSONStyle{Tight: true})
		if err != nil {
			return "", fmt.Errorf("failure field %q: %w", f.Key, err)
		}
		members = append(members, wire.Member{Key: f.Key, Value: v})
	}
	text, err := fileutil.EncodeJSON(wire.Object(members), fileutil.JSONStyle{})
	if err != nil {
		return "", err
	}
	return string(text), nil
}

func JSON(v any) Result {
	text, err := fileutil.EncodeJSON(v, fileutil.JSONStyle{})
	if err != nil {
		return Failure("[TOOL_ERROR] Tool execution failed: " + err.Error())
	}
	return Result{Text: string(text)}
}

func (r Result) Plain() string {
	if r.Parts == nil {
		return r.Text
	}
	if r.Summary != "" {
		return r.Summary
	}
	var texts []string
	for _, p := range r.Parts {
		if p.Kind == wire.PartText {
			texts = append(texts, p.Text)
		}
	}
	if len(texts) == 0 {
		return "[multimodal tool result]"
	}
	return strings.Join(texts, "\n")
}

func boundError(text string) string {
	if utf8.RuneCountInString(text) <= maxErrorChars {
		return text
	}
	slog.Debug(fmt.Sprintf("tool error body truncated for context (%d chars): %s", utf8.RuneCountInString(text), runePrefix(text, maxLoggedErrorChars)))
	return runePrefix(text, maxErrorChars) + errorTruncated
}

func boundJSONError(result string) string {
	if utf8.RuneCountInString(result) <= maxErrorChars || !strings.Contains(result, `"error"`) {
		return result
	}
	var payload map[string]json.RawMessage
	if err := json.Unmarshal([]byte(result), &payload); err != nil {
		return result
	}
	raw, ok := payload["error"]
	if !ok {
		return result
	}
	var msg string
	if err := json.Unmarshal(raw, &msg); err != nil || utf8.RuneCountInString(msg) <= maxErrorChars {
		return result
	}
	bounded, err := replaceMember([]byte(result), "error", wire.Quote(boundError(msg)))
	if err != nil {
		return result
	}
	return bounded
}

func replaceMember(object []byte, key string, value json.RawMessage) (string, error) {
	dec := json.NewDecoder(strings.NewReader(string(object)))
	dec.UseNumber()
	if _, err := dec.Token(); err != nil {
		return "", err
	}
	var members []wire.Member
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return "", err
		}
		k, _ := tok.(string)
		var v json.RawMessage
		if err := dec.Decode(&v); err != nil {
			return "", err
		}
		if k == key {
			v = value
		}
		members = append(members, wire.Member{Key: k, Value: v})
	}
	text, err := fileutil.EncodeJSON(wire.Object(members), fileutil.JSONStyle{})
	if err != nil {
		return "", err
	}
	return string(text), nil
}

func runePrefix(s string, n int) string {
	i := 0
	for pos := range s {
		if i == n {
			return s[:pos]
		}
		i++
	}
	return s
}

var (
	ErrUnknownTool = errors.New("unknown tool")
	ErrBadSchema   = errors.New("tool schema parameters must be a JSON object")
	ErrUnavailable = errors.New("tool unavailable in this session")
)

type RegisterError struct {
	Tool, Toolset, Existing, Owner string
	Reason                         RegisterRefusal
	Global                         bool
	Remove                         bool
}

type RegisterRefusal uint8

const (
	Shadow RegisterRefusal = iota + 1
	OverrideDenied
	ForeignScope
	MultiProfileOwner
)

func (e *RegisterError) Error() string {
	switch e.Reason {
	case Shadow:
		if e.Global {
			return fmt.Sprintf("plugin %q attempted to shadow global tool %q without override", e.Owner, e.Tool)
		}
		return fmt.Sprintf("%q (toolset %q) would shadow existing tool from toolset %q; register it with override if the replacement is intentional, or deregister the existing tool first", e.Tool, e.Toolset, e.Existing)
	case OverrideDenied:
		if e.Remove {
			return fmt.Sprintf("plugin module %q cannot deregister tool %q (toolset %q) without operator opt-in (allow_tool_override)", e.Owner, e.Tool, e.Existing)
		}
		return fmt.Sprintf("plugin module %q cannot override built-in tool %q without operator opt-in (allow_tool_override)", e.Owner, e.Tool)
	case ForeignScope:
		if e.Global {
			return fmt.Sprintf("scoped plugin module %q cannot deregister process-global tool %q; register a scoped override instead", e.Owner, e.Tool)
		}
		return fmt.Sprintf("plugin module %q cannot deregister tools outside its own profile scope", e.Owner)
	case MultiProfileOwner:
		return fmt.Sprintf("plugin module %q is active in multiple profiles and cannot register outside one of those scopes", e.Owner)
	}
	return fmt.Sprintf("registration of tool %q refused", e.Tool)
}

type UnfilledError struct {
	Name string
}

func (e *UnfilledError) Error() string {
	return e.Name + " was not supplied to the tool layer"
}
