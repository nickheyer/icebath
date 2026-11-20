package kanban

import (
	"strings"
	"testing"
)

func TestDefaultWorkflowIsValidAndExcludesArchived(t *testing.T) {
	if err := DefaultWorkflow.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, k := range DefaultWorkflow.Keys() {
		if k == Archived {
			t.Fatal("archived is a column")
		}
	}
}

func TestValidateRejectsInconsistentWorkflows(t *testing.T) {
	w := DefaultWorkflow
	cases := []struct {
		w       Workflow
		message string
	}{
		{Workflow{Columns: append(append([]Column{}, w.Columns...), w.Columns[0]), Manual: w.Manual}, "duplicate"},
		{Workflow{Columns: append(append([]Column{}, w.Columns...), NewColumn(Archived, "A")), Manual: w.Manual}, "reserved"},
		{Workflow{Columns: w.Columns, Manual: FrozenManual(ManualRow{"ready", []string{"gone"}})}, "manual"},
		{Workflow{Columns: w.Columns, Manual: FrozenManual(ManualRow{"gone", []string{"ready"}})}, "manual"},
	}
	for _, c := range cases {
		err := c.w.Validate()
		if err == nil || !strings.Contains(err.Error(), c.message) {
			t.Fatalf("want %q, got %v", c.message, err)
		}
	}
}

func TestDuplicateColumnMessageReprsTheKeyTuple(t *testing.T) {
	w := Workflow{Columns: []Column{NewColumn("a", "A"), NewColumn("a", "B")}, Manual: FrozenManual()}
	if err := w.Validate(); err == nil || err.Error() != "duplicate column keys: a, a" {
		t.Fatal(err)
	}
}

// Column with the default question mark glyph that is offered as a drag target
func NewColumn(key, label string) Column {
	return Column{Key: key, Label: label, Icon: "?", DragTarget: true}
}
