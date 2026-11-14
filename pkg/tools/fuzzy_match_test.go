package tools

import (
	"encoding/base64"
	"encoding/json"
	"math"
	"strings"
	"sync"
	"testing"
)

// Golden cases captured from the hermes modules this port follows
type fileEditGolden struct {
	Fuzzy []struct {
		Name       string   `json:"name"`
		Content    string   `json:"content"`
		Old        string   `json:"old"`
		New        string   `json:"new"`
		ReplaceAll bool     `json:"replace_all"`
		Result     string   `json:"result"`
		Count      int      `json:"count"`
		Strategy   string   `json:"strategy"`
		Error      string   `json:"error"`
		Notes      []string `json:"notes"`
	} `json:"fuzzy"`
	Strategies []struct {
		Name    string  `json:"name"`
		Content string  `json:"content"`
		Pattern string  `json:"pattern"`
		Spans   [][]int `json:"spans"`
	} `json:"strategies"`
	Matcher []struct {
		A       string  `json:"a"`
		B       string  `json:"b"`
		Ratio   float64 `json:"ratio"`
		Blocks  [][]int `json:"blocks"`
		Opcodes [][]any `json:"opcodes"`
	} `json:"matcher"`
	Closest []struct {
		Old     string `json:"old"`
		Content string `json:"content"`
		Result  string `json:"result"`
	} `json:"closest"`
	Hint []struct {
		Error   string `json:"error"`
		Count   int    `json:"count"`
		Old     string `json:"old"`
		Content string `json:"content"`
		Result  string `json:"result"`
	} `json:"hint"`
	Drift []struct {
		Old     string `json:"old"`
		New     string `json:"new"`
		Content string `json:"content"`
		ROld    string `json:"rold"`
		RNew    string `json:"rnew"`
		Note    string `json:"note"`
		Error   string `json:"error"`
	} `json:"drift"`
	Applied []struct {
		Content string `json:"content"`
		Old     string `json:"old"`
		New     string `json:"new"`
		Result  bool   `json:"result"`
	} `json:"applied"`
	Boundary []struct {
		Strategies []string `json:"strategies"`
		Result     string   `json:"result"`
	} `json:"boundary"`
	Diff []struct {
		Old     string `json:"old"`
		New     string `json:"new"`
		Deleted bool   `json:"deleted"`
		Result  string `json:"result"`
	} `json:"diff"`
	Patch []struct {
		Name       string            `json:"name"`
		Files      map[string]string `json:"files"`
		Patch      string            `json:"patch"`
		FailRead   []string          `json:"fail_read"`
		ParseError string            `json:"parse_error"`
		Ops        []struct {
			Kind    string `json:"kind"`
			Path    string `json:"path"`
			NewPath string `json:"new_path"`
			Hunks   []struct {
				Hint  string     `json:"hint"`
				Lines [][]string `json:"lines"`
			} `json:"hunks"`
		} `json:"ops"`
		Result map[string]any    `json:"result"`
		After  map[string]string `json:"after"`
	} `json:"patch"`
}

var goldenOnce struct {
	sync.Once
	g   fileEditGolden
	err error
}

func loadFileEditGolden(t *testing.T) *fileEditGolden {
	t.Helper()
	goldenOnce.Do(func() {
		raw, err := base64.StdEncoding.DecodeString(fileEditGoldenB64)
		if err != nil {
			goldenOnce.err = err
			return
		}
		goldenOnce.err = json.Unmarshal(raw, &goldenOnce.g)
	})
	if goldenOnce.err != nil {
		t.Fatalf("golden data: %v", goldenOnce.err)
	}
	return &goldenOnce.g
}

func TestFuzzyFindAndReplaceGolden(t *testing.T) {
	g := loadFileEditGolden(t)
	if len(g.Fuzzy) == 0 {
		t.Fatal("no fuzzy cases")
	}
	for _, c := range g.Fuzzy {
		var notes []string
		got, count, strategy, errText := fuzzyFindAndReplace(c.Content, c.Old, c.New, c.ReplaceAll, &notes, nil)
		if got != c.Result || count != c.Count || strategy != c.Strategy || errText != c.Error {
			t.Errorf("%s:\n got  (%q, %d, %q, %q)\n want (%q, %d, %q, %q)", c.Name, got, count, strategy, errText, c.Result, c.Count, c.Strategy, c.Error)
		}
		if strings.Join(notes, "|") != strings.Join(c.Notes, "|") {
			t.Errorf("%s notes: got %q want %q", c.Name, notes, c.Notes)
		}
	}
}

func TestFuzzyEveryStrategyGolden(t *testing.T) {
	g := loadFileEditGolden(t)
	byName := map[string]func(string, string) []matchSpan{}
	for _, s := range fuzzyStrategies {
		byName[s.name] = s.fn
	}
	if len(byName) != 9 {
		t.Fatalf("expected 9 strategies, got %d", len(byName))
	}
	seen := map[string]bool{}
	for _, c := range g.Strategies {
		fn := byName[c.Name]
		if fn == nil {
			t.Fatalf("missing strategy %s", c.Name)
		}
		seen[c.Name] = true
		got := fn(c.Content, c.Pattern)
		gotRunes := make([][]int, len(got))
		for i, s := range got {
			gotRunes[i] = []int{len([]rune(c.Content[:s.start])), len([]rune(c.Content[:s.end]))}
		}
		if len(gotRunes) != len(c.Spans) {
			t.Errorf("%s(%q, %q): got %v want %v", c.Name, c.Content, c.Pattern, gotRunes, c.Spans)
			continue
		}
		for i := range gotRunes {
			if gotRunes[i][0] != c.Spans[i][0] || gotRunes[i][1] != c.Spans[i][1] {
				t.Errorf("%s(%q, %q): got %v want %v", c.Name, c.Content, c.Pattern, gotRunes, c.Spans)
				break
			}
		}
	}
	if len(seen) != 9 {
		t.Fatalf("golden covered %d strategies", len(seen))
	}
}

func TestSequenceMatcherGolden(t *testing.T) {
	g := loadFileEditGolden(t)
	for _, c := range g.Matcher {
		m := newStringMatcher(c.A, c.B)
		if r := m.ratio(); math.Abs(r-c.Ratio) > 1e-12 {
			t.Errorf("ratio(%q, %q) = %v want %v", c.A, c.B, r, c.Ratio)
		}
		blocks := m.getMatchingBlocks()
		if len(blocks) != len(c.Blocks) {
			t.Errorf("blocks(%q, %q) = %v want %v", c.A, c.B, blocks, c.Blocks)
		} else {
			for i, b := range blocks {
				w := c.Blocks[i]
				if b.i != w[0] || b.j != w[1] || b.size != w[2] {
					t.Errorf("blocks(%q, %q)[%d] = %v want %v", c.A, c.B, i, b, w)
				}
			}
		}
		ops := m.getOpcodes()
		if len(ops) != len(c.Opcodes) {
			t.Errorf("opcodes(%q, %q) = %v want %v", c.A, c.B, ops, c.Opcodes)
			continue
		}
		for i, op := range ops {
			w := c.Opcodes[i]
			if op.tag != w[0].(string) || op.i1 != int(w[1].(float64)) || op.i2 != int(w[2].(float64)) || op.j1 != int(w[3].(float64)) || op.j2 != int(w[4].(float64)) {
				t.Errorf("opcodes(%q, %q)[%d] = %v want %v", c.A, c.B, i, op, w)
			}
		}
	}
}

func TestSequenceMatcherAutojunk(t *testing.T) {
	b := strings.Repeat("ab", 150)
	m := newSequenceMatcher[rune](nil, []rune("a"+b), []rune(b), true)
	if _, ok := m.b2j['a']; ok {
		t.Fatal("popular element must be purged from b2j once b has 200 or more elements")
	}
	off := newSequenceMatcher[rune](nil, []rune("a"+b), []rune(b), false)
	if _, ok := off.b2j['a']; !ok {
		t.Fatal("autojunk false keeps popular elements")
	}
	if off.ratio() <= m.ratio() {
		t.Fatalf("autojunk must lower the ratio here, got %v and %v", off.ratio(), m.ratio())
	}
	junk := newSequenceMatcher[rune](func(r rune) bool { return r == ' ' }, []rune("a b"), []rune("a  b"), true)
	if !junk.bjunk[' '] {
		t.Fatal("isjunk elements land in bjunk")
	}
}

func TestFuzzyFindClosestLinesGolden(t *testing.T) {
	g := loadFileEditGolden(t)
	for _, c := range g.Closest {
		if got := findClosestLines(c.Old, c.Content, 2, 3); got != c.Result {
			t.Errorf("findClosestLines(%q, %q):\n got  %q\n want %q", c.Old, c.Content, got, c.Result)
		}
	}
}

func TestFuzzyNoMatchHintGolden(t *testing.T) {
	g := loadFileEditGolden(t)
	for _, c := range g.Hint {
		if got := formatNoMatchHint(c.Error, c.Count, c.Old, c.Content); got != c.Result {
			t.Errorf("formatNoMatchHint(%q):\n got  %q\n want %q", c.Error, got, c.Result)
		}
	}
	if !strings.HasPrefix(g.Hint[0].Result, "\n\nDid you mean one of these sections?\n") {
		t.Fatalf("first hint case must produce a hint, got %q", g.Hint[0].Result)
	}
}

func TestFuzzyAlreadyAppliedAndBoundaryGolden(t *testing.T) {
	g := loadFileEditGolden(t)
	for _, c := range g.Applied {
		if got := isAlreadyApplied(c.Content, c.Old, c.New); got != c.Result {
			t.Errorf("isAlreadyApplied(%q, %q, %q) = %v want %v", c.Content, c.Old, c.New, got, c.Result)
		}
	}
	for _, c := range g.Boundary {
		if got := boundaryNote(c.Strategies, "the hunk's", " in a.py"); got != c.Result {
			t.Errorf("boundaryNote(%v):\n got  %q\n want %q", c.Strategies, got, c.Result)
		}
	}
}

func TestFuzzyUnifiedDiffGolden(t *testing.T) {
	g := loadFileEditGolden(t)
	for _, c := range g.Diff {
		var next *string
		if !c.Deleted {
			next = &c.New
		}
		if got := v4aUnifiedDiff("f.txt", c.Old, next); got != c.Result {
			t.Errorf("diff(%q, %q):\n got  %q\n want %q", c.Old, c.New, got, c.Result)
		}
	}
}

func TestFuzzyUnicodePreservesTypography(t *testing.T) {
	content := "say \u201chi\u201d \u2014 done\n"
	got, n, strategy, errText := fuzzyFindAndReplace(content, `say "hi" -- done`, `say "bye" -- done`, false, nil, nil)
	if errText != "" || n != 1 || strategy != "unicode_normalized" {
		t.Fatalf("unexpected result %q %d %q %q", got, n, strategy, errText)
	}
	if got != "say \u201cbye\u201d \u2014 done\n" {
		t.Fatalf("typography lost: %q", got)
	}
}

func TestFuzzyInvalidUTF8OutsideSpanSurvives(t *testing.T) {
	content := "head \xff\xfe\nvalue = 1\n"
	got, n, _, errText := fuzzyFindAndReplace(content, "value = 1", "value = 2", false, nil, nil)
	if errText != "" || n != 1 || got != "head \xff\xfe\nvalue = 2\n" {
		t.Fatalf("bytes outside the span changed: %q %d %q", got, n, errText)
	}
}
