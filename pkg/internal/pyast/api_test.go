package pyast

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

func nodeTypes(nodes []*Node) []string {
	out := make([]string, len(nodes))
	for i, n := range nodes {
		out[i] = n.Type
	}
	return out
}

func TestWalkIterChildNodesAndDocstringMatchCPython(t *testing.T) {
	tree, err := Parse("def f(a, b=1):\n    \"\"\"  Doc line.\n\n\tindented\n    \"\"\"\n    return a + b\nclass C: pass\n")
	if err != nil {
		t.Fatal(err)
	}
	wantWalk := []string{"Module", "FunctionDef", "ClassDef", "arguments", "Expr", "Return", "Pass", "arg", "arg", "Constant", "Constant", "BinOp", "Name", "Add", "Name", "Load", "Load"}
	if got := nodeTypes(Walk(tree)); !equalStrings(got, wantWalk) {
		t.Errorf("Walk = %v, want %v", got, wantWalk)
	}
	fn := tree.Children("body")[0]
	if got := nodeTypes(IterChildNodes(fn)); !equalStrings(got, []string{"arguments", "Expr", "Return"}) {
		t.Errorf("IterChildNodes = %v", got)
	}
	if doc, ok, _ := GetDocstring(fn, true); !ok || doc != "Doc line.\n\nindented" {
		t.Errorf("clean docstring = %q %v", doc, ok)
	}
	if doc, ok, _ := GetDocstring(fn, false); !ok || doc != "  Doc line.\n\n\tindented\n    " {
		t.Errorf("raw docstring = %q %v", doc, ok)
	}
	if _, ok, _ := GetDocstring(tree.Children("body")[1], true); ok {
		t.Error("class without docstring reported one")
	}
	if _, ok, _ := GetDocstring(tree, true); ok {
		t.Error("module without docstring reported one")
	}
	_, _, err = GetDocstring(fn.Children("body")[1], true)
	var de *DocstringError
	if !errors.As(err, &de) || err.Error() != "'Return' can't have docstrings" {
		t.Errorf("GetDocstring(Return) error = %v", err)
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestLiteralEvalMatchesCPython(t *testing.T) {
	var cases []struct {
		Src  string   `json:"src"`
		Repr *string  `json:"repr"`
		Err  []string `json:"err"`
	}
	if err := json.Unmarshal(readGzip(t, "literal_eval.json.gz"), &cases); err != nil {
		t.Fatal(err)
	}
	for _, c := range cases {
		v, err := LiteralEval(c.Src)
		var got string
		switch {
		case err == nil:
			got = pyrepr.Repr(v)
			if r, rerr := ConstantRepr(v); rerr != nil {
				got = "err:ValueError|" + rerr.Error()
			} else if _, isInt := v.(interface{ Text(int) string }); isInt {
				got = r
			}
		default:
			var se *SyntaxError
			var le *LiteralError
			switch {
			case errors.As(err, &se):
				got = "err:" + se.Class + "|" + se.Msg
			case errors.As(err, &le):
				got = "err:" + le.Class + "|" + le.Msg
			default:
				got = "err:" + err.Error()
			}
		}
		want := ""
		if c.Repr != nil {
			want = *c.Repr
		} else {
			want = "err:" + c.Err[0] + "|" + c.Err[1]
		}
		if got != want {
			t.Errorf("LiteralEval(%q)\n got %s\nwant %s", firstRunes(c.Src, 40), firstRunes(got, 300), firstRunes(want, 300))
		}
	}
}
