package tools

import (
	"fmt"
	"math"
	"math/big"
	"os"
	"os/user"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"unicode"

	"github.com/dlclark/regexp2/v2"
)

// Directory names of dependency, build, cache, VCS and backup trees that broad recursive scans skip
var SearchPruneDirNames = setOf(
	".git", ".hg", ".svn",
	"node_modules", "venv", ".venv", "site-packages", "dist-packages",
	"vendor", "third_party",
	"build", "dist", "target", "out", "coverage",
	".next", ".turbo", ".parcel-cache", ".nuxt", ".svelte-kit",
	".archive",
	"__pycache__", ".cache", ".Trash", ".tox", ".nox", ".mypy_cache",
	".pytest_cache", ".ruff_cache", ".npm", ".yarn", ".pnpm-store",
	".gradle", ".m2", ".nuget",
	"backups", "backup", ".backups",
)

// Platform the search treats as the controller, swapped by tests that model a macOS host
var searchHostPlatform = runtime.GOOS

// Controller home captured at load like Path.home, swapped by tests
var searchHome = searchDefaultHome()

// Home directory of the controller user from the environment, else the account database
func searchDefaultHome() string {
	if h, err := os.UserHomeDir(); err == nil && h != "" {
		return h
	}
	if u, err := user.Current(); err == nil {
		return u.HomeDir
	}
	return ""
}

const (
	admissionInterruptedError = "File search was interrupted while waiting for another filename " +
		"search on the same root. Retry when ready."
	permissionSearchHint   = "Nothing matched in the readable part of the search scope."
	findModifiedOrderError = "Exact modification-time order requires GNU find with " +
		"-printf support; install ripgrep 14+ or use order='discovery'."
)

// Stderr bytes the native rg runner keeps before tallying the rest, swapped by tests
var stderrRetainBytes = 64 * 1024

// Interrupt check the admission wait and the native runner poll, swapped by tests
var searchIsInterrupted = IsInterrupted

// Negated Python whitespace characters for building bracket classes
var pyWSChars = pyWSClass[1 : len(pyWSClass)-1]

var (
	searchTimeoutMarkerRe = regexp.MustCompile(`\n?\[Command timed out after \p{Nd}+s\]` + pyWSClass + `*$`)
	searchOutputRe        = regexp.MustCompile(`^(?:([A-Za-z]:)?[^` + pyWSChars + `:][^\n]*?[:\-]\p{Nd}|[^` + pyWSChars + `:][^` + pyWSChars + `]*$)`)
	permissionDeniedRe    = regexp.MustCompile(`^(?P<tool>rg|grep|find): (?P<path>.+?): ` +
		`(?:IO error for operation on .+?: )?Permission denied(?: \(os error \p{Nd}+\))?$`)
	findQuotesRe         = regexp.MustCompile("^['\u2018]|['\u2019]$")
	omittedDiagnosticsRe = regexp.MustCompile(`^rg: \.\.\. (\p{Nd}+) more diagnostic line\(s\) omitted$`)
	contextLineSepRe     = regexp.MustCompile(`-(\p{Nd}+)-`)
	regexNewlineEscapeRe = regexp2.MustCompile(`(?<!\\)(?:\\\\)*\\n`)
	matchLineRe          = regexp.MustCompile(`^([A-Za-z]:)?(.*?):(\p{Nd}+):(.*)$`)
	windowsDriveRootRe   = regexp.MustCompile(`^[A-Za-z]:[\\/]`)
	findPathMetaRe       = regexp.MustCompile(`([*?\[\]])`)
)

// Engine flag of each non content output mode, identical for rg and grep
var outputModeFlags = map[string]string{"files_only": "-l", "count": "-c"}

// Code points Python str.isdigit accepts beyond the decimal digits
var pyDigitOnlyRanges = [][2]rune{
	{0xB2, 0xB3}, {0xB9, 0xB9}, {0x1369, 0x1371}, {0x19DA, 0x19DA}, {0x2070, 0x2070}, {0x2074, 0x2079},
	{0x2080, 0x2089}, {0x2460, 0x2468}, {0x2474, 0x247C}, {0x2488, 0x2490}, {0x24EA, 0x24EA},
	{0x24F5, 0x24FD}, {0x24FF, 0x24FF}, {0x2776, 0x277E}, {0x2780, 0x2788}, {0x278A, 0x2792},
	{0x10A40, 0x10A43}, {0x10E60, 0x10E68}, {0x11052, 0x1105A}, {0x1F100, 0x1F10A},
}

// Python str.isdigit over the whole string, false when empty
func pyIsDigitText(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if unicode.IsDigit(r) {
			continue
		}
		found := false
		for _, rg := range pyDigitOnlyRanges {
			if r >= rg[0] && r <= rg[1] {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

// Signed ASCII digits of a Python int literal in base ten, false where int would raise
func pyIntDigits(s string) (string, bool) {
	t := pyStrip(s)
	sign := ""
	if strings.HasPrefix(t, "+") || strings.HasPrefix(t, "-") {
		sign, t = t[:1], t[1:]
	}
	var b strings.Builder
	prevDigit := false
	for _, r := range t {
		if r == '_' {
			if !prevDigit {
				return "", false
			}
			prevDigit = false
			continue
		}
		if !unicode.IsDigit(r) {
			return "", false
		}
		b.WriteString(pyDigitsASCII(string(r)))
		prevDigit = true
	}
	if !prevDigit {
		return "", false
	}
	if sign == "+" {
		sign = ""
	}
	return sign + b.String(), true
}

// Python int of a decimal string as a machine int
func pyIntText(s string) (int, bool) {
	digits, ok := pyIntDigits(s)
	if !ok {
		return 0, false
	}
	n, err := strconv.Atoi(digits)
	return n, err == nil
}

// Python float of a string, hex spellings refused like float does
func pyFloatText(s string) (float64, bool) {
	t := pyStrip(s)
	if t == "" || strings.ContainsAny(t, "xX") {
		return 0, false
	}
	var b strings.Builder
	for _, r := range t {
		if r > 0x7f && unicode.IsDigit(r) {
			b.WriteString(pyDigitsASCII(string(r)))
			continue
		}
		b.WriteRune(r)
	}
	if body := strings.TrimLeft(strings.ToLower(t), "+-"); body == "nan" && len(t)-len(body) <= 1 {
		return math.NaN(), true
	}
	f, err := strconv.ParseFloat(b.String(), 64)
	if err != nil {
		if ne, ok := err.(*strconv.NumError); ok && ne.Err == strconv.ErrRange {
			return f, true
		}
		return 0, false
	}
	return f, true
}

// Python str.lstrip with no argument
func pyLStrip(s string) string { return strings.TrimLeftFunc(s, pyIsSpace) }

// Python list slice from offset spanning limit items, clamped to the bounds
func sliceWindow[T any](items []T, offset, limit int) []T {
	start := min(max(offset, 0), len(items))
	end := min(max(offset+limit, start), len(items))
	return items[start:end]
}

// Stdout cleaned of the timeout marker plus the search_timeout reason when the run timed out
func searchStdoutAndLimit(result *ExecuteResult) (string, string) {
	if result.ExitCode == 124 {
		return searchTimeoutMarkerRe.ReplaceAllString(result.Stdout, ""), "search_timeout"
	}
	return result.Stdout, ""
}

// Splits merged rg or grep output into diagnostic lines and real match lines by line shape
func splitToolDiagnostics(output string) (string, string) {
	var diagnostics, payload []string
	for _, line := range strings.Split(output, "\n") {
		if pyStrip(line) == "" {
			continue
		}
		switch {
		case hasAnyPrefix(pyLStrip(line), []string{"rg: ", "grep: ", "find: "}):
			diagnostics = append(diagnostics, line)
		case line == "--" || searchOutputRe.MatchString(line):
			payload = append(payload, line)
		default:
			diagnostics = append(diagnostics, line)
		}
	}
	return strings.Join(diagnostics, "\n"), strings.Join(payload, "\n")
}

// Tally line the native runner appends for stderr lines dropped past its retention cap
func omittedDiagnosticsLine(count int) string {
	return "rg: ... " + strconv.Itoa(count) + " more diagnostic line(s) omitted"
}

// Sum of every omitted diagnostics tally in the text
func omittedDiagnosticCount(diagnostics string) int {
	total := 0
	for _, line := range pySplitLines(diagnostics) {
		if m := omittedDiagnosticsRe.FindStringSubmatch(pyStrip(line)); m != nil {
			n, _ := pyIntText(m[1])
			total += n
		}
	}
	return total
}

// True when every kept diagnostic line reports an unreadable path, a partial success
func isPermissionOnlyError(diagnostics string) bool {
	var lines []string
	for _, line := range pySplitLines(diagnostics) {
		if l := pyStrip(line); l != "" && !omittedDiagnosticsRe.MatchString(l) {
			lines = append(lines, l)
		}
	}
	if len(lines) == 0 {
		return false
	}
	for _, l := range lines {
		if !permissionDeniedRe.MatchString(l) {
			return false
		}
	}
	return true
}

// Paths rg, grep or find reported unreadable, deduplicated in first seen order
func extractUnreadablePaths(diagnostics string) []string {
	var paths []string
	seen := map[string]bool{}
	for _, line := range pySplitLines(diagnostics) {
		m := permissionDeniedRe.FindStringSubmatch(pyStrip(line))
		if m == nil {
			continue
		}
		path := pyStrip(m[2])
		if m[1] == "find" {
			path = findQuotesRe.ReplaceAllString(path, "")
		}
		if path != "" && !seen[path] {
			seen[path] = true
			paths = append(paths, path)
		}
	}
	return paths
}

// First three paths joined with a more tally covering the rest and the omitted lines
func compactUnreadablePaths(paths []string, keep, omitted int) string {
	shown := strings.Join(paths[:min(keep, len(paths))], ", ")
	if extra := max(len(paths)-keep, 0) + omitted; extra > 0 {
		return shown + " (+" + strconv.Itoa(extra) + " more)"
	}
	return shown
}

// Sentence naming the unreadable paths a search skipped and how to include them
func skippedUnreadableWarning(paths []string, omitted int) string {
	return "Skipped " + strconv.Itoa(len(paths)+omitted) + " unreadable path(s) with restricted " +
		"permissions: " + compactUnreadablePaths(paths, 3, omitted) +
		". Results may be incomplete; fix permissions or run as a user that " +
		"can read them to include them."
}

// Unreadable paths and dropped diagnostic count a search result carries
type searchSkip struct {
	paths   []string
	omitted int
}

// Skip data from rg, grep or find diagnostics, empty when nothing was skipped
func skippedFields(diagnostics string) searchSkip {
	paths := extractUnreadablePaths(diagnostics)
	if len(paths) == 0 {
		return searchSkip{}
	}
	return searchSkip{paths: paths, omitted: omittedDiagnosticCount(diagnostics)}
}

// Copies the skip data into the result fields
func (sk searchSkip) apply(r *SearchResult) *SearchResult {
	r.SkippedPaths, r.SkippedOmitted = sk.paths, sk.omitted
	return r
}

// Skipped paths sentence rebuilt from the structured skip data, empty without skips
func skippedSentence(r *SearchResult) string {
	if len(r.SkippedPaths) == 0 {
		return ""
	}
	return skippedUnreadableWarning(r.SkippedPaths, r.SkippedOmitted)
}

// Skipped paths warning placed before any existing warning
func prependSkippedPathsWarning(skip searchSkip, existing string) string {
	if len(skip.paths) == 0 {
		return existing
	}
	warning := skippedUnreadableWarning(skip.paths, skip.omitted)
	if existing == "" {
		return warning
	}
	return warning + " " + existing
}

// Per root warnings combined into one skipped sentence plus the remaining deduplicated text
func mergeChildWarnings(children []*SearchResult, hasResults bool) []string {
	total := 0
	var shown, rest []string
	for _, child := range children {
		if child.Warning == "" {
			continue
		}
		warning := child.Warning
		if sentence := skippedSentence(child); sentence != "" {
			total += len(child.SkippedPaths) + child.SkippedOmitted
			for _, path := range child.SkippedPaths {
				if len(shown) >= 3 {
					break
				}
				if !containsString(shown, path) {
					shown = append(shown, path)
				}
			}
			warning = strings.ReplaceAll(warning, sentence, "\x00")
		}
		for _, piece := range strings.Split(warning, "\x00") {
			piece = pyStrip(piece)
			if hasResults {
				piece = pyStrip(strings.ReplaceAll(piece, permissionSearchHint, ""))
			}
			if piece != "" && !containsString(rest, piece) {
				rest = append(rest, piece)
			}
		}
	}
	if total == 0 {
		return rest
	}
	return append([]string{skippedUnreadableWarning(shown, max(total-len(shown), 0))}, rest...)
}

// Path, line number digits and content of a context line split at the rightmost numeric separator
func parseSearchContextLine(line string) (string, string, string, bool) {
	if line == "" || line == "--" {
		return "", "", "", false
	}
	all := contextLineSepRe.FindAllStringSubmatchIndex(line, -1)
	if len(all) == 0 || all[len(all)-1][0] == 0 {
		return "", "", "", false
	}
	m := all[len(all)-1]
	return line[:m[0]], line[m[2]:m[3]], line[m[1]:], true
}

// True when a content regex wants a newline, a literal one or an odd backslash run before n
func patternHasRegexNewline(pattern string) bool {
	if strings.Contains(pattern, "\n") {
		return true
	}
	ok, err := regexNewlineEscapeRe.MatchString(pattern)
	if err != nil {
		panic(err)
	}
	return ok
}

// True for the rg hard error demanding multiline mode
func isLineOrientedNewlineError(errText string) bool {
	return errText != "" && strings.Contains(errText, `literal "\n" is not allowed`) && strings.Contains(errText, "--multiline")
}

// Newline regex warning attached only when the line oriented search found nothing usable
func maybeWarnLineOrientedNewlinePattern(result *SearchResult, pattern string) *SearchResult {
	if result.TotalCount != 0 || !patternHasRegexNewline(pattern) {
		return result
	}
	if result.Error != "" && !isLineOrientedNewlineError(result.Error) {
		return result
	}
	result.Error = ""
	result.SkippedPaths, result.SkippedOmitted = nil, 0
	result.Warning = "0 results found. Note: search_files content search is line-oriented " +
		"and does not run ripgrep with -U/--multiline, so `\\n` in the regex " +
		"does not match line breaks. Use context=N to inspect neighboring " +
		"lines, or escape as `\\\\n` when searching for a literal backslash+n."
	return result
}

// Search result from bounded rg or grep output, failing only when an exit 2 left no payload
func parseSearchOutput(result *ExecuteResult, outputMode string, limit, offset, context int, warning string) *SearchResult {
	stdout, limitReason := searchStdoutAndLimit(result)
	diagnostics, payload := splitToolDiagnostics(stdout)
	if result.ExitCode == 126 || result.ExitCode == 127 {
		msg := pyStrip(result.Stdout)
		if msg == "" {
			msg = "Search error"
		}
		return &SearchResult{Error: "Search failed: " + msg}
	}
	if result.ExitCode == 2 && pyStrip(payload) == "" {
		errorMsg := pyStrip(diagnostics)
		if errorMsg == "" {
			errorMsg = pyStrip(result.Stdout)
		}
		if errorMsg == "" {
			errorMsg = "Search error"
		}
		if isPermissionOnlyError(errorMsg) {
			skip := skippedFields(errorMsg)
			warning = prependSkippedPathsWarning(skip, warning)
			if warning == "" {
				warning = permissionSearchHint
			} else if !strings.Contains(warning, permissionSearchHint) {
				warning += " " + permissionSearchHint
			}
			return skip.apply(&SearchResult{Warning: warning})
		}
		return &SearchResult{Error: "Search failed: " + errorMsg}
	}
	skip := skippedFields(diagnostics)
	skippedWarning := prependSkippedPathsWarning(skip, "")
	var otherErrors []string
	for _, ln := range pySplitLines(diagnostics) {
		l := pyStrip(ln)
		if hasAnyPrefix(pyLStrip(ln), []string{"rg: ", "grep: "}) && !permissionDeniedRe.MatchString(l) && !omittedDiagnosticsRe.MatchString(l) {
			otherErrors = append(otherErrors, l)
		}
	}
	if result.ExitCode == 2 && len(otherErrors) > 0 {
		errorWarning := "The search reported errors for some paths; results may be incomplete (" + otherErrors[0] + ")."
		if skippedWarning != "" {
			skippedWarning = errorWarning + " " + skippedWarning
		} else {
			skippedWarning = errorWarning
		}
	}
	if skippedWarning != "" {
		if warning == "" {
			warning = skippedWarning
		} else {
			warning = skippedWarning + " " + warning
		}
	}
	var lines []string
	for _, ln := range strings.Split(pyStrip(payload), "\n") {
		if ln != "" {
			lines = append(lines, ln)
		}
	}
	truncatedByLimit := limitReason != ""
	switch outputMode {
	case "files_only":
		return skip.apply(&SearchResult{Files: sliceWindow(lines, offset, limit), TotalCount: len(lines),
			Truncated: truncatedByLimit, LimitReason: limitReason, Warning: warning})
	case "count":
		r := &SearchResult{Truncated: truncatedByLimit, LimitReason: limitReason, Warning: warning}
		for _, line := range lines {
			i := strings.LastIndex(line, ":")
			if i < 0 {
				continue
			}
			if n, ok := pyIntText(line[i+1:]); ok {
				r.setCount(line[:i], n)
			}
		}
		for _, c := range r.Counts {
			r.TotalCount += c.Count
		}
		return skip.apply(r)
	}
	var matches []SearchMatch
	for _, line := range lines {
		if line == "--" {
			continue
		}
		if m := matchLineRe.FindStringSubmatch(line); m != nil {
			matches = append(matches, newSearchMatch(m[1]+m[2], m[3], runePrefix(m[4], 500)))
			continue
		}
		if context > 0 {
			if path, digits, content, ok := parseSearchContextLine(line); ok {
				matches = append(matches, newSearchMatch(path, digits, runePrefix(content, 500)))
			}
		}
	}
	total := len(matches)
	return skip.apply(&SearchResult{Matches: sliceWindow(matches, offset, limit), TotalCount: total,
		Truncated: total > offset+limit || truncatedByLimit, LimitReason: limitReason, Warning: warning})
}

// True on a macOS controller when every root is POSIX shaped
func posixRoots(roots []string) bool {
	if searchHostPlatform != "darwin" {
		return false
	}
	for _, root := range roots {
		if windowsDriveRootRe.MatchString(root) || strings.Contains(root, `\`) {
			return false
		}
	}
	return true
}

// Each root with its glob metacharacters escaped for a literal find path test
func findLiteralPathExpressions(roots []string) []string {
	out := make([]string, len(roots))
	for i, root := range roots {
		out[i] = findPathMetaRe.ReplaceAllString(root, `\$1`)
	}
	return out
}

// Big integer sum helper for match tallies that Python keeps unbounded
func addPyInt(total *big.Int, digits string) {
	n, _ := new(big.Int).SetString(digits, 10)
	total.Add(total, n)
}

// Content regex or glob filename lookup with path probing, multi path recovery and macOS notes
func (s *shellFileOps) search(pattern, path, target string, fileGlob string, limit, offset int, outputMode string, context int, order string) *SearchResult {
	offset, limit = normalizeSearchPagination(offset, limit)
	if target == "files" && order != "discovery" && order != "modified" {
		return &SearchResult{Error: "Invalid file search order " + pyReprStr(order) + "; expected " +
			"'discovery' or 'modified'."}
	}
	path = s.expandPath(path)
	probe := s.pathExistsProbe(path)
	if probe.CwdError != "" {
		return &SearchResult{Error: probe.CwdError}
	}
	existsProbe := probe.Stdout
	if !strings.Contains(existsProbe, "exists") && !strings.Contains(existsProbe, "not_found") {
		return &SearchResult{Error: "Terminal environment unavailable: could not stat " + path + " " +
			"(the sandbox may still be starting or was removed). Retry shortly."}
	}
	if strings.Contains(existsProbe, "not_found") {
		if multi := s.tryMultiPathSearch(pattern, path, target, fileGlob, limit, offset, outputMode, context, order); multi != nil {
			return multi
		}
		return s.pathNotFoundResult(path)
	}
	result := s.dispatchSearch(pattern, path, target, fileGlob, limit, offset, outputMode, context, order)
	if exclusions := s.macosSearchExclusions(path); len(exclusions) > 0 && result.Error == "" {
		names := make([]string, len(exclusions))
		for i, item := range exclusions {
			parts := strings.Split(item, "/")
			names[i] = parts[len(parts)-1]
		}
		protected := "Skipped macOS protected folders during broad search to avoid " +
			"an unattended privacy prompt: " + strings.Join(names, ", ") + ". Search a protected " +
			"folder directly when access is intentional."
		if result.Warning != "" {
			result.Warning += " " + protected
		} else {
			result.Warning = protected
		}
	}
	return result
}

// Routes to the filename search or the content search
func (s *shellFileOps) dispatchSearch(pattern, path, target, fileGlob string, limit, offset int, outputMode string, context int, order string) *SearchResult {
	if target == "files" {
		return s.searchFiles(pattern, []string{path}, false, limit, offset, order)
	}
	return s.searchContent(pattern, path, fileGlob, limit, offset, outputMode, context)
}

// Missing root error with up to five similar entries from its parent directory
func (s *shellFileOps) pathNotFoundResult(path string) *SearchResult {
	parent := pathDirnameOS(path)
	if parent == "" {
		parent = "."
	}
	basenameQuery := pathBasenameOS(path)
	hintParts := []string{"Path not found: " + path}
	parentCheck := s.exec("test -d "+s.escapeShellArg(parent, true)+" && echo yes || echo no", "", 0)
	if strings.Contains(parentCheck.Stdout, "yes") && basenameQuery != "" {
		ls := s.exec("ls -1 "+s.escapeShellArg(parent, true)+" 2>/dev/null | head -20", "", 0)
		if ls.ExitCode == 0 && pyStrip(ls.Stdout) != "" {
			lq := pyLower(basenameQuery)
			var candidates []string
			for _, e := range strings.Split(pyStrip(ls.Stdout), "\n") {
				le := pyLower(e)
				if e != "" && (strings.Contains(le, lq) || strings.Contains(lq, le) || strings.HasPrefix(le, runePrefix(lq, 3))) {
					candidates = append(candidates, pathJoinOS(parent, e))
				}
			}
			if len(candidates) > 0 {
				hintParts = append(hintParts, "Similar paths: "+strings.Join(candidates[:min(5, len(candidates))], ", "))
			}
		}
	}
	return &SearchResult{Error: strings.Join(hintParts, ". ")}
}

// Searches every existing part of a path string holding several paths, nil when it is not one
func (s *shellFileOps) tryMultiPathSearch(pattern, path, target, fileGlob string, limit, offset int, outputMode string, context int, order string) *SearchResult {
	var parts []string
	if strings.Contains(path, ",") {
		for _, part := range strings.Split(path, ",") {
			if p := pyStrip(part); p != "" {
				parts = append(parts, p)
			}
		}
	} else {
		parts = pyFields(path)
	}
	if len(parts) < 2 {
		return nil
	}
	var existing, missing []string
	for _, p := range parts {
		expanded := s.expandPath(p)
		if strings.Contains(s.pathExistsProbe(expanded).Stdout, "exists") {
			existing = append(existing, expanded)
		} else {
			missing = append(missing, expanded)
		}
	}
	if len(existing) == 0 {
		return nil
	}
	var merged *SearchResult
	var children []*SearchResult
	if target == "files" {
		merged = s.searchFiles(pattern, existing, true, limit, offset, order)
		children = []*SearchResult{merged}
	} else {
		merged = &SearchResult{}
		for _, root := range existing {
			sub := s.searchContent(pattern, root, fileGlob, limit, offset, outputMode, context)
			if sub.Error != "" {
				return sub
			}
			children = append(children, sub)
			merged.Matches = append(merged.Matches, sub.Matches...)
			merged.Files = append(merged.Files, sub.Files...)
			merged.updateCounts(sub.Counts)
			merged.TotalCount += sub.TotalCount
			merged.Truncated = merged.Truncated || sub.Truncated
		}
		merged.Matches = sliceWindow(merged.Matches, 0, limit)
		merged.Files = sliceWindow(merged.Files, 0, limit)
	}
	note := fmt.Sprintf("path contained %d entries; searched %d that exist", len(parts), len(existing))
	if len(missing) > 0 {
		note += "; skipped missing: " + strings.Join(missing[:min(3, len(missing))], ", ")
		if len(missing) > 3 {
			note += fmt.Sprintf(" (+%d more)", len(missing)-3)
		}
	}
	hasResults := merged.TotalCount != 0 || len(merged.Matches) > 0 || len(merged.Files) > 0 || len(merged.Counts) > 0
	warningParts := append(mergeChildWarnings(children, hasResults), note)
	if merged.Error == "" {
		var protected []string
		for _, ex := range s.effectiveMacosSearchExclusions(existing) {
			protected = append(protected, ex.absolute)
		}
		if len(protected) > 0 {
			warningParts = append(warningParts, macosProtectedSearchWarning(protected))
		}
	}
	merged.Warning = strings.Join(warningParts, " ")
	return merged
}
