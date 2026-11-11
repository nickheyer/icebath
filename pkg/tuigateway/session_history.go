package tuigateway

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"math/big"
	"strings"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/skills"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
)

var (
	historyTextKinds  = map[string]bool{"text": true, "input_text": true, "output_text": true}
	historyImageKinds = map[string]bool{"image_url": true, "input_image": true, "image": true}
	historyAudioKinds = map[string]bool{"input_audio": true, "audio": true}
)

// Labels of a call rendered as the payload maps gateway cards consume
func BridgedToolLabels(name string, args map[string]any) []map[string]string {
	labels := tools.ToolLabelsForCall(nil, name, args)
	out := make([]map[string]string, 0, len(labels))
	for _, label := range labels {
		out = append(out, label.AsPayload())
	}
	return out
}

// Whether a path names something on disk, raising the errors pathlib exists does not swallow
func pathExists(p string) bool {
	fi, err := pypath.PathlibStat(pypath.PathStr(p))
	if err != nil {
		panic(err)
	}
	return fi != nil
}

// Model message pointing the agent at each attached image so it runs vision_analyze in loop
func BuildImageRefMessage(userText string, imagePaths []string) string {
	var parts []string
	for _, p := range imagePaths {
		if pathExists(p) {
			parts = append(parts, "[The user attached an image: "+pypath.PathName(p)+"]\n[Examine it with the vision_analyze tool using image_url: "+pypath.PathStr(p)+"]")
		}
	}
	prefix := strings.Join(parts, "\n\n")
	if prefix != "" {
		if userText != "" {
			return prefix + "\n\n" + userText
		}
		return prefix
	}
	if userText != "" {
		return userText
	}
	return "What do you see in this image?"
}

// Persisted user text with an image directive line per attached image after the caption
func BuildPersistMessageWithImageRefs(userText string, imagePaths []string) string {
	var refs []string
	for _, p := range imagePaths {
		if pathExists(p) {
			refs = append(refs, "@image:"+state.FormatReferenceValue(p))
		}
	}
	if len(refs) == 0 {
		return userText
	}
	if userText != "" {
		return userText + "\n" + strings.Join(refs, "\n")
	}
	return strings.Join(refs, "\n")
}

// Persisted user turn shaped like the model payload with only its text part swapped for the image refs form
func BuildPersistUserMessage(userText string, imagePaths []string, runMessage any) any {
	persistText := BuildPersistMessageWithImageRefs(userText, imagePaths)
	parts, ok := runMessage.([]any)
	if !ok {
		return persistText
	}
	out := []any{map[string]any{"type": "text", "text": persistText}}
	for _, part := range parts {
		if d, isDict := part.(map[string]any); isDict && d["type"] == "text" {
			continue
		}
		out = append(out, part)
	}
	return out
}

// Content part kind as a set member, raising TypeError for an unhashable value like a frozenset lookup
func partKind(v any) (string, bool) {
	switch x := v.(type) {
	case string:
		return x, true
	case []any, map[string]any:
		panic(fmt.Errorf("TypeError: unhashable type: '%s'", pyrepr.TypeName(v)))
	}
	return "", false
}

// URL an image part carries as an image_url dict or string, empty otherwise
func historyPartImageURL(part map[string]any) string {
	imageURL := part["image_url"]
	if d, ok := imageURL.(map[string]any); ok {
		imageURL = d["url"]
	}
	s, _ := imageURL.(string)
	return s
}

// First truthy value of a dict's keys, else the empty string
func firstTruthy(d map[string]any, keys ...string) any {
	for _, key := range keys {
		if pyrepr.Truthy(d[key]) {
			return d[key]
		}
	}
	return ""
}

// Text or stand in label rendering of one structured content dict
func historyDictText(content map[string]any, imageURLs bool) string {
	kind, isStr := partKind(content["type"])
	if isStr && historyTextKinds[kind] {
		return pyrepr.Str(firstTruthy(content, "text", "content"))
	}
	if isStr && historyImageKinds[kind] {
		rendered := ""
		if imageURLs {
			rendered = historyPartImageURL(content)
		}
		if rendered == "" {
			return "[image]"
		}
		return rendered
	}
	if isStr && historyAudioKinds[kind] {
		return "[audio]"
	}
	if pyrepr.Truthy(content["type"]) {
		return "[" + pyrepr.Str(content["type"]) + "]"
	}
	if _, ok := content["text"]; ok {
		return pyrepr.Str(firstTruthy(content, "text"))
	}
	return "[structured content]"
}

// Display text of message content with image parts as stand in labels and list parts joined by lines
func ContentDisplayText(content any) string {
	switch x := content.(type) {
	case []any:
		var lines []string
		for _, part := range x {
			if t := pystr.Strip(ContentDisplayText(part)); t != "" {
				lines = append(lines, t)
			}
		}
		return strings.Join(lines, "\n")
	case map[string]any:
		return historyDictText(x, false)
	case nil:
		return ""
	}
	return pyrepr.Str(content)
}

// Plain string of message content keeping image URLs inline unless imageURLs is false
func CoerceMessageText(content any, imageURLs bool) string {
	switch x := content.(type) {
	case []any:
		var chunks strings.Builder
		for _, part := range x {
			if s, ok := part.(string); ok {
				chunks.WriteString(s)
				continue
			}
			d, ok := part.(map[string]any)
			if !ok {
				continue
			}
			if text, isStr := d["text"].(string); isStr {
				chunks.WriteString(text)
			} else if pyrepr.Truthy(d["type"]) {
				rendered := historyDictText(d, imageURLs)
				if kind, isStr := partKind(d["type"]); isStr && historyTextKinds[kind] {
					chunks.WriteString(rendered)
				} else {
					chunks.WriteString("\n" + rendered)
				}
			}
		}
		return chunks.String()
	case map[string]any:
		return historyDictText(x, imageURLs)
	case nil:
		return ""
	}
	return pyrepr.Str(content)
}

// Whether a content part carries only text
func historyTextOnlyPart(part map[string]any) bool {
	kind, isStr := partKind(part["type"])
	if isStr && historyTextKinds[kind] {
		return true
	}
	_, isText := part["text"].(string)
	return part["type"] == nil && isText
}

// Whether a busy submit carries only plain text and no attachment or media
func IsTextOnlyBusyPayload(content any) bool {
	switch x := content.(type) {
	case []any:
		if len(x) == 0 {
			return false
		}
		for _, part := range x {
			if _, ok := part.(string); ok {
				continue
			}
			d, ok := part.(map[string]any)
			if !ok || !historyTextOnlyPart(d) {
				return false
			}
		}
		return true
	case string, bool, int, int64, float64, json.Number, *big.Int:
		return true
	case map[string]any:
		return historyTextOnlyPart(x)
	}
	return false
}

// Whether a row is a gateway system notice persisted as a user row that must never render as a bubble
func IsDisplayHiddenMarker(role, text string) bool {
	return role == "user" && strings.HasPrefix(pystr.Lstrip(text), "[System:")
}

// Slash invocation a skill expanded turn came from, empty when the row is not one
func SkillScaffoldProjection(contentText string) string {
	described, _ := state.DescribeSkillInvocation(contentText, " ")
	return described
}

// Skill message a projected slash invocation expands back into, the text unchanged when not resolvable
func ExpandSkillInvocationForReplay(ctx context.Context, text, taskID string) (out string) {
	head, arg, _ := strings.Cut(pystr.Strip(text), " ")
	if !strings.HasPrefix(head, "/") {
		return text
	}
	defer func() {
		if r := recover(); r != nil {
			slog.Debug("skill re-expansion failed for replay", "error", r)
			out = text
		}
	}()
	scope := skills.NewScope(home.Dir(ctx), nil)
	cmdKey, found, err := scope.ResolveSkillCommandKey(ctx, strings.TrimLeft(head, "/"), true)
	if err != nil {
		slog.Debug("skill re-expansion failed for replay", "error", err)
		return text
	}
	if !found {
		return text
	}
	msg, built, err := scope.BuildSkillInvocationMessage(ctx, cmdKey, pystr.Strip(arg), taskID, "")
	if err != nil {
		slog.Debug("skill re-expansion failed for replay", "error", err)
		return text
	}
	if !built || msg == "" {
		return text
	}
	return msg
}
