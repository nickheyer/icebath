package dashboard

import (
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

// Validated model of the keyword arguments, the errors pydantic raises as ValidationError
func validateModel(m *Model, pairs ...any) (*pyjson.Object, []ErrorDetails) {
	out, errs := m.Validate(pyjson.ObjectOf(pairs...))
	if len(errs) > 0 {
		return nil, errs
	}
	return out.(*pyjson.Object), nil
}

func TestMutationCountersRejectCoercion(t *testing.T) {
	for _, tc := range []struct {
		model *Model
		extra []any
	}{{sessionRenameModel, nil}, {sessionImportModel, []any{"sessions", []any{}}}} {
		for _, value := range []any{true, false, "0", 1.0, int64(-1)} {
			if _, errs := validateModel(tc.model, append([]any{"request_id", "edit", "expected_revision", value}, tc.extra...)...); len(errs) == 0 {
				t.Fatalf("%s accepted expected_revision=%v", tc.model.ClassName, value)
			}
		}
	}
	for _, value := range []any{true, "1", 1.0, int64(-1)} {
		if _, errs := validateModel(sessionRenameModel, "request_id", "edit", "expected_revision", int64(0), "expected_generation", value); len(errs) == 0 {
			t.Fatalf("accepted expected_generation=%v", value)
		}
	}
	got, errs := validateModel(sessionRenameModel, "expected_revision", int64(3), "expected_generation", int64(2))
	if len(errs) > 0 || got.Vals["expected_revision"] != int64(3) {
		t.Fatalf("got %v %v", got, errs)
	}
}

func TestSidebarFlagsRejectCoercion(t *testing.T) {
	for _, field := range []string{"archived", "hidden", "pinned", "unread"} {
		for _, value := range []any{int64(0), int64(1), "false", "true"} {
			if _, errs := validateModel(sessionRenameModel, field, value); len(errs) == 0 {
				t.Fatalf("%s accepted %v", field, value)
			}
		}
		got, errs := validateModel(sessionRenameModel, field, false)
		if len(errs) > 0 || got.Vals[field] != false {
			t.Fatalf("%s = %v %v", field, got, errs)
		}
	}
}
