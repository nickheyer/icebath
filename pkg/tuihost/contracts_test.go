package tuihost

import (
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/tuigateway"
)

func TestProfileRenameHistorySurvivesResultValidation(t *testing.T) {
	for _, names := range [][]any{{}, {"old-bot", "older-bot"}} {
		payload := pyjson.ObjectOf("profiles", []any{pyjson.ObjectOf("name", "bot", "path", "/profiles/bot", "previous_names", names)})
		if err := tuigateway.ProfilesList.Entry().CheckResult(payload); err != nil {
			t.Fatal(err)
		}
		result, err := tuigateway.ProfilesList.Result(payload)
		if err != nil {
			t.Fatal(err)
		}
		if got := result.Profiles[0].PreviousNames; len(got) != len(names) || len(names) == 2 && (got[0] != names[0] || got[1] != names[1]) {
			t.Fatalf("previous_names %v, want %v", got, names)
		}
	}
	empty, err := tuigateway.ProfilesList.Result(pyjson.ObjectOf("profiles", []any{}))
	if err != nil || len(empty.Profiles) != 0 {
		t.Fatalf("empty roster %v %v", empty, err)
	}
	legacy, err := tuigateway.ProfilesList.Result(pyjson.ObjectOf("profiles", []any{pyjson.ObjectOf("name", "bot", "path", "/profiles/bot")}))
	if err != nil {
		t.Fatal(err)
	}
	if got := legacy.Profiles[0].PreviousNames; len(got) != 0 {
		t.Fatalf("legacy previous_names %v", got)
	}
}

func TestProfileRenameHistoryKeepsStrictResultValidation(t *testing.T) {
	for _, extra := range []*pyjson.Object{
		pyjson.ObjectOf("previous_names", []any{nil}),
		pyjson.ObjectOf("previous_names", "old-bot"),
		pyjson.ObjectOf("unknown_field", []any{}),
	} {
		row := pyjson.ObjectOf("name", "bot", "path", "/profiles/bot")
		row.Update(extra)
		if err := tuigateway.ProfilesList.Entry().CheckResult(pyjson.ObjectOf("profiles", []any{row})); err == nil {
			t.Fatalf("row with %v validated", extra.Keys)
		}
	}
}

func TestCanonicalAdmissionAndCreationModelsLeaveLegacyWireUnchanged(t *testing.T) {
	canonical, legacy := tuigateway.Lookup(tuigateway.KindCanonical, "prompt.submit"), tuigateway.Lookup(tuigateway.KindMethod, "prompt.submit")
	params := map[string]any{"session_id": "s", "input_id": "input", "text": "hello", "finite": true}
	if err := canonical.FitParams(params); err != nil {
		t.Fatal(err)
	}
	if err := legacy.FitParams(params); err == nil {
		t.Fatal("legacy prompt.submit accepted the canonical admission keys")
	}
	legacyEdit := map[string]any{"session_id": "s", "text": "edit", "truncate_before_row_id": 3, "confirm_truncate": true}
	if err := legacy.FitParams(legacyEdit); err != nil {
		t.Fatal(err)
	}
	if err := canonical.FitParams(legacyEdit); err == nil {
		t.Fatal("canonical prompt.submit accepted the legacy edit keys")
	}
	if !tuigateway.Lookup(tuigateway.KindCanonical, "approval.respond").TakesParam("execution_generation") {
		t.Fatal("canonical approval.respond lacks execution_generation")
	}
	if tuigateway.Lookup(tuigateway.KindMethod, "approval.respond").TakesParam("execution_generation") {
		t.Fatal("legacy approval.respond declares execution_generation")
	}
	var created tuigateway.CanonicalCreateParams
	if err := tuigateway.Strict(map[string]any{"ignore_rules": true}, &created); err != nil || !created.IgnoreRules {
		t.Fatalf("session.create ignore_rules %v %v", created, err)
	}
}

func TestCanonicalCreateContractAcceptsRowCwd(t *testing.T) {
	if err := tuigateway.Lookup(tuigateway.KindCanonical, "session.create").FitParams(map[string]any{"source": "cli", "row_cwd": ""}); err != nil {
		t.Fatal(err)
	}
}
