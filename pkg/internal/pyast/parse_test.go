package pyast

import (
	"bufio"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
)

type cpythonCase struct {
	Name  string  `json:"name"`
	Src   *string `json:"src"`
	Bytes string  `json:"bytes"`
	Dump  *string `json:"dump"`
	Err   []any   `json:"err"`
	// Parsed marks a tree too deep for the recursive ast.dump, so only parse success is recorded
	Parsed  bool     `json:"parsed"`
	DumpErr []string `json:"dumperr"`
	Unparse *string  `json:"unparse"`
}

func readGzip(t *testing.T, name string) []byte {
	t.Helper()
	f, err := os.Open(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	zr, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	sc := bufio.NewScanner(zr)
	sc.Buffer(make([]byte, 1<<20), 1<<26)
	for sc.Scan() {
		b.WriteString(sc.Text())
		b.WriteByte('\n')
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	return []byte(b.String())
}

// Error fields rendered as CPython's type, msg, lineno, offset, end_lineno and end_offset
func errorRow(err error) string {
	var se *SyntaxError
	if errors.As(err, &se) {
		row := []string{se.Class, se.Msg, "None", "None", "None", "None"}
		if se.HasLineno || se.HasText {
			row[2], row[3] = strconv.Itoa(se.Lineno), strconv.Itoa(se.Offset)
		}
		if se.HasEnd {
			row[4], row[5] = strconv.Itoa(se.EndLineno), strconv.Itoa(se.EndOffset)
		}
		return strings.Join(row, "|")
	}
	var le *LimitError
	if errors.As(err, &le) {
		return strings.Join([]string{le.Class, le.Msg, "None", "None", "None", "None"}, "|")
	}
	return "unexpected " + err.Error()
}

func expectedErrorRow(fields []any) string {
	row := make([]string, len(fields))
	for i, f := range fields {
		switch v := f.(type) {
		case nil:
			row[i] = "None"
		case float64:
			row[i] = strconv.Itoa(int(v))
		default:
			row[i] = fmt.Sprint(v)
		}
	}
	return strings.Join(row, "|")
}

// Source text ast.unparse produces for the parsed tree, or the parse error row
func unparseOutcome(src []byte, isBytes bool) string {
	parse := Parse
	if isBytes {
		parse = func(s string) (*Node, error) { return ParseBytes([]byte(s)) }
	}
	n, err := parse(string(src))
	if err != nil {
		return "err:" + errorRow(err)
	}
	out, err := Unparse(n)
	if err != nil {
		return "unparseerr:" + err.Error()
	}
	return out
}

func parseOutcome(src []byte, isBytes bool) string {
	var n *Node
	var err error
	if isBytes {
		n, err = ParseBytes(src)
	} else {
		n, err = Parse(string(src))
	}
	if err != nil {
		return "err:" + errorRow(err)
	}
	d, err := Dump(n, true)
	if err != nil {
		return "dumperr:ValueError|" + err.Error()
	}
	return "dump:" + d
}

func TestParseMatchesCPythonCases(t *testing.T) {
	var cases []cpythonCase
	if err := json.Unmarshal(readGzip(t, "cases.json.gz"), &cases); err != nil {
		t.Fatal(err)
	}
	failed := 0
	for _, c := range cases {
		var src []byte
		isBytes := c.Src == nil
		if isBytes {
			src, _ = hex.DecodeString(c.Bytes)
		} else {
			src = []byte(*c.Src)
		}
		want := "dump:"
		if c.Dump != nil {
			want += *c.Dump
		} else if c.DumpErr != nil {
			want = "dumperr:" + strings.Join(c.DumpErr, "|")
		} else if c.Parsed {
			want = "parsed"
		} else {
			want = "err:" + expectedErrorRow(c.Err)
		}
		got := parseOutcome(src, isBytes)
		if want == "parsed" && strings.HasPrefix(got, "dump:") {
			got = "parsed"
		}
		if c.Unparse != nil {
			if got := unparseOutcome(src, isBytes); got != *c.Unparse {
				failed++
				t.Errorf("%s unparse\n got %s\nwant %s", c.Name, firstRunes(got, 300), firstRunes(*c.Unparse, 300))
			}
		}
		if got != want {
			failed++
			if failed <= 40 {
				i := 0
				for i < len(got) && i < len(want) && got[i] == want[i] {
					i++
				}
				lo := max(0, i-150)
				t.Errorf("%s %q\n got %s\nwant %s", c.Name, firstRunes(string(src), 60), firstRunes(got[lo:], 300), firstRunes(want[lo:], 300))
			}
		}
	}
	if failed > 0 {
		t.Errorf("%d of %d cases differ from CPython", failed, len(cases))
	}
}

func firstRunes(s string, n int) string {
	r := []rune(s)
	if len(r) > n {
		return string(r[:n]) + "..."
	}
	return s
}

const hermesRoot = "../../../references/projects/hermes-agent"

func TestParseMatchesCPythonOnHermesTree(t *testing.T) {
	lines := strings.Split(strings.TrimSpace(string(readGzip(t, "hermes_tree.tsv.gz"))), "\n")
	type result struct{ rel, msg string }
	jobs := make(chan string)
	results := make(chan result)
	var wg sync.WaitGroup
	for w := 0; w < 6; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for line := range jobs {
				parts := strings.SplitN(line, "\t", 5)
				rel, srcHash, kind, want := parts[0], parts[1], parts[2], parts[3]
				data, err := os.ReadFile(filepath.Join(hermesRoot, rel))
				if err != nil {
					results <- result{rel, err.Error()}
					continue
				}
				if sum := sha256.Sum256(data); hex.EncodeToString(sum[:]) != srcHash {
					results <- result{rel, "source changed since the CPython reference was recorded"}
					continue
				}
				got := parseOutcome(data, true)
				if kind == "ok" {
					sum := sha256.Sum256([]byte(strings.TrimPrefix(got, "dump:")))
					if !strings.HasPrefix(got, "dump:") || hex.EncodeToString(sum[:]) != want {
						results <- result{rel, firstRunes(got, 200)}
						continue
					}
					if parts[4] != "-" {
						up := sha256.Sum256([]byte(unparseOutcome(data, true)))
						if hex.EncodeToString(up[:]) != parts[4] {
							results <- result{rel, "unparse differs"}
						}
					}
					continue
				}
				var fields []any
				_ = json.Unmarshal([]byte(want), &fields)
				if got != "err:"+expectedErrorRow(fields) {
					results <- result{rel, firstRunes(got, 200) + " want " + want}
				}
			}
		}()
	}
	go func() {
		for _, l := range lines {
			jobs <- l
		}
		close(jobs)
		wg.Wait()
		close(results)
	}()
	failed := 0
	for r := range results {
		failed++
		if failed <= 25 {
			t.Errorf("%s: %s", r.rel, r.msg)
		}
	}
	if failed > 0 {
		t.Errorf("%d of %d hermes files differ from CPython", failed, len(lines))
	}
}
