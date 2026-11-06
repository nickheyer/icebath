package builtin

import (
	"bytes"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/dlclark/regexp2/v2"
	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/internal/pyre"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/skills"
)

// Compiles a Python re pattern or fails the test
func pyPattern(t *testing.T, pattern string) *regexp2.Regexp {
	t.Helper()
	re, err := pyre.Compile(pattern)
	if err != nil {
		t.Fatal(err)
	}
	return re
}

// Groups of every non overlapping match like re.finditer
func pyFindAll(t *testing.T, re *regexp2.Regexp, s string) [][]string {
	t.Helper()
	var out [][]string
	m, err := re.FindStringMatch(s)
	for ; m != nil && err == nil; m, err = re.FindNextMatch(m) {
		var groups []string
		for _, g := range m.Groups() {
			groups = append(groups, g.String())
		}
		out = append(out, groups)
	}
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// Result of re.sub with a replacement computed from each match
func pySub(t *testing.T, re *regexp2.Regexp, s string, repl func(string) string) string {
	t.Helper()
	out, err := re.ReplaceFunc(s, func(m regexp2.Match) string { return repl(m.String()) }, -1, -1)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// Markdown files below a root skipping dot directories and vendored node_modules like the EGM rglob
func egmMarkdownFiles(t *testing.T, root string) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(p, ".md") {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		for _, part := range strings.Split(filepath.ToSlash(rel), "/") {
			if strings.HasPrefix(part, ".") || part == "node_modules" {
				return nil
			}
		}
		out = append(out, p)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// Slug anchors of every Markdown heading with one hyphen per space as GitHub renders them
func extractHeaders(t *testing.T, content string) map[string]bool {
	slugs := map[string]bool{}
	strip := pyPattern(t, `[^a-z0-9 -]`)
	for _, m := range pyFindAll(t, pyPattern(t, `(?m)^(#{1,6})\s+(.+)$`), content) {
		slug := pySub(t, strip, pystr.Lower(pystr.Strip(m[2])), func(string) string { return "" })
		slugs[strings.ReplaceAll(slug, " ", "-")] = true
	}
	return slugs
}

func TestEGMHeadingSlugKeepsOneHyphenPerSpace(t *testing.T) {
	got := extractHeaders(t, "## Contribution Tracks & Workflow\n## Design Principles\n")
	if len(got) != 2 || !got["contribution-tracks--workflow"] || !got["design-principles"] {
		t.Fatalf("slugs %v", got)
	}
}

// Fails on every relative Markdown link below root that does not resolve inside root
func checkEGMLinks(t *testing.T, root string) int {
	link := pyPattern(t, `\[([^\]]+)\]\(([^\)]+)\)`)
	fence, tick := pyPattern(t, "```[\\s\\S]*?```"), pyPattern(t, "`[^`]*`")
	checked := 0
	for _, md := range egmMarkdownFiles(t, root) {
		raw, err := os.ReadFile(md)
		if err != nil {
			t.Fatal(err)
		}
		clean := pySub(t, fence, strings.ToValidUTF8(string(raw), ""), func(string) string { return "" })
		clean = pySub(t, tick, clean, func(string) string { return "" })
		headers := extractHeaders(t, clean)
		rel, _ := filepath.Rel(root, md)
		for _, m := range pyFindAll(t, link, clean) {
			target := pystr.Strip(m[2])
			if strings.HasPrefix(target, "http://") || strings.HasPrefix(target, "https://") || strings.HasPrefix(target, "mailto:") || strings.HasPrefix(target, "file://") {
				continue
			}
			if strings.HasSuffix(target, "_url") || strings.HasSuffix(target, "_path") || strings.HasSuffix(target, "_link") || strings.ContainsAny(target, "{<") {
				continue
			}
			checked++
			if strings.HasPrefix(target, "#") {
				if anchor := pystr.Lower(target[1:]); !headers[anchor] && len(headers) > 0 {
					t.Errorf("Broken intra-document anchor '%s' in %s", target, rel)
				}
				continue
			}
			fileTarget := strings.SplitN(target, "#", 2)[0]
			if fileTarget == "" {
				continue
			}
			resolved := fileTarget
			if !filepath.IsAbs(resolved) {
				resolved = filepath.Join(filepath.Dir(md), filepath.FromSlash(fileTarget))
			}
			if _, err := os.Stat(resolved); err != nil {
				if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(fileTarget))); err == nil {
					resolved = filepath.Join(root, filepath.FromSlash(fileTarget))
				}
			}
			if _, err := os.Stat(resolved); err != nil {
				t.Errorf("Broken relative link '[%s](%s)' in %s: resolved path '%s' does not exist on disk.", m[1], target, rel, resolved)
			}
		}
	}
	return checked
}

func TestEGMRelativeMarkdownLinksResolveInMaterializedTree(t *testing.T) {
	tree, err := materializeAt(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, root := range []string{tree.Payload(egmRoot), tree.Host("egm")} {
		if checkEGMLinks(t, root) < 100 {
			t.Errorf("%s: too few links checked", root)
		}
	}
}

func TestEGMBundledPromptDependenciesAndHarnessGuides(t *testing.T) {
	tree, err := materializeAt(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	root := tree.Payload(egmRoot)
	for _, doc := range []string{"claude.md", "codex.md", "muse.md"} {
		if !isFile(filepath.Join(root, "docs", doc)) || !isFile(tree.Host("egm/docs/"+doc)) {
			t.Errorf("docs/%s is not materialized", doc)
		}
	}
	codexRe := pyPattern(t, `\[Codex integration\]\(([^)]+)\)`)
	codex, _ := os.ReadFile(filepath.Join(root, "docs", "codex.md"))
	dirs := egmSkillDirs()
	if len(dirs) != 7 {
		t.Fatalf("skills %v", dirs)
	}
	var prompts []string
	for _, dir := range dirs {
		skill := filepath.Join(root, "skills", dir, "SKILL.md")
		prompts = append(prompts, skill)
		text, _ := os.ReadFile(skill)
		if !bytes.Contains(text, []byte("${CLAUDE_PLUGIN_ROOT}/docs/claude.md")) {
			t.Errorf("%s does not name the Claude guide", skill)
		}
		targets := pyFindAll(t, codexRe, string(text))
		if len(targets) != 1 || filepath.Join(filepath.Dir(skill), targets[0][1]) != filepath.Join(root, "docs", "codex.md") {
			t.Errorf("%s codex targets %v", skill, targets)
		}
		host := tree.Host("egm/skills/" + dir + "/SKILL.md")
		hostText, _ := os.ReadFile(host)
		if bytes.Contains(hostText, egmPluginRootVar) || !bytes.Contains(hostText, []byte(root+"/docs/claude.md")) || !isFile(filepath.Join(root, "docs", "claude.md")) {
			t.Errorf("%s does not resolve the plugin root onto %s", host, root)
		}
		hostTargets := pyFindAll(t, codexRe, string(hostText))
		resolved, _ := os.ReadFile(filepath.Join(filepath.Dir(host), hostTargets[0][1]))
		if len(hostTargets) != 1 || !bytes.Equal(resolved, codex) {
			t.Errorf("%s codex link does not reach docs/codex.md", host)
		}
	}
	agents, _ := filepath.Glob(filepath.Join(root, "agents", "*.md"))
	prompts = append(prompts, agents...)
	refRe := pyPattern(t, "`((?:agents|resources|references)/[^`\\s]+\\.md)`")
	checked := 0
	for _, prompt := range prompts {
		text, _ := os.ReadFile(prompt)
		for _, m := range pyFindAll(t, refRe, string(text)) {
			if strings.ContainsAny(m[1], "{}[]<>") {
				continue
			}
			base := root
			if strings.HasPrefix(m[1], "references/") {
				base = filepath.Dir(prompt)
			}
			checked++
			if !isFile(filepath.Join(base, filepath.FromSlash(m[1]))) {
				t.Errorf("%s: missing %s", prompt, m[1])
			}
		}
	}
	if checked == 0 {
		t.Fatal("no prompt dependencies found")
	}
}

func TestEGMArtifactDirectoryOverrideDocumentedForEachHarness(t *testing.T) {
	marker := "EGM artifact directory: "
	for _, c := range []struct{ guide, fallback string }{
		{"docs/claude.md", "use the working directory and say why"},
		{"docs/codex.md", "use the working directory and say why"},
		{"docs/harnesses.md", "the files go to the working directory"},
	} {
		raw, err := payloadFile(egmRoot + "/" + c.guide)
		if err != nil {
			t.Fatal(err)
		}
		text := strings.Join(pystr.SplitWhitespace(string(raw), -1), " ")
		if !strings.Contains(text, marker) {
			t.Errorf("%s does not document `%s<path>`", c.guide, marker)
		}
		for _, phrase := range []string{"resolves outside the working directory", "following symbolic links",
			"confirm it before creating the folder or writing there", "decline or no one can confirm", c.fallback} {
			if !strings.Contains(text, phrase) {
				t.Errorf("%s does not ask before using a folder outside the working directory: missing `%s`", c.guide, phrase)
			}
		}
	}
	guide, _ := payloadFile(egmRoot + "/docs/claude.md")
	if !strings.Contains(strings.Join(pystr.SplitWhitespace(string(guide), -1), " "), "name the artifact directory you are using in its task") {
		t.Error("docs/claude.md does not tell the main context to name the artifact directory")
	}
	synth, _ := payloadFile(egmRoot + "/agents/synthesizer.md")
	fm, _ := skills.ParseFrontmatter(string(synth))
	if fm["name"] != "egm-synthesizer" {
		t.Errorf("canonical synthesizer name %v", fm["name"])
	}
	if _, ok := fm["tools"]; ok {
		t.Error("canonical synthesizer must not restrict tools")
	}
}

func TestEGMSkillsHaveCoLocatedTestScenarios(t *testing.T) {
	tree, err := materializeAt(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(tree.Payload(egmRoot + "/skills"))
	if err != nil {
		t.Fatal(err)
	}
	var skillDirs []string
	for _, e := range entries {
		if e.IsDir() && !strings.HasPrefix(e.Name(), ".") && !strings.HasPrefix(e.Name(), "_") {
			skillDirs = append(skillDirs, e.Name())
		}
	}
	sort.Strings(skillDirs)
	if len(skillDirs) == 0 {
		t.Fatal("No skills discovered")
	}
	for _, name := range skillDirs {
		file := tree.Payload(egmRoot + "/skills/" + name + "/test_scenarios.yaml")
		raw, err := os.ReadFile(file)
		if err != nil {
			t.Errorf("Missing required co-located test_scenarios.yaml in %s", name)
			continue
		}
		if !isFile(tree.Payload(egmRoot + "/skills/" + name + "/TEST.md")) {
			t.Errorf("Missing co-located TEST.md in %s", name)
		}
		parsed, err := config.FastSafeLoad(raw)
		data, ok := parsed.(map[string]any)
		if err != nil || !ok {
			t.Errorf("%s must parse as a dictionary: %v", file, err)
			continue
		}
		if data["skill"] != name {
			t.Errorf("'skill' in %s (%v) must match directory name %s", file, data["skill"], name)
		}
		scenarios, _ := data["scenarios"].([]any)
		if len(scenarios) == 0 {
			t.Errorf("%s must define at least one scenario", file)
		}
		for _, raw := range scenarios {
			sc, _ := raw.(map[string]any)
			for _, key := range []string{"scenario_id", "eval_type"} {
				if _, ok := sc[key]; !ok {
					t.Errorf("Scenario in %s missing '%s'", file, key)
				}
			}
			if turns, ok := sc["turns"]; !ok || !pyrepr.Truthy(turns) {
				t.Errorf("Scenario in %s missing non-empty 'turns'", file)
			}
		}
	}
}

func TestEGMTablesHaveNoMultilineContinuationRows(t *testing.T) {
	tree, err := materializeAt(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	root := tree.Payload(egmRoot)
	fence, continuation := pyPattern(t, "```[\\s\\S]*?```"), pyPattern(t, `^:.*:\s*$`)
	var failures []string
	files := egmMarkdownFiles(t, root)
	for _, md := range files {
		raw, _ := os.ReadFile(md)
		content := pySub(t, fence, strings.ToValidUTF8(string(raw), ""), func(m string) string {
			return strings.Repeat("\n", strings.Count(m, "\n"))
		})
		previous := ""
		for number, line := range pystr.Splitlines(content, false) {
			if ok, _ := continuation.MatchString(line); ok && (strings.HasPrefix(previous, "|") || strings.HasPrefix(previous, ":")) {
				rel, _ := filepath.Rel(root, md)
				failures = append(failures, rel+":"+strconv.Itoa(number+1))
			}
			previous = line
		}
	}
	if len(files) < 60 {
		t.Fatalf("only %d markdown files scanned", len(files))
	}
	if len(failures) > 0 {
		t.Fatalf("Multi-line table rows do not render on GitHub. Put each row on one line:\n%s", strings.Join(failures, "\n"))
	}
}

func TestEGMStyleGoldfishAndReadinessBoundary(t *testing.T) {
	style, _ := payloadFile(egmRoot + "/agents/style.md")
	for _, action := range []string{"DROP", "CALIBRATE", "RELOCATE", "KEEP"} {
		if !strings.Contains(string(style), "`"+action+"`") {
			t.Errorf("Missing editorial calibration action %s in agents/style.md", action)
		}
	}
	if !strings.Contains(string(style), "Defensive Caveats & Uncalibrated Claims") {
		t.Error("agents/style.md lacks the defensive hedging and uncalibrated claims audit")
	}
	standard, _ := payloadFile(egmRoot + "/resources/document_standards/implementation-readiness-standard.md")
	for _, phrase := range []string{"Target Scope & File Boundary", "repository-relative", "Phase 5 write allowlist"} {
		if !strings.Contains(string(standard), phrase) {
			t.Errorf("implementation readiness standard lacks %q", phrase)
		}
	}
}
