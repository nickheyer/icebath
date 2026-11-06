package gateway

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/state"
)

type displayEventDouble struct {
	messageID any
	internal  bool
	metadata  map[string]any
}

func (e *displayEventDouble) IsInternal() bool              { return e.internal }
func (e *displayEventDouble) EventMetadata() map[string]any { return e.metadata }
func (e *displayEventDouble) HeartbeatSessionID() any       { return nil }
func (e *displayEventDouble) EventMessageID() any           { return e.messageID }

func wantInvalidParams(t *testing.T, err error, label string) {
	t.Helper()
	var rse *state.RuntimeStoreError
	if !errors.As(err, &rse) || rse.Reason != "invalid_params" {
		t.Fatalf("%s: want invalid_params, got %v", label, err)
	}
}

func TestDisplayFieldsAreAdmittedAndBoundToTheirOwnTurn(t *testing.T) {
	preview := strings.Repeat("P", 1500)
	type turn struct {
		rid   string
		extra map[string]any
	}
	type seenRow struct {
		text      string
		kind      any
		metadata  map[string]any
		strayKind any
		strayMeta map[string]any
	}
	var seen []seenRow
	payloads := map[string]map[string]any{}
	for _, tc := range []turn{{"paste", map[string]any{"title_preview": preview}}, {"hidden", map[string]any{"display_kind": "hidden"}}, {"plain", map[string]any{}}} {
		params := map[string]any{"session_id": "s", "submission_id": tc.rid, "text": tc.rid}
		for k, v := range tc.extra {
			params[k] = v
		}
		admitted, err := AdmitDisplay(SubmitDisplayFields(params))
		if err != nil {
			t.Fatalf("%s: %v", tc.rid, err)
		}
		payloads[tc.rid] = admitted
		committed, _ := admitted["display_v1"].(map[string]any)
		ctx := DisplayTurnScope(context.Background(), tc.rid, committed)
		event := &displayEventDouble{messageID: tc.rid, metadata: map[string]any{}}
		stray := &displayEventDouble{messageID: "other", metadata: map[string]any{}}
		seen = append(seen, seenRow{tc.rid, DisplayKindForEvent(ctx, event), DisplayMetadataForEvent(ctx, event), DisplayKindForEvent(ctx, stray), DisplayMetadataForEvent(ctx, stray)})
	}
	_, err := AdmitDisplay(SubmitDisplayFields(map[string]any{"session_id": "s", "submission_id": "minted", "text": "x", "display_kind": "internal_notification"}))
	wantInvalidParams(t, err, "foreign kind")
	if got := payloads["paste"]["display_v1"]; !reflect.DeepEqual(got, map[string]any{"title_preview": strings.Repeat("P", 1000)}) {
		t.Fatalf("paste display_v1 %v", got)
	}
	if got := payloads["hidden"]["display_v1"]; !reflect.DeepEqual(got, map[string]any{"kind": "hidden"}) {
		t.Fatalf("hidden display_v1 %v", got)
	}
	if _, ok := payloads["plain"]["display_v1"]; ok {
		t.Fatal("plain turn committed display_v1")
	}
	want := []seenRow{
		{"paste", nil, map[string]any{"title_preview": strings.Repeat("P", 1000)}, nil, map[string]any{}},
		{"hidden", "hidden", map[string]any{}, nil, map[string]any{}},
		{"plain", nil, map[string]any{}, nil, map[string]any{}},
	}
	if !reflect.DeepEqual(seen, want) {
		t.Fatalf("seen %#v", seen)
	}
}

func TestAdmitDisplayRefusesWhatAdmissionCannotCommit(t *testing.T) {
	for _, params := range []map[string]any{{"display_kind": ""}, {"display_kind": true}, {"title_preview": 7}, {"title_preview": []any{"x"}}} {
		_, err := AdmitDisplay(params)
		wantInvalidParams(t, err, "admit")
	}
	got, err := AdmitDisplay(map[string]any{"display_kind": nil, "title_preview": " \t\n"})
	if err != nil || len(got) != 0 {
		t.Fatalf("blank preview %v %v", got, err)
	}
	got, _ = AdmitDisplay(map[string]any{"title_preview": strings.Repeat("\U0001F600", 1001)})
	if p := got["display_v1"].(map[string]any)["title_preview"].(string); len([]rune(p)) != 1000 {
		t.Fatalf("preview truncated to %d code points", len([]rune(p)))
	}
	if got := SubmitDisplayFields(map[string]any{"text": "x", "title_preview": nil}); !reflect.DeepEqual(got, map[string]any{"title_preview": nil}) {
		t.Fatalf("submit fields %v", got)
	}
}

func TestRestoreDisplayAcceptsOnlyAnAdmittedObject(t *testing.T) {
	for _, ok := range []map[string]any{{"kind": "hidden"}, {"title_preview": "t"}, {"kind": "hidden", "title_preview": "t"}} {
		got, err := RestoreDisplay(ok)
		if err != nil || !reflect.DeepEqual(got, ok) {
			t.Fatalf("restore %v: %v %v", ok, got, err)
		}
	}
	for _, bad := range []any{nil, "hidden", map[string]any{}, map[string]any{"kind": "hidden", "extra": 1}, map[string]any{"kind": nil},
		map[string]any{"kind": "internal_notification"}, map[string]any{"title_preview": "  "}, map[string]any{"title_preview": strings.Repeat("x", 1001)}, map[string]any{"title_preview": 3}} {
		_, err := RestoreDisplay(bad)
		wantInvalidParams(t, err, "restore")
	}
}

func TestSubmissionAndWorkerKwargsFollowTheBoundTurn(t *testing.T) {
	ctx := SubmissionTurnScope(context.Background(), "adm", "sub-1")
	ctx = DisplayTurnScope(ctx, "adm", map[string]any{"kind": "hidden", "title_preview": "Title"})
	event := &displayEventDouble{messageID: "adm", metadata: map[string]any{}}
	if got := PersistedUserMetadataForEvent(ctx, event); !reflect.DeepEqual(got, map[string]any{"title_preview": "Title", "submission_id": "sub-1"}) {
		t.Fatalf("persisted metadata %v", got)
	}
	if got := PersistedUserMetadataForEvent(ctx, &displayEventDouble{messageID: "adm2", metadata: map[string]any{}}); len(got) != 0 {
		t.Fatalf("follow up inherited %v", got)
	}
	if got := PersistedUserMetadataForEvent(ctx, nil); len(got) != 0 {
		t.Fatalf("nil event %v", got)
	}
	want := map[string]any{"persist_user_display_kind": "hidden", "persist_user_display_metadata": map[string]any{"title_preview": "Title"}}
	if got := WorkerDisplayKwargs(ctx); !reflect.DeepEqual(got, want) {
		t.Fatalf("worker kwargs %v", got)
	}
	if got := WorkerDisplayKwargs(context.Background()); len(got) != 0 {
		t.Fatalf("unbound worker kwargs %v", got)
	}
	if got := admissionSubmission(SubmissionTurnScope(ctx, "adm", ""), event); len(got) != 0 {
		t.Fatalf("empty submission id bound %v", got)
	}
	if got := admissionDisplay(DisplayTurnScope(ctx, "adm", map[string]any{}), event); len(got) != 0 {
		t.Fatalf("empty committed bound %v", got)
	}
}
