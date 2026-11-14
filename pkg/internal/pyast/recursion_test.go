package pyast

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// Rows of the frozen script file invocation, the largest chains that parse and one more that raises
func TestParseRecursionErrorMatchesFrozenCPython(t *testing.T) {
	compile := "Stack overflow (used 8152 kB) during compilation"
	ast2obj := "Stack overflow (used 8152 kB)during  ast construction"
	rows := []struct {
		src  string
		want string
	}{
		{"a" + strings.Repeat(" + a", 52153), ""},
		{"a" + strings.Repeat(" + a", 52154), compile},
		{"a" + strings.Repeat(".x", 130384), ""},
		{"a" + strings.Repeat(".x", 130385), ast2obj},
	}
	for i, r := range rows {
		_, err := Parse(r.src)
		got := ""
		if err != nil {
			le, ok := err.(*LimitError)
			if !ok || le.Class != "RecursionError" {
				t.Fatalf("row %d got %T %v", i, err, err)
			}
			got = le.Msg
		}
		if got != r.want {
			t.Errorf("row %d got %q want %q", i, got, r.want)
		}
	}
}

type recursionRow struct {
	Name  string   `json:"name"`
	Mode  string   `json:"mode"`
	Tmpl  string   `json:"tmpl"`
	Chain string   `json:"chain"`
	N     int      `json:"n"`
	Err   []string `json:"err"`
}

// Builds the family source whose marked chain repeats its unit n times
func (r recursionRow) source(n int) string {
	var chain string
	switch r.Chain {
	case "C":
		chain = "a" + strings.Repeat(" + a", n)
	case "A":
		chain = "a" + strings.Repeat(".x", n)
	case "S":
		chain = "a" + strings.Repeat("[b]", n)
	case "K":
		chain = "a" + strings.Repeat("()", n)
	case "P":
		chain = strings.Repeat("[", n) + "b" + strings.Repeat("]", n)
	case "G":
		chain = "(b for b in c)" + strings.Repeat(" + a", n)
	case "T":
		chain = "tuple(b for b in c)" + strings.Repeat(" + a", n)
	default:
		panic("unknown chain " + r.Chain)
	}
	return strings.Replace(r.Tmpl, "\x01"+r.Chain+"\x01", chain, 1)
}

func errorClass(err error) string {
	switch e := err.(type) {
	case *LimitError:
		return e.Class
	case *SyntaxError:
		return "SyntaxError"
	}
	return fmt.Sprintf("%T", err)
}

// Families recorded under a script file run reproducing the frozen chains, each parses at n and raises at n plus one
func TestParseRecursionRowsMatchCPython(t *testing.T) {
	var rows []recursionRow
	if err := json.Unmarshal(readGzip(t, "recursion_rows.json.gz"), &rows); err != nil {
		t.Fatal(err)
	}
	if len(rows) != 144 {
		t.Fatalf("got %d rows", len(rows))
	}
	for _, r := range rows {
		t.Run(r.Name, func(t *testing.T) {
			t.Parallel()
			o := Options{Mode: r.Mode}
			if _, _, err := ParseWith(r.source(r.N), o); err != nil {
				t.Fatalf("n=%d got %s %v", r.N, errorClass(err), err)
			}
			_, _, err := ParseWith(r.source(r.N+1), o)
			if err == nil {
				t.Fatalf("n=%d parsed, want %s %q", r.N+1, r.Err[0], r.Err[1])
			}
			if got := []string{errorClass(err), err.Error()}; got[0] != r.Err[0] || got[1] != r.Err[1] {
				t.Errorf("n=%d got %q want %q", r.N+1, got, r.Err)
			}
		})
	}
}
