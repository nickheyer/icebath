package skills

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

const lintClean = "---\nname: my-skill\ndescription: Search arXiv papers by keyword, author, or ID.\nversion: 1.0.0\nauthor: Icebath Agent\nlicense: MIT\nmetadata:\n  icebath:\n    tags: [arxiv, research]\n    related_skills: []\n---\n\n# My Skill\n\n## Overview\nDoes a thing.\n\n## When to Use\n- When the user wants X.\n\n## Procedure\n1. Use `read_file` to load it.\n"

func lintRules(fs []LintFinding) map[string]bool {
	out := map[string]bool{}
	for _, f := range fs {
		out[f.Rule] = true
	}
	return out
}

func lintDir(t *testing.T, files map[string]string) string {
	t.Helper()
	d := filepath.Join(t.TempDir(), "my-skill")
	os.MkdirAll(d, 0o755)
	for rel, body := range files {
		p := filepath.Join(d, filepath.FromSlash(rel))
		os.MkdirAll(filepath.Dir(p), 0o755)
		os.WriteFile(p, []byte(body), 0o644)
	}
	return d
}

func TestLintContentRules(t *testing.T) {
	rep := func(old, new string) string { return strings.Replace(lintClean, old, new, 1) }
	cases := []struct {
		name    string
		content string
		dir     string
		rule    string
		present bool
	}{
		{"marketing", rep("Search arXiv papers by keyword, author, or ID.", "A powerful comprehensive tool."), "", "description-marketing", true},
		{"shell prose", rep("Use `read_file` to load it.", "Use `grep` to find it."), "", "shell-utility-reference", true},
		{"shell code block", lintClean + "\n```bash\ngrep -r foo .\n```\n", "", "shell-utility-reference", false},
		{"missing metadata", "---\nname: bare-skill\ndescription: Does a thing briefly.\n---\n\n# Bare Skill\n\n## When to Use\n- now\n", "", "missing-metadata", true},
		{"missing section", rep("## When to Use\n- When the user wants X.\n", ""), "", "missing-section", true},
		{"dangling reference", lintClean + "\nSee references/missing.md for detail.\n", lintDir(t, nil), "dangling-reference", true},
		{"present reference", lintClean + "\nSee references/detail.md for detail.\n", lintDir(t, map[string]string{"references/detail.md": "x"}), "dangling-reference", false},
		{"posix without platforms", lintClean, lintDir(t, map[string]string{"scripts/run.py": "import fcntl\nfcntl.flock(1, 2)\n"}), "platforms-gating", true},
		{"posix with platforms", rep("version: 1.0.0", "version: 1.0.0\nplatforms: [linux, macos]"), lintDir(t, map[string]string{"scripts/run.py": "import fcntl\n"}), "platforms-gating", false},
		{"forbidden file", lintClean, lintDir(t, map[string]string{"README.md": "# readme"}), "forbidden-file", true},
		{"platforms value", rep("version: 1.0.0", "version: 1.0.0\nplatforms: [linux, solaris]"), "", "platforms-value", true},
		{"author caps", rep("author: Icebath Agent", "author: icebath agent"), "", "author-caps", true},
		{"incident log", rep("1. Use `read_file` to load it.", "In #12345 the watcher died; #23456 was the same; see PR #34567 and issue #45678 for the fix."), "", "incident-log-shape", true},
		{"rule shape", rep("1. Use `read_file` to load it.", "Launch the watcher from a directory that outlives the watch; a deleted cwd reads as a stall."), "", "incident-log-shape", false},
	}
	for _, c := range cases {
		if got := lintRules(LintContent(c.content, c.dir))[c.rule]; got != c.present {
			t.Fatalf("%s: rule %s present=%v", c.name, c.rule, got)
		}
	}
}

func TestLintCleanSkillHasNoFindings(t *testing.T) {
	if fs := LintContent(lintClean, ""); len(fs) != 0 {
		t.Fatal(fs)
	}
	d := lintDir(t, map[string]string{"SKILL.md": lintClean})
	if fs, err := LintSkill(filepath.Join(d, "SKILL.md")); err != nil || len(fs) != 0 {
		t.Fatal(fs, err)
	}
}

func TestLintSeverities(t *testing.T) {
	long := LintContent(strings.Replace(lintClean, "Search arXiv papers by keyword, author, or ID.", strings.Repeat("x", 80), 1), "")
	if !lintRules(long)["description-length"] {
		t.Fatal(long)
	}
	for _, f := range long {
		if f.Severity != "warning" {
			t.Fatal(f)
		}
	}
	hasError := func(fs []LintFinding) bool {
		for _, f := range fs {
			if f.Severity == "error" {
				return true
			}
		}
		return false
	}
	bad := LintContent(strings.Replace(lintClean, "name: my-skill", "name: My_Skill!", 1), "")
	if !lintRules(bad)["name-format"] || !hasError(bad) {
		t.Fatal(bad)
	}
	actual := filepath.Join(t.TempDir(), "actual-dir")
	os.MkdirAll(actual, 0o755)
	mismatch := LintContent(lintClean, actual)
	if !lintRules(mismatch)["name-dir-mismatch"] || !hasError(mismatch) {
		t.Fatal(mismatch)
	}
}

func TestLintReferencesSprawlAboveCap(t *testing.T) {
	files := map[string]string{"SKILL.md": lintClean}
	for i := 0; i <= lintMaxReferenceFiles; i++ {
		files["references/note-"+strconv.Itoa(i)+".md"] = "x"
	}
	d := lintDir(t, files)
	if fs, _ := LintSkill(filepath.Join(d, "SKILL.md")); !lintRules(fs)["references-sprawl"] {
		t.Fatal(fs)
	}
	os.Remove(filepath.Join(d, "references", "note-"+strconv.Itoa(lintMaxReferenceFiles)+".md"))
	if fs, _ := LintSkill(filepath.Join(d, "SKILL.md")); lintRules(fs)["references-sprawl"] {
		t.Fatal(fs)
	}
}

func TestLintOversizedBodyAboveBudgetOnly(t *testing.T) {
	filler := "- Prefer the native tool; the shell path loses the structured result.\n"
	over := lintClean + strings.Repeat(filler, lintBodySoftBudgetChars/len(filler)+1)
	under := lintClean + strings.Repeat(filler, lintBodySoftBudgetChars/len(filler)/2)
	var found []LintFinding
	for _, f := range LintContent(over, "") {
		if f.Rule == "oversized-body" {
			found = append(found, f)
		}
	}
	if len(found) == 0 || found[0].Severity != "warning" {
		t.Fatal(found)
	}
	if lintRules(LintContent(under, ""))["oversized-body"] {
		t.Fatal("under budget flagged")
	}
}
