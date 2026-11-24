package pyast

import (
	"strings"
	"testing"
)

func nodeTypes(nodes []*Node) []string {
	out := make([]string, len(nodes))
	for i, n := range nodes {
		out[i] = n.Type
	}
	return out
}

func TestWalkIterChildNodesMatchCPython(t *testing.T) {
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

// Results recorded from ast.literal_eval(value.strip()) with str of each list item under CPython 3.14
func TestLiteralStrListMatchesCPython(t *testing.T) {
	cases := []struct {
		src     string
		want    []string
		ok      bool
		wantErr string
	}{
		{`["browser", "terminal"]`, []string{"browser", "terminal"}, true, ""},
		{"['a', 1, 2.5, None, True, (1, 2), {'k': [1]}, {1}, b'x', 1j, ..., -0.0, 1e400, -3]", []string{"a", "1", "2.5", "None", "True", "(1, 2)", "{'k': [1]}", "{1}", "b'x'", "1j", "Ellipsis", "-0.0", "inf", "-3"}, true, ""},
		{"[1, 2", nil, false, ""},
		{"[x]", nil, false, ""},
		{"  [ 'a' ]  \t", []string{"a"}, true, ""},
		{"[{[]: 1}]", nil, false, "cannot use 'list' as a dict key (unhashable type: 'list')"},
		{"[0x" + strings.Repeat("f", 4000) + "]", nil, false, "Exceeds the limit (4300 digits) for integer string conversio"},
		{"{'a': 1}", nil, false, ""},
		{"[[1, 'a'], ('b',), 'it''s']", []string{"[1, 'a']", "('b',)", "its"}, true, ""},
		{"\t[1]", []string{"1"}, true, ""},
	}
	for _, c := range cases {
		got, ok, err := LiteralStrList(c.src)
		if (err == nil) != (c.wantErr == "") || (err != nil && !strings.HasPrefix(err.Error(), c.wantErr)) {
			t.Errorf("%.40q: err %v want %q", c.src, err, c.wantErr)
			continue
		}
		if ok != c.ok || strings.Join(got, "\x00") != strings.Join(c.want, "\x00") {
			t.Errorf("%.40q: got %q %v want %q", c.src, got, ok, c.want)
		}
	}
}
