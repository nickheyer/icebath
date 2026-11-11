package tuiserver

import (
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/tuigateway"
)

// Replayable projection of the in flight turn, nil when it carries no prompt, text, stream or error
func inflightSnapshot(session *tuigateway.Session) map[string]any {
	turn, isDict := session.Get(inflightTurnKey).(map[string]any)
	if !isDict {
		return nil
	}
	user, assistant := pystr.Strip(pyrepr.Str(orEmpty(turn["user"]))), pyrepr.Str(orEmpty(turn["assistant"]))
	streaming, errText := truthy(turn["streaming"]), pystr.Strip(pyrepr.Str(orEmpty(turn["error"])))
	if user == "" && assistant == "" && !streaming && errText == "" {
		return nil
	}
	snapshot := map[string]any{"assistant": assistant, "streaming": streaming, "user": user}
	if displayKind, isText := turn["display_kind"].(string); isText && displayKind != "" {
		snapshot["display_kind"] = displayKind
	}
	if displayMetadata, isMeta := turn["display_metadata"].(map[string]any); isMeta {
		snapshot["display_metadata"] = copyDict(displayMetadata)
	}
	rawOffsets := unpackSlot(orEmpty(turn["correction_offsets"]))
	corrections, offsets := []any{}, []any{}
	for i, correction := range unpackSlot(orEmpty(turn["corrections"])) {
		text := pyrepr.Str(correction)
		if pystr.Strip(text) == "" {
			continue
		}
		corrections = append(corrections, text)
		if i < len(rawOffsets) {
			offsets = append(offsets, rawOffsets[i])
		} else {
			offsets = append(offsets, nil)
		}
	}
	if len(corrections) > 0 {
		snapshot["corrections"] = corrections
		valid := true
		for _, offset := range offsets {
			if n, isInt := offset.(int); !isInt || n < 0 {
				valid = false
			}
		}
		if valid {
			snapshot["correction_offsets"] = offsets
		}
	}
	if errText != "" {
		status := pyrepr.Str(orEmpty(turn["status"]))
		if status == "" {
			status = "error"
		}
		snapshot["error"], snapshot["status"], snapshot["recoverable"] = errText, status, truthy(turn["recoverable"])
		if surface, isSurface := turn["error_surface"].(map[string]any); isSurface && len(surface) > 0 {
			snapshot["error_surface"] = surface
		}
	}
	return snapshot
}

// Accepted next turn prompt without its transport handle, nil when nothing is queued
func queuedPromptSnapshot(session *tuigateway.Session) map[string]any {
	queued, isDict := session.Get("queued_prompt").(map[string]any)
	user := ""
	if isDict {
		user = inflightText(queued["text"])
	}
	if user == "" {
		return nil
	}
	return map[string]any{"user": user}
}
