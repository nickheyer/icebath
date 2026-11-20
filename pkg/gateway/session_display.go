package gateway

import (
	"context"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/state"
)

const (
	DisplayHidden            = "hidden"
	displayTitlePreviewLimit = 1000
)

var displaySubmitFields = []string{"display_kind", "title_preview"}

type displayTurnKey struct{}

type submissionTurnKey struct{}

type displayTurn struct {
	admissionID string
	committed   map[string]any
}

type submissionTurn struct {
	admissionID  string
	submissionID string
}

// Raw per turn presentation fields of a prompt.submit request forwarded to admission
func SubmitDisplayFields(params map[string]any) map[string]any {
	out := map[string]any{}
	for _, key := range displaySubmitFields {
		if v, ok := params[key]; ok {
			out[key] = v
		}
	}
	return out
}

func displayInvalidParams() error { return state.NewRuntimeStoreError("invalid_params", nil, nil) }

// Wire display fields become the committed display_v1 object, refusing every kind but hidden
func AdmitDisplay(params map[string]any) (map[string]any, error) {
	kind, preview := params["display_kind"], params["title_preview"]
	if kind != nil && kind != any(DisplayHidden) {
		return nil, displayInvalidParams()
	}
	text, isStr := preview.(string)
	if preview != nil && !isStr {
		return nil, displayInvalidParams()
	}
	committed := map[string]any{}
	if kind != nil {
		committed["kind"] = DisplayHidden
	}
	if text != "" && pystr.Strip(text) != "" {
		if rs := pystr.Runes(text); len(rs) > displayTitlePreviewLimit {
			text = pystr.FromRunes(rs[:displayTitlePreviewLimit])
		}
		committed["title_preview"] = text
	}
	if len(committed) == 0 {
		return map[string]any{}, nil
	}
	return map[string]any{"display_v1": committed}, nil
}

// Committed display_v1 crossing a process boundary is accepted only when admission could have committed it
func RestoreDisplay(committed any) (map[string]any, error) {
	obj, ok := committed.(map[string]any)
	if !ok || len(obj) == 0 {
		return nil, displayInvalidParams()
	}
	for key := range obj {
		if key != "kind" && key != "title_preview" {
			return nil, displayInvalidParams()
		}
	}
	admitted, err := AdmitDisplay(map[string]any{"display_kind": obj["kind"], "title_preview": obj["title_preview"]})
	if err != nil {
		return nil, err
	}
	again, _ := admitted["display_v1"].(map[string]any)
	if len(again) != len(obj) {
		return nil, displayInvalidParams()
	}
	for key, want := range again {
		if got, isStr := obj[key].(string); !isStr || got != want {
			return nil, displayInvalidParams()
		}
	}
	return obj, nil
}

// Binds the committed presentation to the admission event for the returned context only
func DisplayTurnScope(ctx context.Context, admissionID string, committed map[string]any) context.Context {
	var bound *displayTurn
	if len(committed) > 0 {
		bound = &displayTurn{admissionID: admissionID, committed: committed}
	}
	return context.WithValue(ctx, displayTurnKey{}, bound)
}

// Binds the client submission id to the admission event for the returned context only
func SubmissionTurnScope(ctx context.Context, admissionID, submissionID string) context.Context {
	var bound *submissionTurn
	if submissionID != "" {
		bound = &submissionTurn{admissionID: admissionID, submissionID: submissionID}
	}
	return context.WithValue(ctx, submissionTurnKey{}, bound)
}

func boundToEvent(event DisplayEvent, admissionID string) bool {
	if event == nil {
		return false
	}
	id, ok := event.EventMessageID().(string)
	return ok && id == admissionID
}

// Submission id of the admission the event executes, empty for every other event
func admissionSubmission(ctx context.Context, event DisplayEvent) map[string]any {
	bound, _ := ctx.Value(submissionTurnKey{}).(*submissionTurn)
	if bound == nil || !boundToEvent(event, bound.admissionID) {
		return map[string]any{}
	}
	return map[string]any{"submission_id": bound.submissionID}
}

// Committed presentation of the admission the event executes, empty for a follow up on the same task
func admissionDisplay(ctx context.Context, event DisplayEvent) map[string]any {
	bound, _ := ctx.Value(displayTurnKey{}).(*displayTurn)
	if bound == nil || !boundToEvent(event, bound.admissionID) {
		return map[string]any{}
	}
	return bound.committed
}

// Persistence keyword arguments of the managed child single admitted turn
func WorkerDisplayKwargs(ctx context.Context) map[string]any {
	committed := map[string]any{}
	if bound, _ := ctx.Value(displayTurnKey{}).(*displayTurn); bound != nil {
		committed = bound.committed
	}
	out := map[string]any{}
	if committed["kind"] == any(DisplayHidden) {
		out["persist_user_display_kind"] = DisplayHidden
	}
	if preview, ok := committed["title_preview"]; ok && pyrepr.Truthy(preview) {
		out["persist_user_display_metadata"] = map[string]any{"title_preview": preview}
	}
	return out
}
