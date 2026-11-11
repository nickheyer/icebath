package pyast

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
)

type compileCase struct {
	Name string  `json:"name"`
	Src  *string `json:"src"`
	Hex  string  `json:"hex"`
	Out  any     `json:"out"`
	FOut any     `json:"fout"`
}

// Outcome of a compile rendered as class, msg, lineno, offset, end_lineno, end_offset and text, or ok
func compileRow(err error) string {
	if err == nil {
		return "ok"
	}
	var se *SyntaxError
	if errors.As(err, &se) {
		text := "None"
		if se.HasText {
			text = se.Text
		}
		return errorRow(err) + "|" + text
	}
	var le *LimitError
	if errors.As(err, &le) {
		return errorRow(err) + "|None"
	}
	return "unexpected " + err.Error()
}

func wantCompileRow(v any) string {
	fields, ok := v.([]any)
	if !ok {
		return v.(string)
	}
	text := "None"
	if t, ok := fields[6].(string); ok {
		text = strings.TrimSuffix(t, "\n")
	}
	return expectedErrorRow(fields[:6]) + "|" + text
}

func (c compileCase) compile(filename string) error {
	if c.Src != nil {
		return CompileString(*c.Src, filename)
	}
	src, _ := hex.DecodeString(c.Hex)
	return Compile(src, filename)
}

func (c compileCase) bytes() []byte {
	if c.Src != nil {
		return []byte(*c.Src)
	}
	src, _ := hex.DecodeString(c.Hex)
	return src
}

// Every recorded source compiles to the outcome of compile(source, name, "exec") under CPython 3.14.8, with and without its file on disk
func TestCompileMatchesCPythonCases(t *testing.T) {
	var cases []compileCase
	if err := json.Unmarshal(readGzip(t, "compile_cases.json.gz"), &cases); err != nil {
		t.Fatal(err)
	}
	if len(cases) < 2500 {
		t.Fatalf("only %d recorded rows", len(cases))
	}
	srcfile := filepath.Join(t.TempDir(), "srcfile.py")
	for _, c := range cases {
		t.Run(c.Name, func(t *testing.T) {
			want := wantCompileRow(c.Out)
			if got := compileRow(c.compile("<corpus>")); got != want {
				t.Errorf("%q\n got %s\nwant %s", firstRunes(string(c.bytes()), 200), firstRunes(got, 300), firstRunes(want, 300))
			}
			if c.FOut != nil {
				want = wantCompileRow(c.FOut)
			}
			if err := os.WriteFile(srcfile, c.bytes(), 0o600); err != nil {
				t.Fatal(err)
			}
			if got := compileRow(c.compile(srcfile)); got != want {
				t.Errorf("with its file %q\n got %s\nwant %s", firstRunes(string(c.bytes()), 200), firstRunes(got, 300), firstRunes(want, 300))
			}
		})
	}
}

func TestCompileMatchesCPythonOnHermesTree(t *testing.T) {
	lines := strings.Split(strings.TrimSpace(string(readGzip(t, "compile_hermes_tree.tsv.gz"))), "\n")
	if len(lines) < 10000 {
		t.Fatalf("only %d recorded rows", len(lines))
	}
	var mu sync.Mutex
	var wg sync.WaitGroup
	var bad []string
	jobs := make(chan string)
	for w := 0; w < 6; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for line := range jobs {
				parts := strings.SplitN(line, "\t", 3)
				rel, srcHash := parts[0], parts[1]
				var want any
				_ = json.Unmarshal([]byte(parts[2]), &want)
				path := filepath.Join(hermesRoot, rel)
				msg := ""
				data, err := os.ReadFile(path)
				if err != nil {
					msg = err.Error()
				} else if sum := sha256.Sum256(data); hex.EncodeToString(sum[:]) != srcHash {
					msg = "source changed since the CPython reference was recorded"
				} else if got := compileRow(Compile(data, path)); got != wantCompileRow(want) {
					msg = firstRunes(got, 300) + " want " + firstRunes(wantCompileRow(want), 300)
				}
				if msg != "" {
					mu.Lock()
					bad = append(bad, rel+": "+msg)
					mu.Unlock()
				}
			}
		}()
	}
	for _, l := range lines {
		jobs <- l
	}
	close(jobs)
	wg.Wait()
	for i, b := range bad {
		if i < 25 {
			t.Error(b)
		}
	}
	if len(bad) > 0 {
		t.Errorf("%d of %d hermes files differ from CPython", len(bad), len(lines))
	}
}

// Every recursion check of the symbol table and code generator passes sits at the stack distance gdb recorded under CPython 3.14.8
func TestCompileStackDistancesMatchCPython(t *testing.T) {
	var rows []struct {
		Src   string `json:"src"`
		Trace string `json:"trace"`
	}
	if err := json.Unmarshal(readGzip(t, "compile_stack_trace.json.gz"), &rows); err != nil {
		t.Fatal(err)
	}
	if len(rows) < 1800 {
		t.Fatalf("only %d recorded rows", len(rows))
	}
	failed := 0
	for i, r := range rows {
		var got []string
		seen := func(tag byte, d int) { got = append(got, string(tag)+strconv.Itoa(d)) }
		_, n, err := parseLexed(translateLines([]byte(r.Src), true), Options{Filename: "<trace>", encoding: "utf-8"}, latestMinor)
		if err != nil {
			t.Errorf("row %d %q does not parse: %v", i, firstRunes(r.Src, 80), err)
			continue
		}
		_ = compileStages(n, "<trace>", seen)
		want := strings.Fields(r.Trace)
		at := 0
		for at < len(got) && at < len(want) && got[at] == want[at] {
			at++
		}
		if at == len(got) && at == len(want) {
			continue
		}
		failed++
		if failed <= 25 {
			lo := max(0, at-6)
			t.Errorf("row %d %q differs at check %d\n got %v\nwant %v", i, firstRunes(r.Src, 300), at, got[lo:min(len(got), at+6)], want[lo:min(len(want), at+6)])
		}
	}
	if failed > 0 {
		t.Errorf("%d of %d sources check the stack at other distances than CPython", failed, len(rows))
	}
}

// Outcome of a compile as the first six fields of a recorded row, or ok
func depthRow(err error) string {
	if err == nil {
		return "ok"
	}
	return errorRow(err)
}

func wantDepthRow(v any) string {
	if fields, ok := v.([]any); ok {
		return expectedErrorRow(fields)
	}
	return v.(string)
}

type compileDepthCorpus struct {
	Chains []struct {
		Tmpl  string `json:"tmpl"`
		Chain string `json:"chain"`
		N     *int   `json:"n"`
		Err   any    `json:"err"`
	} `json:"chains"`
	Limits []struct {
		Ctx  string `json:"ctx"`
		Pre  string `json:"pre"`
		Leaf string `json:"leaf"`
		N    int    `json:"n"`
		At   any    `json:"at"`
		Over any    `json:"over"`
	} `json:"limits"`
}

func loadCompileDepth(t *testing.T) compileDepthCorpus {
	var corpus compileDepthCorpus
	if err := json.Unmarshal(readGzip(t, "compile_depth.json.gz"), &corpus); err != nil {
		t.Fatal(err)
	}
	return corpus
}

// Each chain family compiles at the longest chain the frozen reference invocation of CPython compiles and fails one past it the same way
func TestCompileRecursionLimitsMatchFrozenCPython(t *testing.T) {
	corpus := loadCompileDepth(t)
	if len(corpus.Chains) < 280 {
		t.Fatalf("only %d recorded families", len(corpus.Chains))
	}
	for i, r := range corpus.Chains {
		t.Run(strconv.Itoa(i), func(t *testing.T) {
			t.Parallel()
			source := func(n int) string {
				return recursionRow{Tmpl: "\x01" + r.Chain + "\x01", Chain: r.Chain}.sourceIn(r.Tmpl, n)
			}
			n := 1
			if r.N != nil {
				n = *r.N
				if got := depthRow(CompileString(source(n), "<ref>")); got != "ok" {
					t.Fatalf("%q %s n=%d got %s, want ok", r.Tmpl, r.Chain, n, got)
				}
				n++
			}
			if got, want := depthRow(CompileString(source(n), "<ref>")), wantDepthRow(r.Err); got != want {
				t.Errorf("%q %s n=%d\n got %s\nwant %s", r.Tmpl, r.Chain, n, got, want)
			}
		})
	}
}

// Builds the chain of n units and places it at the marker of a template
func (r recursionRow) sourceIn(tmpl string, n int) string {
	return strings.Replace(tmpl, "\x01", r.source(n), 1)
}

// Nesting N and N plus one of each recorded context compile to the outcomes of the frozen reference invocation of CPython
func TestCompileDepthLimitsMatchFrozenCPython(t *testing.T) {
	corpus := loadCompileDepth(t)
	if len(corpus.Limits) < 2000 {
		t.Fatalf("only %d recorded rows", len(corpus.Limits))
	}
	var mu sync.Mutex
	var wg sync.WaitGroup
	var bad []string
	next := make(chan int)
	for w := 0; w < 6; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range next {
				r := corpus.Limits[i]
				at := depthRow(CompileString(strings.ReplaceAll(r.Ctx, "§", strings.Repeat(r.Pre, r.N)+r.Leaf), "<ref>"))
				over := depthRow(CompileString(strings.ReplaceAll(r.Ctx, "§", strings.Repeat(r.Pre, r.N+1)+r.Leaf), "<ref>"))
				if wantAt, wantOver := wantDepthRow(r.At), wantDepthRow(r.Over); at != wantAt || over != wantOver {
					mu.Lock()
					bad = append(bad, firstRunes(strconv.Quote(r.Ctx)+" "+strconv.Quote(r.Pre)+" "+strconv.Quote(r.Leaf)+" n="+strconv.Itoa(r.N)+": got "+firstRunes(at, 150)+" and "+firstRunes(over, 150)+", want "+firstRunes(wantAt, 150)+" and "+firstRunes(wantOver, 150), 700))
					mu.Unlock()
				}
			}
		}()
	}
	for i := range corpus.Limits {
		next <- i
	}
	close(next)
	wg.Wait()
	for i, b := range bad {
		if i < 25 {
			t.Error(b)
		}
	}
	if len(bad) > 0 {
		t.Errorf("%d of %d rows differ from CPython", len(bad), len(corpus.Limits))
	}
}
