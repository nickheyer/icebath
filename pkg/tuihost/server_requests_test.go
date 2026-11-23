package tuihost

import (
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

type wireFrame struct{ event, id string }

func wireEvents(frames []wireFrame) []string {
	out := []string{}
	for _, f := range frames {
		out = append(out, f.event)
	}
	return out
}

// Compact JSON of an object for comparisons
func objText(t *testing.T, o *pyjson.Object) string {
	t.Helper()
	text, err := pyjson.Dumps(o)
	if err != nil {
		t.Fatal(err)
	}
	return text
}

func TestTheRuleItself(t *testing.T) {
	locked := pyjson.ObjectOf("q0", "blue")
	kept := `{"answers": {"q0": "blue"}, "outcome": "cancelled"}`
	if got := objText(t, settleBatch(locked, nil, "cancelled")); got != kept {
		t.Fatalf("ended cancelled = %s", got)
	}
	if got := objText(t, settleBatch(locked, pyjson.NewObject(), "")); got != kept {
		t.Fatalf("empty frame = %s", got)
	}
	if got := objText(t, settleBatch(locked, nil, "")); got != kept {
		t.Fatalf("error frame = %s", got)
	}
	bogus := pyjson.ObjectOf("answers", pyjson.NewObject(), "outcome", "bogus")
	if got := objText(t, settleBatch(locked, bogus, "")); got != `{"answers": {"q0": "blue"}, "outcome": "submitted"}` {
		t.Fatalf("bogus outcome = %s", got)
	}
	if settleBatch(locked, nil, "timed_out").GetOr("answers", nil).(*pyjson.Object) == locked {
		t.Fatal("the locked answers were aliased into the result")
	}
	if got := objText(t, settleBatch(locked, nil, "weird")); got != kept {
		t.Fatalf("unknown ending = %s", got)
	}
	extra := pyjson.ObjectOf("answers", pyjson.ObjectOf("q1", "red"), "outcome", "bogus", "extra", 1)
	if got := objText(t, settleBatch(locked, extra, "")); got != `{"answers": {"q0": "blue", "q1": "red"}, "outcome": "submitted", "extra": 1}` {
		t.Fatalf("extra frame keys = %s", got)
	}
}
