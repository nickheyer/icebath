package pyast

import (
	"encoding/hex"
	"math/big"
	"strconv"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

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

func TestParseNodeRejectsNil(t *testing.T) {
	_, err := ParseNode(nil, Options{})
	if e, ok := err.(*LimitError); !ok || e.Class != "TypeError" || e.Msg != "compile() arg 1 must be a string, bytes or AST object" {
		t.Fatalf("got %v", err)
	}
	if _, err := ParseNode(NewNode("Module", []*Node{}, []*Node{}), Options{Optimize: 3}); err == nil || err.Error() != "compile(): invalid optimize value" {
		t.Fatalf("got %v", err)
	}
}
