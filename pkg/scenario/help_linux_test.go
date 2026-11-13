package scenario

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// Commands whose help differs from hermes, each an open entry of SCENARIO_FINDINGS.md
var helpFindings = map[string]string{}

// Removes every whitespace character so wrapping and column alignment never count
func unspaced(text string) string {
	return strings.Join(strings.Fields(text), "")
}

// Returns the text around the first position where two strings part
func firstDifference(got, want string) (string, string) {
	i := 0
	for i < len(got) && i < len(want) && got[i] == want[i] {
		i++
	}
	from := max(i-60, 0)
	return got[from:min(i+60, len(got))], want[from:min(i+60, len(want))]
}

// The help of every top level command exits 0 and carries the content hermes prints for it
func TestScenarioHelpOfEveryCommand(t *testing.T) {
	s := Start(t, Setup{Env: map[string]string{"COLUMNS": "100"}})
	fixtures, err := filepath.Glob(filepath.Join("testdata", "help", "*.txt"))
	if err != nil || len(fixtures) == 0 {
		t.Fatalf("no help fixtures under testdata/help: %v", err)
	}
	sort.Strings(fixtures)
	var problems []string
	for _, fixture := range fixtures {
		name := strings.TrimSuffix(filepath.Base(fixture), ".txt")
		args := []string{"--help"}
		if name != "icebath" {
			args = append(strings.Split(name, "_"), "--help")
		}
		raw, err := os.ReadFile(fixture)
		if err != nil {
			t.Fatal(err)
		}
		r := s.Run(args...)
		got, want := unspaced(r.Stdout), unspaced(s.Expand(string(raw)))
		finding, open := helpFindings[name]
		switch {
		case r.Code == 0 && got == want && open:
			problems = append(problems, name+": matches hermes again, remove it from helpFindings and close "+finding)
		case r.Code == 0 && got == want:
		case open:
			t.Logf("OPEN FINDING %s still reproduces: help of %s differs from hermes", finding, name)
		case r.Code != 0:
			problems = append(problems, name+": exit "+r.dump())
		default:
			g, w := firstDifference(got, want)
			problems = append(problems, name+": help content differs from hermes\n  got  ..."+g+"...\n  want ..."+w+"...")
		}
	}
	if len(problems) > 0 {
		s.Failf("%d of %d commands\n%s", len(problems), len(fixtures), strings.Join(problems, "\n"))
	}
}
