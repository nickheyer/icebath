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

func TestArchiveIsAlwaysAManualMove(t *testing.T) {
	for _, k := range DefaultWorkflow.Keys() {
		if ok, err := DefaultWorkflow.CanMove(k, Archived); err != nil || !ok {
			t.Fatalf("%s -> archived: %v %v", k, ok, err)
		}
		if ok, err := DefaultWorkflow.CanMove(k, "running"); err != nil || ok {
			t.Fatalf("%s -> running allowed: %v %v", k, ok, err)
		}
	}
}

func TestCanMoveIsTotalOverTaskStatuses(t *testing.T) {
	for src := range ValidStatuses {
		if ok, err := DefaultWorkflow.CanMove(src, src); err != nil || ok {
			t.Fatalf("%s same column move: %v %v", src, ok, err)
		}
		any := false
		for dst := range ValidStatuses {
			ok, err := DefaultWorkflow.CanMove(src, dst)
			if err != nil {
				t.Fatal(err)
			}
			any = any || ok
		}
		if !any {
			t.Fatalf("%s has no move", src)
		}
	}
}

func TestUnknownColumnKeysAreRejectedNotDefaulted(t *testing.T) {
	for _, pair := range [][2]string{{"bogus", Archived}, {"ready", "bogus"}} {
		_, err := DefaultWorkflow.CanMove(pair[0], pair[1])
		if err == nil || !strings.Contains(err.Error(), "unknown kanban workflow column") {
			t.Fatalf("%v: %v", pair, err)
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
	if err := w.Validate(); err == nil || err.Error() != "duplicate column keys: ('a', 'a')" {
		t.Fatal(err)
	}
}

func TestWorkflowToDictKeepsManualOrderAndSortsDestinations(t *testing.T) {
	d := DefaultWorkflow.ToDict()
	manual := d.Vals["manual"].(interface{ Len() int })
	if manual.Len() != 9 {
		t.Fatalf("manual rows %d", manual.Len())
	}
	b, err := d.MarshalJSON()
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	if !strings.Contains(s, `"triage":["ready","todo"]`) || !strings.HasSuffix(s, `"archived":"archived"}`) ||
		!strings.Contains(s, `{"key":"scheduled","label":"Scheduled","icon":"⏱","drag_target":false}`) {
		t.Fatal(s)
	}
}
