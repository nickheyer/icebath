package plugins

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/dlclark/regexp2/v2"
	"github.com/nickheyer/icebath/pkg/internal/pyre"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// Compiles a Python re pattern with Python semantics, panicking on an invalid pattern
func mustPyRe(pattern string) *regexp2.Regexp {
	re, err := pyre.Compile(pattern)
	if err != nil {
		panic(err)
	}
	return re
}

type desktopRule struct {
	name string
	re   *regexp2.Regexp
}

// A desktop finding, the rule or review cue and the line it sits on
type DesktopFinding struct {
	Rule string
	Line int
}

var desktopForbidden = []desktopRule{
	{"dynamic code evaluation", mustPyRe(`(?<![\w$.])eval\(|\bnew\s+Function\(`)},
	{"dynamic import outside the SDK", mustPyRe(`\bimport\(\s*(?!['"](?:@icebath/plugin-sdk|@hermes/plugin-sdk|react)(?:/[\w/-]*)?['"]\s*\))`)},
	{"remote import outside the SDK", mustPyRe(`\bimport\s+(?:[^;'"]*?\bfrom\s*)?['"][a-zA-Z][\w+.-]*:`)},
	{"script injection", mustPyRe(`createElement\(\s*['"]script['"]\s*\)|<script\b`)},
	{"app DOM reach", mustPyRe(`\bdocument\.(?:querySelector(?:All)?|getElementsBy\w+|getElementById)\(\s*[` + "`" + `'"][^` + "`" + `'"]*\[data-(?:slot|tour|sidebar|testid)\b`)},
	{"app DOM reach", mustPyRe(`\.observe\(\s*document\.body\s*,\s*\{[^}]*\b(?:childList|subtree)\b`)},
	{"preview guest reach", mustPyRe(`\b(?:querySelector(?:All)?|closest|getElementsByTagName|createElement)\(\s*[` + "`" + `'"]webview\b`)},
}

var desktopReview = []desktopRule{
	{"raw bridge window.hermesDesktop", mustPyRe(`\b(?:window|globalThis|self)\s*(?:\?\.|\.)\s*hermesDesktop\b`)},
	{"app markup selector", mustPyRe(`\[data-(?:slot|tour|sidebar|testid|tree-[\w-]+|pane-host|titlebar-cluster)\b`)},
}

var (
	desktopCommentRe        = mustPyRe("(?s)(\"(?:[^\"\\\\\\n]|\\\\.)*\"|'(?:[^'\\\\\\n]|\\\\.)*'|`(?:[^`\\\\]|\\\\.)*`)|/\\*.*?\\*/|(?<![:\\w])//[^\\n]*")
	desktopRegexLiteralRe   = mustPyRe(`(?<![\w)\]])/(?:[^/\\\n\[]|\\.|\[(?:[^\]\\\n]|\\.)*\])+/[a-z]*`)
	desktopJSString         = `(?:"(?:[^"\\\n]|\\.)*"|'(?:[^'\\\n]|\\.)*')`
	desktopRegexpCtorRe     = mustPyRe(`\.(?:replace|replaceAll|split|match|matchAll|search)\(\s*new\s+RegExp\(\s*` + desktopJSString + `|\bnew\s+RegExp\(\s*` + desktopJSString + `(?=[^()\n]*\)\s*\.\s*(?:test|exec)\()`)
	desktopMarkupRules      = map[string]bool{"script injection": true}
	desktopRootEntry        = "plugin.js"
	desktopDir              = "desktop"
	desktopSurfaceHitsShown = 8
)

// Comments blanked to their newlines with every string literal kept whole
func stripJSComments(source string) string {
	out, err := desktopCommentRe.ReplaceFunc(source, func(m regexp2.Match) string {
		if g := m.GroupByNumber(1); g != nil && len(g.Captures) > 0 {
			return g.String()
		}
		return strings.Repeat("\n", strings.Count(m.String(), "\n"))
	}, -1, -1)
	if err != nil {
		panic(err)
	}
	return out
}

// Every match of a pattern replaced by spaces of the same length
func blankMatches(re *regexp2.Regexp, source string) string {
	out, err := re.ReplaceFunc(source, func(m regexp2.Match) string { return strings.Repeat(" ", m.RuneLength) }, -1, -1)
	if err != nil {
		panic(err)
	}
	return out
}

// Regex literals and matcher RegExp constructor strings masked for the markup shaped rules
func maskRegexLiterals(source string) string {
	return blankMatches(desktopRegexpCtorRe, blankMatches(desktopRegexLiteralRe, source))
}

// One based line of every match of a pattern in a haystack
func matchLines(re *regexp2.Regexp, haystack string) []int {
	runes := []rune(haystack)
	var lines []int
	m, err := re.FindStringMatch(haystack)
	for err == nil && m != nil {
		n := 1
		for _, r := range runes[:m.RuneIndex] {
			if r == '\n' {
				n++
			}
		}
		lines = append(lines, n)
		m, err = re.FindNextMatch(m)
	}
	if err != nil {
		panic(err)
	}
	return lines
}

// Every forbidden construct of a plugin.js source by line
func DesktopSurfaceFindings(source string) []DesktopFinding {
	stripped := stripJSComments(source)
	noRegex := maskRegexLiterals(stripped)
	findings := []DesktopFinding{}
	for _, rule := range desktopForbidden {
		haystack := stripped
		if desktopMarkupRules[rule.name] {
			haystack = noRegex
		}
		for _, line := range matchLines(rule.re, haystack) {
			findings = append(findings, DesktopFinding{rule.name, line})
		}
	}
	sort.SliceStable(findings, func(i, j int) bool { return findings[i].Line < findings[j].Line })
	return findings
}

// Every review cue of a plugin.js source by line, lines a refusal covers skipped
func DesktopReviewFindings(source string) []DesktopFinding {
	stripped := stripJSComments(source)
	refused := map[int]bool{}
	for _, f := range DesktopSurfaceFindings(source) {
		refused[f.Line] = true
	}
	found := map[DesktopFinding]bool{}
	for _, cue := range desktopReview {
		for _, line := range matchLines(cue.re, stripped) {
			found[DesktopFinding{cue.name, line}] = true
		}
	}
	out := []DesktopFinding{}
	for hit := range found {
		if !refused[hit.Line] {
			out = append(out, hit)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Line != out[j].Line {
			return out[i].Line < out[j].Line
		}
		return out[i].Rule < out[j].Rule
	})
	return out
}

func hasDesktopRootEntry(pluginDir string) bool {
	_, err := os.Stat(filepath.Join(pluginDir, desktopRootEntry))
	return err == nil
}

// Suffix of a final path component as pathlib computes it
func pureSuffix(name string) string {
	i := strings.LastIndex(name, ".")
	if 0 < i && i < len(name)-1 {
		return name[i:]
	}
	return ""
}

// Reports whether a relative path belongs to the Desktop surface the lint governs
func IsDesktopSurface(relPath string, rootEntry bool) bool {
	parts := []string{}
	for _, p := range strings.Split(filepath.ToSlash(relPath), "/") {
		if p != "" && p != "." {
			parts = append(parts, p)
		}
	}
	if len(parts) == 0 || pureSuffix(parts[len(parts)-1]) != ".js" {
		return false
	}
	if len(parts) == 1 {
		return parts[0] == desktopRootEntry || rootEntry
	}
	return parts[0] == desktopDir
}

// Lexicographic order of slash separated paths compared component by component
func lessByParts(a, b string) bool {
	pa, pb := strings.Split(a, "/"), strings.Split(b, "/")
	for i := 0; i < len(pa) && i < len(pb); i++ {
		if pa[i] != pb[i] {
			return pa[i] < pb[i]
		}
	}
	return len(pa) < len(pb)
}

// Relative slash paths of the Desktop surface files of a plugin in pathlib order
func desktopSurfaceFiles(pluginDir string) []string {
	rootEntry := hasDesktopRootEntry(pluginDir)
	candidates := []string{}
	if entries, err := os.ReadDir(pluginDir); err == nil {
		for _, e := range entries {
			if strings.HasSuffix(e.Name(), ".js") {
				candidates = append(candidates, e.Name())
			}
		}
	}
	if isDir(filepath.Join(pluginDir, desktopDir)) {
		_ = filepath.WalkDir(filepath.Join(pluginDir, desktopDir), func(p string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(d.Name(), ".js") {
				return nil
			}
			if rel, err := filepath.Rel(pluginDir, p); err == nil {
				candidates = append(candidates, filepath.ToSlash(rel))
			}
			return nil
		})
	}
	out := []string{}
	for _, rel := range candidates {
		if isFile(filepath.Join(pluginDir, filepath.FromSlash(rel))) && IsDesktopSurface(rel, rootEntry) {
			out = append(out, rel)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return lessByParts(out[i], out[j]) })
	return out
}

// Rule and site of every finding over the plugin's Desktop surface files, refusals unless findings is given
func DesktopSurfaceHits(pluginDir string, findings func(string) []DesktopFinding) []string {
	if findings == nil {
		findings = DesktopSurfaceFindings
	}
	hits := []string{}
	for _, rel := range desktopSurfaceFiles(pluginDir) {
		raw, err := os.ReadFile(filepath.Join(pluginDir, filepath.FromSlash(rel)))
		if err != nil {
			continue
		}
		source := pystr.DecodeUTF8Replace(stripBOM(raw))
		for _, f := range findings(source) {
			hits = append(hits, fmt.Sprintf("%s (%s:%d)", f.Rule, rel, f.Line))
		}
	}
	return hits
}

// First eight hits joined with a count of the rest
func shownHits(hits []string) string {
	if len(hits) <= desktopSurfaceHitsShown {
		return strings.Join(hits, "; ")
	}
	return strings.Join(hits[:desktopSurfaceHitsShown], "; ") + fmt.Sprintf(" (+%d more)", len(hits)-desktopSurfaceHitsShown)
}

// Fails the report when the Desktop surface steps outside the SDK, silent when there is none
func checkDesktopSurface(report *ValidationReport, pluginDir string) {
	if !isDir(filepath.Join(pluginDir, desktopDir)) && !hasDesktopRootEntry(pluginDir) {
		return
	}
	hits := DesktopSurfaceHits(pluginDir, nil)
	if len(hits) > 0 {
		report.add("desktop surface", false, shownHits(hits))
	} else {
		report.add("desktop surface", true, "stays inside the plugin SDK surface")
	}
	if cues := DesktopSurfaceHits(pluginDir, DesktopReviewFindings); len(cues) > 0 {
		report.warn("desktop surface review (catalog rule 8: SDK surface only): " + shownHits(cues) +
			". Use ctx.files / ctx.os / host.request and SDK slots, or open an issue for the hook you need.")
	}
}
