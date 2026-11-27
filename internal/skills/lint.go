package skills

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/dlclark/regexp2/v2"

	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/pkg/fsx"
	"github.com/nickheyer/icebath/pkg/strs"
)

// One advisory finding about how a skill is written
type Finding struct {
	// error or warning
	Severity string `json:"severity"`
	Rule     string `json:"rule"`
	Message  string `json:"message"`
}

// Shell programs named in prose, with the tool to name instead
var shellTools = [][2]string{
	{"grep", "search_files"}, {"rg", "search_files"}, {"cat", "read_file"}, {"head", "read_file"}, {"tail", "read_file"},
	{"sed", "patch"}, {"awk", "patch"}, {"find", "search_files (target='files')"}, {"ls", "search_files (target='files')"},
}

var (
	marketingWords  = []string{"powerful", "comprehensive", "seamless", "advanced", "cutting-edge", "state-of-the-art", "revolutionary", "robust"}
	posixPrimitives = []string{"fcntl", "termios", "os.setsid", "signal.SIGKILL", "osascript", "/proc/", "apt-get", "systemctl"}
	forbiddenFiles  = []string{"README.md", "CHANGELOG.md", "install.sh", ".env", ".env.example", ".gitignore"}
)

const (
	incidentMin      = 4
	incidentPerKChar = 0.5
	maxReferences    = 60
	bodyBudget       = 24_000
	wordClass        = `\p{L}\p{N}_`
)

var (
	codeBlock   = regexp.MustCompile("(?s)```.*?```")
	lintName    = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)
	incident    = regexp2.MustCompile(`(?<![`+wordClass+`/])#\p{Nd}{3,6}(?![`+wordClass+`])|(?<![`+wordClass+`])(?:PR|issue)[`+spaceClass+`]*#?\p{Nd}{3,6}(?![`+wordClass+`])`, regexp2.None)
	supportLink = regexp.MustCompile(`(references|templates|assets)/[` + wordClass + `./-]+`)
	whenToUse   = regexp.MustCompile(`(?m)^#+[` + spaceClass + `]+When to [Uu]se`)
)

func wordRune(r rune) bool { return r == '_' || unicode.IsLetter(r) || unicode.IsNumber(r) }

// Whether the word stands in the text as a whole word
func wholeWord(text, word string) bool {
	first, _ := utf8.DecodeRuneInString(word)
	last, _ := utf8.DecodeLastRuneInString(word)
	for start := 0; start <= len(text); {
		i := strings.Index(text[start:], word)
		if i < 0 {
			return false
		}
		at, end := start+i, start+i+len(word)
		before, _ := utf8.DecodeLastRuneInString(text[:at])
		after, _ := utf8.DecodeRuneInString(text[end:])
		if (at == 0 || wordRune(before) != wordRune(first)) && (end == len(text) || wordRune(after) != wordRune(last)) {
			return true
		}
		_, size := utf8.DecodeRuneInString(text[at:])
		start = at + size
	}
	return false
}

// Names in quotes as a sentence lists them, the last after "and"
func quoted(values []string) string {
	parts := make([]string, len(values))
	for i, v := range values {
		parts[i] = "“" + v + "”"
	}
	if len(parts) < 2 {
		return strings.Join(parts, "")
	}
	return strings.Join(parts[:len(parts)-1], ", ") + " and " + parts[len(parts)-1]
}

func lintFrontmatter(fm Frontmatter, dir string) []Finding {
	var out []Finding
	name := strings.TrimSpace(fm.Name(""))
	if name != "" && !lintName.MatchString(name) {
		out = append(out, Finding{"error", "name-format", fmt.Sprintf("The name %s may hold only lowercase letters, digits, hyphens and underscores.", name)})
	}
	if dir != "" && name != "" && name != filepath.Base(dir) {
		out = append(out, Finding{"error", "name-dir-mismatch", fmt.Sprintf("The name %s differs from the name of its folder, %s. They must be the same.", name, filepath.Base(dir))})
	}
	description := strings.Trim(strings.TrimSpace(text(fm["description"])), `'"`)
	if n := utf8.RuneCountInString(description); n > IndexDescription {
		out = append(out, Finding{"warning", "description-length", fmt.Sprintf("The description has %s, and the skill index shows only %s, so the agent never sees the rest. Keep it to one sentence.", strs.Count(n, "character"), strs.Number(IndexDescription))})
	}
	var hits []string
	for _, word := range marketingWords {
		if wholeWord(strings.ToLower(description), word) {
			hits = append(hits, word)
		}
	}
	if len(hits) > 0 {
		out = append(out, Finding{"warning", "description-marketing", fmt.Sprintf("The description sells with %s. Say what the skill does instead.", quoted(hits))})
	}
	for _, key := range []string{"version", "author", "license"} {
		if _, ok := fm[key]; !ok {
			out = append(out, Finding{"warning", "missing-metadata", fmt.Sprintf("The frontmatter has no %s, which every other skill has.", key)})
		}
	}
	var meta map[string]any
	for _, key := range metadataKeys {
		if meta = mapping(mapping(fm["metadata"])[key]); meta != nil {
			break
		}
	}
	if meta == nil {
		out = append(out, Finding{"warning", "missing-metadata", "The frontmatter has no metadata.icebath with tags and related_skills."})
	} else if _, ok := meta["tags"]; !ok {
		out = append(out, Finding{"warning", "missing-metadata", "The frontmatter has no metadata.icebath.tags."})
	}
	author := text(fm["author"])
	switch strings.ToLower(strings.TrimSpace(author)) {
	case "icebath", "agent", "icebath agent":
		// Icebath is what the skill-authoring standards tell the agent to write, Icebath Agent what the shipped skills carry
		if author != "Icebath Agent" && author != "Icebath" {
			out = append(out, Finding{"warning", "author-caps", fmt.Sprintf("The author %s should be written Icebath or Icebath Agent, or be the name of a real person.", author)})
		}
	}
	if platforms := fm["platforms"]; config.Truthy(platforms) {
		items, ok := platforms.([]any)
		if !ok {
			items = []any{platforms}
		}
		var bad []string
		for _, p := range items {
			switch strings.ToLower(text(p)) {
			case "linux", "macos", "windows", "darwin":
			default:
				bad = append(bad, text(p))
			}
		}
		if len(bad) > 0 {
			out = append(out, Finding{"warning", "platforms-value", fmt.Sprintf("The platforms list holds %s, which icebath doesn't know. Use only darwin, linux, macos and windows.", quoted(bad))})
		}
	}
	return out
}

func lintBody(body, dir string) []Finding {
	var out []Finding
	if n := utf8.RuneCountInString(body); n > bodyBudget {
		out = append(out, Finding{"warning", "oversized-body", fmt.Sprintf("The body of SKILL.md has %s, about %s, and all of it stays in the context of the agent for the rest of the session. Keep only the rules that always apply here, about 200 lines, and move the depth of each topic into references/<topic>.md, linked from the body.", strs.Count(n, "character"), strs.Count(n/4, "token"))})
	}
	prose := codeBlock.ReplaceAllString(body, "")
	for _, pair := range shellTools {
		if strings.Contains(prose, "`"+pair[0]+"`") {
			out = append(out, Finding{"warning", "shell-utility-reference", fmt.Sprintf("The text names %s. Name the tool %s of the agent instead.", pair[0], pair[1])})
		}
	}
	if !whenToUse.MatchString(body) {
		out = append(out, Finding{"warning", "missing-section", "There is no ## When to Use section. A skill must say near the top when it applies."})
	}
	refs := 0
	for m, _ := incident.FindStringMatch(prose); m != nil; m, _ = incident.FindNextMatch(m) {
		refs++
	}
	if length := max(1, utf8.RuneCountInString(prose)); refs >= incidentMin && float64(refs)/float64(length)*1000 >= incidentPerKChar {
		out = append(out, Finding{"warning", "incident-log-shape", fmt.Sprintf("The text cites %s. Write the general rule and why it holds, and drop the numbers, because the rule must stand without the story.", strs.Count(refs, "pull request or issue number"))})
	}
	if dir == "" {
		return out
	}
	seen := map[string]bool{}
	for _, rel := range supportLink.FindAllString(body, -1) {
		if seen[rel] || strings.Contains(rel, "*") || strings.HasSuffix(rel, "/") {
			continue
		}
		seen[rel] = true
		if !fsx.Exists(filepath.Join(dir, filepath.FromSlash(rel))) {
			out = append(out, Finding{"warning", "dangling-reference", fmt.Sprintf("The body links %s, and the folder of the skill has no such file.", rel)})
		}
	}
	return out
}

// Every path under a folder, in walk order
func walked(root string) []string {
	var out []string
	filepath.WalkDir(root, func(p string, _ os.DirEntry, err error) error {
		if err == nil && p != root {
			out = append(out, p)
		}
		return nil
	})
	return out
}

func lintFiles(fm Frontmatter, dir string) []Finding {
	var out []Finding
	var offenders []string
	if scripts := filepath.Join(dir, "scripts"); !config.Truthy(fm["platforms"]) && fsx.IsDir(scripts) {
		for _, script := range walked(scripts) {
			ext := filepath.Ext(script)
			if !fsx.IsFile(script) || ext != ".py" && ext != ".sh" && ext != ".bash" {
				continue
			}
			data, err := os.ReadFile(script)
			if err != nil {
				continue
			}
			content := strings.TrimPrefix(strings.ToValidUTF8(string(data), ""), "\uFEFF")
			var hit []string
			for _, primitive := range posixPrimitives {
				if strings.Contains(content, primitive) {
					hit = append(hit, primitive)
				}
			}
			if len(hit) > 0 {
				offenders = append(offenders, filepath.Base(script)+" uses "+quoted(hit))
			}
		}
	}
	if len(offenders) > 0 {
		out = append(out, Finding{"warning", "platforms-gating", fmt.Sprintf("Scripts use what only Linux and macOS have, and the frontmatter has no platforms: %s. Make them work everywhere, or add platforms: [linux, macos].", strings.Join(offenders, ", "))})
	}
	for _, name := range forbiddenFiles {
		if fsx.Exists(filepath.Join(dir, name)) {
			out = append(out, Finding{"warning", "forbidden-file", fmt.Sprintf("The skill ships %s. A skill should hold no files of build tools or settings.", name)})
		}
	}
	if refs := filepath.Join(dir, "references"); fsx.IsDir(refs) {
		n := 0
		for _, p := range walked(refs) {
			base := filepath.Base(p)
			if runtime.GOOS == "windows" {
				base = strings.ToLower(base)
			}
			underscored := false
			for _, part := range strings.FieldsFunc(p, func(r rune) bool { return r == '/' || r == filepath.Separator }) {
				underscored = underscored || strings.HasPrefix(part, "_")
			}
			if strings.HasSuffix(base, ".md") && !underscored {
				n++
			}
		}
		if n > maxReferences {
			out = append(out, Finding{"warning", "references-sprawl", fmt.Sprintf("The references folder holds %s, which reads like a log of sessions, not depth on topics. Merge the files of each topic into one set of rules and drop the stories of incidents.", strs.Count(n, "file"))})
		}
	}
	return out
}

// Findings about a SKILL.md text, the checks of its folder only when the folder is given
func lintText(content, dir string) []Finding {
	fm, body := Parse(content)
	out := append(lintFrontmatter(fm, dir), lintBody(body, dir)...)
	if dir != "" {
		out = append(out, lintFiles(fm, dir)...)
	}
	return out
}

// Findings about a SKILL.md on disk, every check of its folder made
func LintFile(skillMD string) ([]Finding, error) {
	data, err := os.ReadFile(skillMD)
	if err != nil {
		return nil, err
	}
	return lintText(strings.TrimPrefix(strings.ToValidUTF8(string(data), ""), "\uFEFF"), filepath.Dir(skillMD)), nil
}
