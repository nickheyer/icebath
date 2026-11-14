package tuigateway

import (
	"slices"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pydantic"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

// Contract violations raise for the duration of the test as they do under test isolation
func strictContracts(t *testing.T) {
	t.Helper()
	saved := Strict
	Strict = true
	t.Cleanup(func() { Strict = saved })
}

func TestProfileRenameHistorySurvivesResultValidation(t *testing.T) {
	strictContracts(t)
	for _, names := range [][]any{{}, {"old-bot", "older-bot"}} {
		payload := pyjson.ObjectOf("profiles", []any{pyjson.ObjectOf("name", "bot", "path", "/profiles/bot", "previous_names", names)})
		CheckResult(Methods["profiles.list"], payload)
		result, err := ProfilesListResult.Validate(payload)
		if err != nil {
			t.Fatal(err)
		}
		row := result.Get("profiles").([]any)[0].(*pydantic.Instance)
		if !pyrepr.Equal(row.Get("previous_names"), names) {
			t.Fatalf("previous_names %v, want %v", row.Get("previous_names"), names)
		}
	}
	empty, err := ProfilesListResult.Validate(pyjson.ObjectOf("profiles", []any{}))
	if err != nil || len(empty.Get("profiles").([]any)) != 0 {
		t.Fatalf("empty roster %v %v", empty, err)
	}
	legacy, err := ProfilesListResult.Validate(pyjson.ObjectOf("profiles", []any{pyjson.ObjectOf("name", "bot", "path", "/profiles/bot")}))
	if err != nil {
		t.Fatal(err)
	}
	row := legacy.Get("profiles").([]any)[0].(*pydantic.Instance)
	if got := row.Get("previous_names").([]any); len(got) != 0 {
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
		if _, err := ProfilesListResult.Validate(pyjson.ObjectOf("profiles", []any{row})); err == nil {
			t.Fatalf("row with %v validated", extra.Keys)
		}
	}
}

func TestCanonicalAdmissionAndCreationModelsLeaveLegacyWireUnchanged(t *testing.T) {
	canonical := CanonicalMethods["prompt.submit"]
	params := map[string]any{"session_id": "s", "input_id": "input", "text": "hello", "finite": true}
	if _, problem := ValidateParams(canonical, params); problem != "" {
		t.Fatal(problem)
	}
	if _, problem := ValidateParams(Methods["prompt.submit"], params); problem == "" {
		t.Fatal("legacy prompt.submit accepted the canonical admission keys")
	}
	legacyEdit := map[string]any{"session_id": "s", "text": "edit", "truncate_before_row_id": 3, "confirm_truncate": true}
	if _, problem := ValidateParams(Methods["prompt.submit"], legacyEdit); problem != "" {
		t.Fatal(problem)
	}
	if _, problem := ValidateParams(canonical, legacyEdit); problem == "" {
		t.Fatal("canonical prompt.submit accepted the legacy edit keys")
	}
	if !slices.Contains(CanonicalMethods["approval.respond"].Params.ModelFields(), "execution_generation") {
		t.Fatal("canonical approval.respond lacks execution_generation")
	}
	if slices.Contains(Methods["approval.respond"].Params.ModelFields(), "execution_generation") {
		t.Fatal("legacy approval.respond declares execution_generation")
	}
	created, err := CanonicalMethods["session.create"].Params.Validate(map[string]any{"ignore_rules": true})
	if err != nil || created.Get("ignore_rules") != true {
		t.Fatalf("session.create ignore_rules %v %v", created, err)
	}
}

func TestCanonicalCreateContractAcceptsRowCwd(t *testing.T) {
	contract := CanonicalMethods["session.create"]
	if problems := CanonicalParamProblems(contract, map[string]any{"source": "cli", "row_cwd": ""}); len(problems) != 0 {
		t.Fatal(problems)
	}
}

func TestLegacyEventsSinceAcceptsTheProcessEpochClientsSend(t *testing.T) {
	contract := Methods["session.events.since"]
	if _, problem := ValidateParams(contract, map[string]any{"session_id": "s1", "last_seen": 3, "replay_epoch": "proc-1"}); problem != "" {
		t.Fatal(problem)
	}
}

func TestLegacyEventsSinceStillRefusesOtherUnknownKeys(t *testing.T) {
	contract := Methods["session.events.since"]
	_, problem := ValidateParams(contract, map[string]any{"session_id": "s1", "last_seen": 3, "bogus": 1})
	if problem == "" || !strings.Contains(problem, "bogus") {
		t.Fatalf("problem %q", problem)
	}
}
