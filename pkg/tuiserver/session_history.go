package tuiserver

import (
	"context"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/tools"
	"github.com/nickheyer/icebath/pkg/tuigateway"
)

const (
	turnFailureDetailLimit   = 240
	turnPromptEchoWindow     = 24
	turnPromptEchoMaxPrompt  = 65536
	inflightTurnKey          = "inflight_turn"
	submitUserRowSessionSlot = "_submit_user_row"
)

// Seams over the configured main route and the forced redactor, swapped only where a hermes case patches them
var (
	readMainProvider      = provider.AuxReadMainProvider
	readMainModel         = provider.AuxReadMainModel
	redactSensitiveForced = func(text string) string {
		return tools.RedactSensitiveTextWith(nil, text, tools.RedactOptions{Force: true})
	}
)

// Route a live agent reports
type routedAgent interface {
	Provider() string
	Model() string
}

// Live provider and model of the agent, else the configured main route before the agent starts
func activeImageRoutingIdentity(ctx context.Context, a routedAgent) (string, string) {
	providerName, model := "", ""
	if a != nil && !isNilAgent(a) {
		providerName, model = a.Provider(), a.Model()
	}
	if providerName == "" {
		providerName = readMainProvider(ctx)
	}
	if model == "" {
		model = readMainModel(ctx)
	}
	return providerName, model
}

// Whether the interface holds a nil agent pointer
func isNilAgent(a routedAgent) bool {
	p, ok := a.(*agent.AIAgent)
	return ok && p == nil
}

// Seed rows a client may author, user assistant and system text with only the hidden display kind kept
func coerceSeedHistory(value any) []any {
	history := []any{}
	items, _ := value.([]any)
	for _, raw := range items {
		item, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		role, _ := item["role"].(string)
		if role != "user" && role != "assistant" && role != "system" {
			continue
		}
		content := item["content"]
		if content == nil {
			content = item["text"]
		}
		text, isStr := content.(string)
		if !isStr || pystr.Strip(text) == "" {
			continue
		}
		row := map[string]any{"role": role, "content": text}
		if item["display_kind"] == "hidden" {
			row["display_kind"] = "hidden"
		}
		history = append(history, row)
	}
	return history
}

// Stripped display text of a prompt or correction
func inflightText(value any) string {
	return pystr.Strip(tuigateway.ContentDisplayText(value))
}

// Unix seconds as a float like time.time
func nowSeconds() float64 { return float64(time.Now().UnixNano()) / 1e9 }

// Opens the replayable in flight turn snapshot for the prompt
func startInflightTurn(session *tuigateway.Session, text any, displayKind string, displayMetadata map[string]any) {
	now := nowSeconds()
	turn := map[string]any{"assistant": "", "started_at": now, "streaming": true, "updated_at": now, "user": inflightText(text)}
	if displayKind != "" {
		turn["display_kind"] = displayKind
	}
	if displayMetadata != nil {
		meta := make(map[string]any, len(displayMetadata))
		for k, v := range displayMetadata {
			meta[k] = v
		}
		turn["display_metadata"] = meta
	}
	session.Set(inflightTurnKey, turn)
}

// Appends streamed assistant text to the in flight turn
func appendInflightDelta(session *tuigateway.Session, delta any) {
	text := ""
	if delta != nil {
		text = pyrepr.Str(delta)
	}
	if text == "" {
		return
	}
	turn, ok := session.Get(inflightTurnKey).(map[string]any)
	if !ok {
		turn = map[string]any{"assistant": "", "streaming": true, "user": ""}
	}
	turn["assistant"] = pyrepr.Str(orEmpty(turn["assistant"])) + text
	turn["streaming"] = true
	turn["updated_at"] = nowSeconds()
	session.Set(inflightTurnKey, turn)
}

// Value when truthy, else the empty string like a Python or chain
func orEmpty(v any) any {
	if pyrepr.Truthy(v) {
		return v
	}
	return ""
}

// Items of a truthy list or string slot like unpacking it, empty otherwise
func unpackSlot(v any) []any {
	switch x := v.(type) {
	case []any:
		return append([]any{}, x...)
	case string:
		out := []any{}
		for _, r := range pystr.Runes(x) {
			out = append(out, pystr.FromRunes([]rune{r}))
		}
		return out
	}
	return []any{}
}

// Appends an accepted mid turn correction and its arrival offset to the live turn
func recordInflightCorrection(session *tuigateway.Session, text any) {
	correction := inflightText(text)
	current, ok := session.Get(inflightTurnKey).(map[string]any)
	if correction == "" || !ok {
		return
	}
	turn := make(map[string]any, len(current)+3)
	for k, v := range current {
		turn[k] = v
	}
	turn["corrections"] = append(unpackSlot(turn["corrections"]), correction)
	turn["correction_offsets"] = append(unpackSlot(turn["correction_offsets"]), pystr.Len(pyrepr.Str(orEmpty(turn["assistant"]))))
	turn["updated_at"] = nowSeconds()
	session.Set(inflightTurnKey, turn)
}

// Drops the in flight turn and the submit time row a later turn must not adopt
func clearInflightTurn(session *tuigateway.Session) {
	session.Set(inflightTurnKey, nil)
	session.Pop(submitUserRowSessionSlot)
}

// Message of a raised or returned error, the type name when an exception carries no text
func errorMessage(err any) string {
	if e, ok := err.(error); ok {
		if msg := e.Error(); msg != "" {
			return msg
		}
		return pyrepr.TypeName(e)
	}
	return pyrepr.Str(err)
}

// Marks the in flight turn terminal error while keeping it replayable for a resuming client
func failInflightTurn(session *tuigateway.Session, err any, errorSurface map[string]any) {
	message := errorMessage(err)
	now := nowSeconds()
	turn, ok := session.Get(inflightTurnKey).(map[string]any)
	if !ok {
		turn = map[string]any{"assistant": "", "user": "", "started_at": now}
	}
	if message == "" {
		message = "turn failed"
	}
	turn["assistant"] = pyrepr.Str(orEmpty(turn["assistant"]))
	turn["user"] = pyrepr.Str(orEmpty(turn["user"]))
	turn["error"], turn["status"], turn["recoverable"] = message, "error", true
	if len(errorSurface) > 0 {
		surface := make(map[string]any, len(errorSurface))
		for k, v := range errorSurface {
			surface[k] = v
		}
		turn["error_surface"] = surface
	} else {
		delete(turn, "error_surface")
	}
	turn["streaming"], turn["updated_at"] = false, now
	session.Set(inflightTurnKey, turn)
	session.Pop(submitUserRowSessionSlot)
}

// Message with every run of the submitted prompt it quotes back replaced by a prompt marker
func stripPromptEcho(message string, prompt any) string {
	if message == "" || !pyrepr.Truthy(prompt) {
		return message
	}
	needle := pystr.Slice(strings.Join(pystr.SplitWhitespace(pyrepr.Str(prompt), -1), " "), 0, turnPromptEchoMaxPrompt)
	window := turnPromptEchoWindow
	needleRunes, msg := pystr.Runes(needle), pystr.Runes(message)
	if len(needleRunes) < window || len(msg) < window {
		return message
	}
	shingles := map[string]bool{}
	for i := 0; i+window <= len(needleRunes); i++ {
		shingles[string(needleRunes[i:i+window])] = true
	}
	dumped, err := pyjson.Dumps(needle)
	if err != nil {
		panic(err)
	}
	escaped := pystr.Runes(pystr.Slice(dumped, 1, -1))
	if string(escaped) != needle {
		for i := 0; i+window <= len(escaped); i++ {
			shingles[string(escaped[i:i+window])] = true
		}
	}
	var out strings.Builder
	i, n := 0, len(msg)
	for i <= n-window {
		if shingles[string(msg[i:i+window])] {
			j := i + window
			for j < n && shingles[string(msg[j-window+1:j+1])] {
				j++
			}
			out.WriteString("<prompt>")
			i = j
		} else {
			out.WriteString(string(msg[i]))
			i++
		}
	}
	out.WriteString(string(msg[i:]))
	return out.String()
}

// Redacted failure cause fragment for the turn finished bookend, empty when there is nothing to say
func turnFailureDetail(err, reason, prompt any) string {
	reasonText := pystr.Strip(pyrepr.Str(orEmpty(reason)))
	message := pystr.Strip(pyrepr.Str(orEmpty(err)))
	if e, ok := err.(error); ok {
		message = pystr.Strip(e.Error())
		if message == "" {
			message = pyrepr.TypeName(e)
		}
	}
	if message == "" && reasonText == "" {
		return ""
	}
	message = redactForced(message)
	message = strings.Join(pystr.SplitWhitespace(message, -1), " ")
	message = stripPromptEcho(message, prompt)
	if pystr.Len(message) > turnFailureDetailLimit {
		message = pystr.Slice(message, 0, turnFailureDetailLimit) + "…"
	}
	out := ""
	if reasonText != "" {
		out = " failure_reason=" + strings.Join(pystr.SplitWhitespace(reasonText, -1), " ")
	}
	if message != "" {
		out += " cause=" + pyrepr.Repr(message)
	}
	return out
}

// Force redacted text, a fixed marker when redaction itself fails so the record never fails open
func redactForced(text string) (out string) {
	defer func() {
		if recover() != nil {
			out = "<unredactable>"
		}
	}()
	return redactSensitiveForced(text)
}
