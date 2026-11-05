package skills

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"unicode/utf8"

	"github.com/dlclark/regexp2/v2"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// Lint severities attached to findings
const (
	LintError   = "error"
	LintWarning = "warning"
)

// One advisory lint result
type LintFinding struct {
	Severity string
	Rule     string
	Message  string
}

// Shell utility named in prose mapped to the native tool to name instead, in upstream order
var lintShellUtilToTool = [][2]string{
	{"grep", "search_files"}, {"rg", "search_files"}, {"cat", "read_file"}, {"head", "read_file"},
	{"tail", "read_file"}, {"sed", "patch"}, {"awk", "patch"},
	{"find", "search_files (target='files')"}, {"ls", "search_files (target='files')"},
}

var (
	lintMarketingWords   = []string{"powerful", "comprehensive", "seamless", "advanced", "cutting-edge", "state-of-the-art", "revolutionary", "robust"}
	lintPosixPrimitives  = []string{"fcntl", "termios", "os.setsid", "signal.SIGKILL", "osascript", "/proc/", "apt-get", "systemctl"}
	lintForbiddenFiles   = []string{"README.md", "CHANGELOG.md", "install.sh", ".env", ".env.example", ".gitignore"}
	lintExpectedSections = []string{"When to Use", "When to use"}
)

const (
	lintIncidentRefMin      = 4
	lintIncidentRefPerKChar = 0.5
	lintMaxReferenceFiles   = 60
	lintBodySoftBudgetChars = 24_000
	pyWordClass             = `\p{L}\p{N}_`
	pySpaceClass            = `\t\n\v\f\r \x1c-\x1f\x85\p{Z}`
)

var (
	lintCodeBlockRe = regexp.MustCompile("(?s)```.*?```")
	lintNameRe      = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)
	lintIncidentRe  = regexp2.MustCompile(`(?<![`+pyWordClass+`/])#\p{Nd}{3,6}(?![`+pyWordClass+`])|(?<![`+pyWordClass+`])(?:PR|issue)[`+pySpaceClass+`]*#?\p{Nd}{3,6}(?![`+pyWordClass+`])`, regexp2.None)
	lintLinkRe      = regexp.MustCompile(`(references|templates|assets)/[` + pyWordClass + `./-]+`)
	lintSectionRes  = func() []*regexp.Regexp {
		out := make([]*regexp.Regexp, 0, len(lintExpectedSections))
		for _, s := range lintExpectedSections {
			out = append(out, regexp.MustCompile(`(?m)^#+[`+pySpaceClass+`]+`+regexp.QuoteMeta(s)))
		}
		return out
	}()
)

// Reports a Python regex word character
func isPyWordRune(r rune) bool { return r == '_' || pystr.IsAlnum(r) }

// Reports word occurring in text with Python word boundaries on both sides
func containsWholeWord(text, word string) bool {
	for start := 0; start <= len(text); {
		i := strings.Index(text[start:], word)
		if i < 0 {
			return false
		}
		at := start + i
		end := at + len(word)
		prev, _ := utf8.DecodeLastRuneInString(text[:at])
		next, _ := utf8.DecodeRuneInString(text[end:])
		first, _ := utf8.DecodeRuneInString(word)
		last, _ := utf8.DecodeLastRuneInString(word)
		leftOK := at == 0 || isPyWordRune(prev) != isPyWordRune(first)
		rightOK := end == len(text) || isPyWordRune(next) != isPyWordRune(last)
		if leftOK && rightOK {
			return true
		}
		_, size := utf8.DecodeRuneInString(text[at:])
		start = at + size
	}
	return false
}

// Thousands separated integer text
func commaInt(n int) string {
	s, err := pyrepr.Format(n, ",")
	if err != nil {
		return fmt.Sprint(n)
	}
	return s
}

// Frontmatter rule findings, the directory checks only when a skill dir is known
func lintFrontmatter(fm map[string]any, skillDir string) []LintFinding {
	var out []LintFinding
	name := ""
	if v, ok := fm["name"]; ok {
		name = pyStrValue(v)
	}
	name = pystr.Strip(name)
	if name != "" && !lintNameRe.MatchString(name) {
		out = append(out, LintFinding{LintError, "name-format", fmt.Sprintf("name '%s' must be lowercase letters, digits, hyphens, and underscores only.", name)})
	}
	if skillDir != "" && name != "" && name != filepath.Base(skillDir) {
		out = append(out, LintFinding{LintError, "name-dir-mismatch", fmt.Sprintf("frontmatter name '%s' does not match directory '%s'; they must be identical.", name, filepath.Base(skillDir))})
	}
	desc := ""
	if v, ok := fm["description"]; ok {
		desc = pyStrValue(v)
	}
	desc = strings.Trim(pystr.Strip(desc), "'\"")
	if n := utf8.RuneCountInString(desc); n > SkillPromptDescLimit {
		out = append(out, LintFinding{LintWarning, "description-length", fmt.Sprintf("description is %d chars; the skill index shows %d and clips the rest, losing routing signal. Keep it to one sentence.", n, SkillPromptDescLimit)})
	}
	var hits []any
	lowered := pystr.Lower(desc)
	for _, w := range lintMarketingWords {
		if containsWholeWord(lowered, w) {
			hits = append(hits, w)
		}
	}
	if len(hits) > 0 {
		out = append(out, LintFinding{LintWarning, "description-marketing", fmt.Sprintf("description contains marketing words %s; state the capability, not adjectives.", pyrepr.Repr(hits))})
	}
	for _, key := range []string{"version", "author", "license"} {
		if _, ok := fm[key]; !ok {
			out = append(out, LintFinding{LintWarning, "missing-metadata", fmt.Sprintf("frontmatter is missing '%s'; every peer skill has it.", key)})
		}
	}
	var meta map[string]any
	if m := fmMap(fm["metadata"]); m != nil {
		if im := fmMap(m["icebath"]); im != nil {
			meta = im
		} else {
			meta = fmMap(m["hermes"])
		}
	}
	if meta == nil {
		out = append(out, LintFinding{LintWarning, "missing-metadata", "frontmatter is missing metadata.icebath.{tags, related_skills}."})
	} else if _, ok := meta["tags"]; !ok {
		out = append(out, LintFinding{LintWarning, "missing-metadata", "metadata.icebath.tags is missing."})
	}
	author := ""
	if v, ok := fm["author"]; ok {
		author = pyStrValue(v)
	}
	switch pystr.Lower(pystr.Strip(author)) {
	case "icebath", "agent", "icebath agent":
		if author != "Icebath Agent" {
			out = append(out, LintFinding{LintWarning, "author-caps", fmt.Sprintf("author '%s' should be 'Icebath Agent' (proper caps) or a real contributor name.", author)})
		}
	}
	if platforms := fm["platforms"]; fmTruthy(platforms) {
		items, ok := platforms.([]any)
		if !ok {
			items = []any{platforms}
		}
		var bad []any
		for _, p := range items {
			switch pystr.Lower(pyStrValue(p)) {
			case "linux", "macos", "windows", "darwin":
			default:
				bad = append(bad, p)
			}
		}
		if len(bad) > 0 {
			out = append(out, LintFinding{LintWarning, "platforms-value", fmt.Sprintf("platforms contains unrecognized value(s) %s; expected a subset of ['darwin', 'linux', 'macos', 'windows'].", pyrepr.Repr(bad))})
		}
	}
	return out
}

// Body rule findings, dangling links checked only when a skill dir is known
func lintBody(body, skillDir string) []LintFinding {
	var out []LintFinding
	if n := utf8.RuneCountInString(body); n > lintBodySoftBudgetChars {
		out = append(out, LintFinding{LintWarning, "oversized-body", fmt.Sprintf("SKILL.md body is %s chars (~%s tokens); skill_view loads all of it and it stays in context for every later call of the session. Keep the always-on rules here (~200 lines) and move topic depth into references/<topic>.md, linked from the body.", commaInt(n), commaInt(n/4))})
	}
	prose := lintCodeBlockRe.ReplaceAllString(body, "")
	for _, pair := range lintShellUtilToTool {
		if strings.Contains(prose, "`"+pair[0]+"`") {
			out = append(out, LintFinding{LintWarning, "shell-utility-reference", fmt.Sprintf("prose references `%s`; name the native tool `%s` instead.", pair[0], pair[1])})
		}
	}
	found := false
	for _, re := range lintSectionRes {
		if re.MatchString(body) {
			found = true
			break
		}
	}
	if !found {
		out = append(out, LintFinding{LintWarning, "missing-section", "no '## When to Use' section found; skills need explicit trigger conditions near the top."})
	}
	refs := 0
	for m, _ := lintIncidentRe.FindStringMatch(prose); m != nil; m, _ = lintIncidentRe.FindNextMatch(m) {
		refs++
	}
	proseLen := utf8.RuneCountInString(prose)
	if proseLen < 1 {
		proseLen = 1
	}
	if refs >= lintIncidentRefMin && float64(refs)/float64(proseLen)*1000 >= lintIncidentRefPerKChar {
		out = append(out, LintFinding{LintWarning, "incident-log-shape", fmt.Sprintf("%d PR/issue references in prose; write the generalizable rule + why and drop the incident numbers — the rule must stand without the story.", refs)})
	}
	if skillDir == "" {
		return out
	}
	seen := map[string]bool{}
	for _, rel := range lintLinkRe.FindAllString(body, -1) {
		if seen[rel] || strings.Contains(rel, "*") || strings.HasSuffix(rel, "/") {
			continue
		}
		seen[rel] = true
		if _, err := os.Stat(filepath.Join(skillDir, rel)); err != nil {
			out = append(out, LintFinding{LintWarning, "dangling-reference", fmt.Sprintf("body references '%s' but that file does not exist in the skill directory.", rel)})
		}
	}
	return out
}

// Every path under root in lexical walk order, root itself excluded
func lintWalk(root string) []string {
	var out []string
	_ = filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err == nil && p != root {
			out = append(out, p)
		}
		return nil
	})
	return out
}

// On disk findings for POSIX only scripts, scaffolding files and reference sprawl
func lintFiles(fm map[string]any, skillDir string) []LintFinding {
	var out []LintFinding
	scriptsDir := filepath.Join(skillDir, "scripts")
	var order []string
	offenders := map[string][]any{}
	if !fmTruthy(fm["platforms"]) && isDir(scriptsDir) {
		for _, script := range lintWalk(scriptsDir) {
			ext := filepath.Ext(script)
			if !isFile(script) || (ext != ".py" && ext != ".sh" && ext != ".bash") {
				continue
			}
			data, err := os.ReadFile(script)
			if err != nil {
				continue
			}
			text := strings.TrimPrefix(strings.ToValidUTF8(string(data), ""), "\xEF\xBB\xBF")
			var hit []any
			for _, p := range lintPosixPrimitives {
				if strings.Contains(text, p) {
					hit = append(hit, p)
				}
			}
			if len(hit) > 0 {
				base := filepath.Base(script)
				if _, ok := offenders[base]; !ok {
					order = append(order, base)
				}
				offenders[base] = hit
			}
		}
	}
	if len(order) > 0 {
		parts := make([]string, 0, len(order))
		for _, k := range order {
			parts = append(parts, k+": "+pyrepr.Repr(offenders[k]))
		}
		out = append(out, LintFinding{LintWarning, "platforms-gating", fmt.Sprintf("scripts use POSIX-only primitives (%s) but no 'platforms:' frontmatter is declared. Fix cross-platform or gate with platforms: [linux, macos].", strings.Join(parts, "; "))})
	}
	for _, name := range lintForbiddenFiles {
		if _, err := os.Stat(filepath.Join(skillDir, name)); err == nil {
			out = append(out, LintFinding{LintWarning, "forbidden-file", fmt.Sprintf("skill ships '%s'; skills should not include scaffolding/config files.", name)})
		}
	}
	refsDir := filepath.Join(skillDir, "references")
	if isDir(refsDir) {
		n := 0
		for _, p := range lintWalk(refsDir) {
			base := filepath.Base(p)
			if runtime.GOOS == "windows" {
				base = strings.ToLower(base)
			}
			if !strings.HasSuffix(base, ".md") || pathHasUnderscorePart(p) {
				continue
			}
			n++
		}
		if n > lintMaxReferenceFiles {
			out = append(out, LintFinding{LintWarning, "references-sprawl", fmt.Sprintf("%d files under references/; that is a per-session log, not topical depth. Merge same-topic files into one rule set and drop incident narration.", n)})
		}
	}
	return out
}

// Reports any component of the path starting with an underscore
func pathHasUnderscorePart(p string) bool {
	for _, part := range strings.FieldsFunc(p, func(r rune) bool { return r == '/' || r == filepath.Separator }) {
		if strings.HasPrefix(part, "_") {
			return true
		}
	}
	return false
}

// Lints SKILL.md text, the on disk checks running only when a skill dir is given
func LintContent(content, skillDir string) []LintFinding {
	fm, body := ParseFrontmatter(content)
	out := append(lintFrontmatter(fm, skillDir), lintBody(body, skillDir)...)
	if skillDir != "" {
		out = append(out, lintFiles(fm, skillDir)...)
	}
	return out
}

// Lints a SKILL.md on disk with every on disk check enabled
func LintSkill(skillMD string) ([]LintFinding, error) {
	data, err := os.ReadFile(skillMD)
	if err != nil {
		return nil, err
	}
	text := strings.TrimPrefix(strings.ToValidUTF8(string(data), ""), "\xEF\xBB\xBF")
	return LintContent(text, filepath.Dir(skillMD)), nil
}
