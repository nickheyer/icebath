package tuigateway

import (
	"context"
	"slices"
	"strings"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Display metadata flag on a held turn's published reply whose commentary no projection shows
const CommentaryWithheld = "commentary_withheld"

// Commentary with think blocks stripped and secrets redacted under the context profile policy like live delivery
func VisibleCommentary(ctx context.Context, text string) string {
	visible := pystr.Strip(provider.StripThinkBlocks(text))
	if visible == "" {
		return visible
	}
	return tools.RedactSensitiveTextInProfile(ctx, visible)
}

// AttributeError a get on display metadata that is not a dict raises
type DisplayMetadataError struct{ TypeName string }

func (e *DisplayMetadataError) Error() string {
	return "'" + e.TypeName + "' object has no attribute 'get'"
}

// Python exception class the error stands for
func (e *DisplayMetadataError) PyTypeName() string { return "AttributeError" }

// Output text of the assistant message items whose phase is in phases, the nil key standing for no phase
func phaseMessageItems(message map[string]any, phases map[string]bool, unphased bool) []string {
	items := message["codex_message_items"]
	if s, ok := items.(string); ok {
		decoded, err := pyjson.Loads(s)
		if err != nil {
			return []string{}
		}
		items = pyjson.Plain(decoded)
	}
	list, ok := items.([]any)
	if !ok {
		return []string{}
	}
	result := []string{}
	for _, raw := range list {
		item, ok := raw.(map[string]any)
		if !ok || item["type"] != "message" || item["role"] != "assistant" {
			continue
		}
		phase, content := item["phase"], item["content"]
		phaseStr, isStr := phase.(string)
		if phase != nil && !isStr {
			continue
		}
		parts, isList := content.([]any)
		if isStr && !phases[strings.ToLower(pystr.Strip(phaseStr))] || !isStr && !unphased || !isList {
			continue
		}
		var b strings.Builder
		for _, p := range parts {
			part, ok := p.(map[string]any)
			if !ok || part["type"] != "output_text" {
				continue
			}
			if text, ok := part["text"].(string); ok && pystr.Strip(text) != "" {
				b.WriteString(text)
			}
		}
		if text := pystr.Strip(b.String()); text != "" {
			result = append(result, text)
		}
	}
	return result
}

// Commentary phase texts of an assistant row
func CommentaryItems(message map[string]any) []string {
	return phaseMessageItems(message, map[string]bool{"commentary": true}, false)
}

// Final phase texts of an assistant row, unphased messages counting as final
func finalItems(message map[string]any) []string {
	return phaseMessageItems(message, map[string]bool{"final": true, "final_answer": true}, true)
}

// Reasoning display copy with each whole line public commentary segment removed
func withoutFlattenedCommentary(reasoning string, commentary []string) string {
	seen := map[string]bool{}
	var unique []string
	for _, text := range commentary {
		if !seen[text] {
			seen[text] = true
			unique = append(unique, text)
		}
	}
	slices.SortStableFunc(unique, func(a, b string) int { return pystr.Len(b) - pystr.Len(a) })
	remaining := reasoning
	for _, text := range unique {
		searchFrom := 0
		for {
			rel := strings.Index(remaining[searchFrom:], text)
			if rel < 0 {
				break
			}
			at := searchFrom + rel
			end := at + len(text)
			if (at == 0 || remaining[at-1] == '\n') && (end == len(remaining) || remaining[end] == '\n') {
				before, after := remaining[:at], remaining[end:]
				prefix, suffix := strings.TrimRight(before, "\n"), strings.TrimLeft(after, "\n")
				separator := "\n"
				if strings.HasSuffix(before, "\n\n") || strings.HasPrefix(after, "\n\n") {
					separator = "\n\n"
				}
				if prefix != "" && suffix != "" {
					remaining = prefix + separator + suffix
				} else {
					remaining = prefix + suffix
				}
				searchFrom = 0
			} else {
				searchFrom = end
			}
		}
	}
	return remaining
}

// Display copy of one assistant row with its commentary projected under the profile policy
func projectOne(ctx context.Context, message map[string]any, enabled bool) (map[string]any, error) {
	if message["role"] != "assistant" || message["display_kind"] == "hidden" {
		return message, nil
	}
	raw := CommentaryItems(message)
	if len(raw) == 0 {
		return message, nil
	}
	projected := make(map[string]any, len(message)+3)
	for k, v := range message {
		projected[k] = v
	}
	projected["display_commentary"] = []any{}
	if enabled {
		withheld, err := withheldCommentary(message["display_metadata"])
		if err != nil {
			return nil, err
		}
		if !withheld {
			visible := []any{}
			for _, text := range raw {
				if part := VisibleCommentary(ctx, text); part != "" {
					visible = append(visible, part)
				}
			}
			projected["display_commentary"] = visible
		}
	}
	reasoning := firstTruthy(message, "reasoning", "reasoning_content", "reasoning_details")
	if s, ok := reasoning.(string); ok {
		projected["display_reasoning"] = withoutFlattenedCommentary(s, raw)
	}
	content, ok := message["display_content"]
	if !ok {
		if content, ok = message["content"]; !ok {
			content = message["text"]
		}
	}
	if s, isStr := content.(string); isStr && slices.ContainsFunc(raw, func(t string) bool { return strings.Contains(s, t) }) {
		finals := finalItems(message)
		if !(len(finals) > 0 && pystr.Strip(s) == strings.Join(finals, "\n")) {
			projected["display_content"] = VisibleCommentary(ctx, s)
			if slices.ContainsFunc(raw, func(t string) bool { return strings.HasPrefix(s, t) }) {
				projected["display_commentary"] = []any{}
			}
		}
	}
	return projected, nil
}

// Whether display metadata marks the commentary withheld, a truthy non dict failing like a missing get
func withheldCommentary(meta any) (bool, error) {
	if !pyrepr.Truthy(meta) {
		return false, nil
	}
	switch d := meta.(type) {
	case map[string]any:
		return pyrepr.Truthy(d[CommentaryWithheld]), nil
	case *pyjson.Object:
		return pyrepr.Truthy(d.GetOr(CommentaryWithheld, nil)), nil
	}
	return false, &DisplayMetadataError{TypeName: pyrepr.TypeName(meta)}
}

// Whether commentary display is enabled by the profile config, false when the policy is unreadable
func commentaryEnabled(ctx context.Context) (enabled bool) {
	defer func() {
		if recover() != nil {
			enabled = false
		}
	}()
	cfg, err := config.LoadContext(ctx, home.Dir(ctx), nil)
	if err != nil {
		return false
	}
	display := map[string]any{}
	if raw := cfg["display"]; pyrepr.Truthy(raw) {
		d, ok := raw.(map[string]any)
		if !ok {
			return false
		}
		display = d
	}
	return truthyValueDefault(display["show_commentary"], true) && truthyValueDefault(display["interim_assistant_messages"], true)
}

// Shared truthy coercion with the given default for an absent value
func truthyValueDefault(v any, def bool) bool {
	if v == nil {
		return def
	}
	return provider.IsTruthyValue(v)
}

// History rows with Codex commentary projected inside the owning profile's config and redaction scope
func ProjectHistoryCommentary(ctx context.Context, messages []any, profileHome string) ([]any, error) {
	if !slices.ContainsFunc(messages, func(m any) bool {
		d, ok := m.(map[string]any)
		return ok && pyrepr.Truthy(d["codex_message_items"])
	}) {
		return messages, nil
	}
	if profileHome != "" {
		ctx = home.WithOverride(ctx, profileHome)
	}
	enabled := commentaryEnabled(ctx)
	out := make([]any, len(messages))
	for i, m := range messages {
		d, ok := m.(map[string]any)
		if !ok {
			out[i] = m
			continue
		}
		projected, err := projectOne(ctx, d, enabled)
		if err != nil {
			return nil, err
		}
		out[i] = projected
	}
	return out, nil
}
