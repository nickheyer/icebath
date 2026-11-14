package pyast

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/big"
	"strconv"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

type nodeRow struct {
	Name, Mode, Fn string
	Opt            int
	Tree           any
	Out            *string
	Err            []any
	Crash          bool
}

// Rebuilds a Python value recorded by node_rec.py, nodes keeping only the attributes the object had
func decodePyValue(t *testing.T, v any) any {
	switch x := v.(type) {
	case nil, bool, string:
		return x
	case []any:
		out := make([]any, len(x))
		nodes, names := len(x) > 0, len(x) > 0
		for i, e := range x {
			out[i] = decodePyValue(t, e)
			n, ok := out[i].(*Node)
			nodes = nodes && ok && n != nil
			_, ok = out[i].(string)
			names = names && ok
		}
		if nodes {
			ns := make([]*Node, len(out))
			for i, e := range out {
				ns[i] = e.(*Node)
			}
			return ns
		}
		if names {
			ss := make([]string, len(out))
			for i, e := range out {
				ss[i] = e.(string)
			}
			return ss
		}
		return out
	case map[string]any:
		if typ, ok := x["node"].(string); ok {
			s := specs[typ]
			vals := make([]any, len(s.fields))
			for i := range vals {
				vals[i] = Missing{}
			}
			n := NewNode(typ, vals...)
			if s.attrs {
				for _, a := range []string{"lineno", "col_offset", "end_lineno", "end_col_offset"} {
					n.SetAttr(a, Missing{})
				}
			}
			for _, part := range []string{"f", "a"} {
				for k, e := range x[part].(map[string]any) {
					n.SetAttr(k, decodePyValue(t, e))
				}
			}
			return n
		}
		for k, e := range x {
			switch k {
			case "int":
				b, _ := new(big.Int).SetString(e.(string), 10)
				if b.IsInt64() {
					return int(b.Int64())
				}
				return b
			case "pow10":
				return new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(e.(float64))), nil)
			case "float":
				f, err := strconv.ParseFloat(e.(string), 64)
				if err != nil {
					t.Fatal(err)
				}
				return f
			case "bytes":
				b, _ := hex.DecodeString(e.(string))
				return b
			case "complex":
				p := e.([]any)
				re, _ := strconv.ParseFloat(p[0].(string), 64)
				im, _ := strconv.ParseFloat(p[1].(string), 64)
				return Complex{re, im}
			case "ellipsis":
				return Ellipsis{}
			case "tuple", "frozenset":
				items := e.([]any)
				out := make([]any, len(items))
				for i, it := range items {
					out[i] = decodePyValue(t, it)
				}
				if k == "tuple" {
					return pyrepr.Tuple(out)
				}
				return FrozenSet(out)
			case "dict":
				m := map[any]any{}
				for _, kv := range e.([]any) {
					p := kv.([]any)
					m[decodePyValue(t, p[0])] = decodePyValue(t, p[1])
				}
				return m
			}
		}
	}
	t.Fatalf("undecodable value %v", v)
	return nil
}

// Runs ParseNode and reports a recovered panic as the crash CPython has there
func parseNodeOutcome(n *Node, o Options) (out *Node, err error, crash string) {
	defer func() {
		if r := recover(); r != nil {
			crash = fmt.Sprint(r)
		}
	}()
	out, err = ParseNode(n, o)
	return out, err, ""
}

func TestParseNodeMatchesCPython(t *testing.T) {
	var rows []nodeRow
	if err := json.Unmarshal(readGzip(t, "node_rows.json.gz"), &rows); err != nil {
		t.Fatal(err)
	}
	if len(rows) < 200 {
		t.Fatalf("only %d rows", len(rows))
	}
	for _, r := range rows {
		label := fmt.Sprintf("%s/%s/opt%d", r.Name, r.Mode, r.Opt)
		tree := decodePyValue(t, r.Tree).(*Node)
		before, _ := objRepr(tree)
		got, err, crash := parseNodeOutcome(tree, Options{Mode: r.Mode, Optimize: r.Opt, Filename: r.Fn})
		if after, _ := objRepr(tree); after != before {
			t.Errorf("%s: argument changed\n%s\n%s", label, before, after)
		}
		switch {
		case r.Crash:
			if !strings.Contains(crash, "SIGSEGV") {
				t.Errorf("%s: want the CPython crash, got %v %v %q", label, got, err, crash)
			}
		case crash != "":
			t.Errorf("%s: panic %s", label, crash)
		case r.Out != nil:
			if err != nil {
				t.Errorf("%s: unexpected error %v", label, err)
				continue
			}
			if d, derr := Dump(got, true); derr != nil || d != *r.Out {
				t.Errorf("%s:\n got %s %v\nwant %s", label, d, derr, *r.Out)
			}
		default:
			want := fmt.Sprintf("%v: %v", r.Err[0], r.Err[1])
			var have string
			switch e := err.(type) {
			case *SyntaxError:
				have = fmt.Sprintf("%s: %s", e.Class, e.Msg)
				if e.Lineno != int(r.Err[2].(float64)) || e.Offset != int(r.Err[3].(float64)) || e.EndLineno != int(r.Err[4].(float64)) || e.EndOffset != int(r.Err[5].(float64)) || e.Filename != r.Fn {
					t.Errorf("%s: location %+v want %v", label, e, r.Err)
				}
			case *LimitError:
				have = e.Class + ": " + e.Msg
			case *IntDigitsError:
				have = "ValueError: " + e.Error()
			default:
				have = fmt.Sprintf("%T %v", err, err)
			}
			if have != want {
				t.Errorf("%s:\n got %s\nwant %s", label, have, want)
			}
		}
	}
}

func TestParseNodeRejectsNil(t *testing.T) {
	_, err := ParseNode(nil, Options{})
	if e, ok := err.(*LimitError); !ok || e.Class != "TypeError" || e.Msg != "compile() arg 1 must be a string, bytes or AST object" {
		t.Fatalf("got %v", err)
	}
	if _, err := ParseNode(NewNode("Module", []*Node{}, []*Node{}), Options{Optimize: 3}); err == nil || err.Error() != "compile(): invalid optimize value" {
		t.Fatalf("got %v", err)
	}
}
