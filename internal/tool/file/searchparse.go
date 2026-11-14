package file

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

const (
	limitTimeout               = "search_timeout"
	stderrRetainBytes          = 64 * 1024
	permissionSearchHint       = "Nothing matched in the readable part of the search scope."
	admissionInterruptedError  = "File search was interrupted while waiting for another filename search on the same root. Retry when ready."
	findModifiedOrderError     = "Exact modification-time order requires GNU find with -printf support; install ripgrep 14+ or use order='discovery'."
	ripgrepModifiedOrderError  = "Exact modification-time order requires ripgrep 14 or newer; upgrade ripgrep or use order='discovery'."
	findTraversalFailure       = "File search failed while running bounded find traversal"
	partialFindTraversalNotice = "find traversal skipped one or more unreadable paths (restricted permissions); results are partial."
)

var (
	timeoutMarker      = regexp.MustCompile(`\n?\[Command timed out after \d+s\][` + spaceClass + `]*$`)
	searchOutputShape  = regexp.MustCompile(`^(?:([A-Za-z]:)?[^` + spaceClass + `:][^\n]*?[:\-]\d|[^` + spaceClass + `:][^` + spaceClass + `]*$)`)
	permissionDenied   = regexp.MustCompile(`^(?P<tool>rg|grep|find): (?P<path>.+?): (?:IO error for operation on .+?: )?Permission denied(?: \(os error \d+\))?$`)
	omittedDiagnostics = regexp.MustCompile(`^rg: \.\.\. (\d+) more diagnostic line\(s\) omitted$`)
	contextSeparator   = regexp.MustCompile(`-(\d+)-`)
	matchLine          = regexp.MustCompile(`^([A-Za-z]:)?(.*?):(\d+):(.*)$`)
	drivePath          = regexp.MustCompile(`^[A-Za-z]:[\\/]`)
	plainExecutable    = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)
	regexMeta          = regexp.MustCompile(`[.\[\](){}?*+^$\\|]`)
	globMeta           = regexp.MustCompile(`([*?\[\]])`)
	ripgrepVersion     = regexp.MustCompile(`(?m)^ripgrep\s+((?:0|[1-9]\d*))\.` +
		`(?:0|[1-9]\d*)\.(?:0|[1-9]\d*)` +
		`(?:-(?:(?:0|[1-9]\d*)|(?:[0-9A-Za-z-]*[A-Za-z-]` +
		`[0-9A-Za-z-]*))(?:\.(?:(?:0|[1-9]\d*)|` +
		`(?:[0-9A-Za-z-]*[A-Za-z-][0-9A-Za-z-]*)))*)?` +
		`(?:\+[0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*)?` +
		`(?:\s+\(rev [^)]+\))?\s*$`)
)

func searchStdoutAndLimit(res execResult) (string, string) {
	if res.exitCode == 124 {
		return timeoutMarker.ReplaceAllString(res.stdout, ""), limitTimeout
	}
	return res.stdout, ""
}

func hasToolPrefix(line string, tools ...string) bool {
	trimmed := trimLeftSpace(line)
	for _, t := range tools {
		if strings.HasPrefix(trimmed, t+": ") {
			return true
		}
	}
	return false
}

func splitToolDiagnostics(output string) (string, string) {
	var diagnostics, payload []string
	for _, line := range strings.Split(output, "\n") {
		if isBlank(line) {
			continue
		}
		switch {
		case hasToolPrefix(line, "rg", "grep", "find"):
			diagnostics = append(diagnostics, line)
		case line == "--" || searchOutputShape.MatchString(line):
			payload = append(payload, line)
		default:
			diagnostics = append(diagnostics, line)
		}
	}
	return strings.Join(diagnostics, "\n"), strings.Join(payload, "\n")
}

func omittedDiagnosticsLine(count int) string {
	return fmt.Sprintf("rg: ... %d more diagnostic line(s) omitted", count)
}

func omittedDiagnosticCount(diagnostics string) int {
	total := 0
	for _, line := range splitLines(diagnostics, false) {
		if m := omittedDiagnostics.FindStringSubmatch(trimSpace(line)); m != nil {
			n, _ := strconv.Atoi(m[1])
			total += n
		}
	}
	return total
}

func permissionOnlyError(diagnostics string) bool {
	seen := false
	for _, line := range splitLines(diagnostics, false) {
		s := trimSpace(line)
		if s == "" || omittedDiagnostics.MatchString(s) {
			continue
		}
		if !permissionDenied.MatchString(s) {
			return false
		}
		seen = true
	}
	return seen
}

func trimFindQuotes(path string) string {
	for _, q := range [...]string{"'", string(rune(0x2018))} {
		if strings.HasPrefix(path, q) {
			path = path[len(q):]
			break
		}
	}
	for _, q := range [...]string{"'", string(rune(0x2019))} {
		if strings.HasSuffix(path, q) {
			path = path[:len(path)-len(q)]
			break
		}
	}
	return path
}

func unreadablePaths(diagnostics string) []string {
	var paths []string
	seen := make(map[string]struct{})
	for _, line := range splitLines(diagnostics, false) {
		m := permissionDenied.FindStringSubmatch(trimSpace(line))
		if m == nil {
			continue
		}
		p := trimSpace(m[2])
		if m[1] == "find" {
			p = trimFindQuotes(p)
		}
		if p == "" {
			continue
		}
		if _, dup := seen[p]; !dup {
			seen[p] = struct{}{}
			paths = append(paths, p)
		}
	}
	return paths
}

func skippedUnreadableWarning(paths []string, omitted int) string {
	shown := paths
	if len(shown) > 3 {
		shown = shown[:3]
	}
	compact := strings.Join(shown, ", ")
	if extra := max(len(paths)-3, 0) + omitted; extra > 0 {
		compact += fmt.Sprintf(" (+%d more)", extra)
	}
	return "Skipped " + strconv.Itoa(len(paths)+omitted) + " unreadable path(s) with restricted " +
		"permissions: " + compact +
		". Results may be incomplete; fix permissions or run as a user that " +
		"can read them to include them."
}

type skipped struct {
	paths   []string
	omitted int
}

func skippedFields(diagnostics string) skipped {
	paths := unreadablePaths(diagnostics)
	if len(paths) == 0 {
		return skipped{}
	}
	return skipped{paths: paths, omitted: omittedDiagnosticCount(diagnostics)}
}

func prependSkippedWarning(skip skipped, existing string) string {
	if len(skip.paths) == 0 {
		return existing
	}
	warning := skippedUnreadableWarning(skip.paths, skip.omitted)
	if existing == "" {
		return warning
	}
	return warning + " " + existing
}

func mergeChildWarnings(children []SearchResult, hasResults bool) []string {
	total := 0
	var shown, rest []string
	contains := func(list []string, s string) bool {
		for _, x := range list {
			if x == s {
				return true
			}
		}
		return false
	}
	for _, child := range children {
		if child.Warning == "" {
			continue
		}
		pieces := []string{child.Warning}
		if len(child.SkippedPaths) > 0 {
			sentence := skippedUnreadableWarning(child.SkippedPaths, child.SkippedOmitted)
			total += len(child.SkippedPaths) + child.SkippedOmitted
			for _, p := range child.SkippedPaths {
				if len(shown) >= 3 {
					break
				}
				if !contains(shown, p) {
					shown = append(shown, p)
				}
			}
			pieces = strings.Split(child.Warning, sentence)
		}
		for _, piece := range pieces {
			piece = trimSpace(piece)
			if hasResults {
				piece = trimSpace(strings.ReplaceAll(piece, permissionSearchHint, ""))
			}
			if piece != "" && !contains(rest, piece) {
				rest = append(rest, piece)
			}
		}
	}
	if total == 0 {
		return rest
	}
	return append([]string{skippedUnreadableWarning(shown, max(total-len(shown), 0))}, rest...)
}

func splitFindOutput(stdout, order string) ([]string, []string) {
	var files, diagnostics []string
	for _, line := range splitLines(stdout, false) {
		switch {
		case strings.HasPrefix(line, "find: "):
			diagnostics = append(diagnostics, line)
		case order == "modified":
			stamp, rest, found := strings.Cut(line, " ")
			if found && allDigits(strings.Replace(stamp, ".", "", 1)) {
				files = append(files, rest)
			}
		case line != "":
			files = append(files, line)
		}
	}
	return files, diagnostics
}

func findExitVerdict(exitCode int, files, diagnostics []string, fetchLimit int, order string) (*SearchResult, string) {
	boundedPipe := exitCode == 141 && len(files) >= fetchLimit
	if exitCode != 0 && exitCode != 1 && exitCode != 124 && !boundedPipe {
		if order == "modified" {
			return &SearchResult{Error: findModifiedOrderError}, ""
		}
		return &SearchResult{Error: findTraversalFailure + "."}, ""
	}
	diagnosticText := strings.Join(diagnostics, "\n")
	skippedPaths := unreadablePaths(diagnosticText)
	warning := ""
	if len(skippedPaths) > 0 {
		warning = skippedUnreadableWarning(skippedPaths, 0)
	}
	if exitCode != 1 {
		return nil, warning
	}
	if len(files) == 0 && permissionOnlyError(diagnosticText) {
		return &SearchResult{Warning: warning + " " + permissionSearchHint, SkippedPaths: skippedPaths}, ""
	}
	if len(files) == 0 {
		if order == "modified" {
			return &SearchResult{Error: findModifiedOrderError}, ""
		}
		detail := "."
		if len(diagnostics) > 0 {
			detail = ": " + diagnostics[0]
		}
		return &SearchResult{Error: findTraversalFailure + detail}, ""
	}
	for _, d := range diagnostics {
		if s := trimSpace(d); s != "" && !permissionDenied.MatchString(s) {
			notice := "Some paths could not be read: " + s + ". Results may be incomplete."
			if warning != "" {
				notice += " " + warning
			}
			return nil, notice
		}
	}
	if warning != "" {
		return nil, warning
	}
	return nil, partialFindTraversalNotice
}

func parseContextLine(line string) (string, int, string, bool) {
	if line == "" || line == "--" {
		return "", 0, "", false
	}
	all := contextSeparator.FindAllStringSubmatchIndex(line, -1)
	if len(all) == 0 {
		return "", 0, "", false
	}
	m := all[len(all)-1]
	if m[0] == 0 {
		return "", 0, "", false
	}
	n, err := strconv.Atoi(line[m[2]:m[3]])
	if err != nil {
		return "", 0, "", false
	}
	return line[:m[0]], n, line[m[1]:], true
}

func patternHasRegexNewline(pattern string) bool {
	if strings.Contains(pattern, "\n") {
		return true
	}
	for i := 0; i < len(pattern); {
		if pattern[i] != '\\' {
			i++
			continue
		}
		j := i
		for j < len(pattern) && pattern[j] == '\\' {
			j++
		}
		if j < len(pattern) && pattern[j] == 'n' && (j-i)%2 == 1 {
			return true
		}
		i = j
	}
	return false
}

func lineOrientedNewlineError(errText string) bool {
	return errText != "" && strings.Contains(errText, `literal "\n" is not allowed`) && strings.Contains(errText, "--multiline")
}

func warnLineOrientedNewline(result SearchResult, pattern string) SearchResult {
	if result.TotalCount != 0 || !patternHasRegexNewline(pattern) {
		return result
	}
	if result.Error != "" && !lineOrientedNewlineError(result.Error) {
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

func outputModeFlag(mode string) (string, bool) {
	switch mode {
	case "files_only":
		return "-l", true
	case "count":
		return "-c", true
	}
	return "", false
}

func page[T any](items []T, offset, limit int) []T {
	if offset >= len(items) {
		return nil
	}
	return items[offset:min(len(items), offset+limit)]
}

func parseSearchOutput(res execResult, outputMode string, limit, offset, context int, warning string) SearchResult {
	stdout, limitReason := searchStdoutAndLimit(res)
	diagnostics, payload := splitToolDiagnostics(stdout)
	raw := trimSpace(res.stdout)
	if raw == "" {
		raw = "Search error"
	}
	if res.exitCode == 126 || res.exitCode == 127 {
		return SearchResult{Error: "Search failed: " + raw}
	}
	if res.exitCode == 2 && isBlank(payload) {
		errMsg := trimSpace(diagnostics)
		if errMsg == "" {
			errMsg = raw
		}
		if permissionOnlyError(errMsg) {
			skip := skippedFields(errMsg)
			warning = prependSkippedWarning(skip, warning)
			if warning == "" {
				warning = permissionSearchHint
			} else if !strings.Contains(warning, permissionSearchHint) {
				warning += " " + permissionSearchHint
			}
			return SearchResult{Warning: warning, SkippedPaths: skip.paths, SkippedOmitted: skip.omitted}
		}
		return SearchResult{Error: "Search failed: " + errMsg}
	}
	skip := skippedFields(diagnostics)
	skippedWarning := prependSkippedWarning(skip, "")
	if res.exitCode == 2 {
		for _, line := range splitLines(diagnostics, false) {
			s := trimSpace(line)
			if hasToolPrefix(line, "rg", "grep") && !permissionDenied.MatchString(s) && !omittedDiagnostics.MatchString(s) {
				errorWarning := "The search reported errors for some paths; results may be incomplete (" + s + ")."
				if skippedWarning != "" {
					skippedWarning = errorWarning + " " + skippedWarning
				} else {
					skippedWarning = errorWarning
				}
				break
			}
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
	for _, line := range strings.Split(trimSpace(payload), "\n") {
		if line != "" {
			lines = append(lines, line)
		}
	}
	base := SearchResult{LimitReason: limitReason, Warning: warning, SkippedPaths: skip.paths, SkippedOmitted: skip.omitted}
	switch outputMode {
	case "files_only":
		base.Files = page(lines, offset, limit)
		base.TotalCount = len(lines)
		base.Truncated = limitReason != ""
		return base
	case "count":
		index := make(map[string]int)
		for _, line := range lines {
			cut := strings.LastIndex(line, ":")
			if cut < 0 {
				continue
			}
			n, ok := parseInt(line[cut+1:])
			if !ok {
				continue
			}
			p := line[:cut]
			if i, seen := index[p]; seen {
				base.Counts[i].Count = n
				continue
			}
			index[p] = len(base.Counts)
			base.Counts = append(base.Counts, PathCount{Path: p, Count: n})
		}
		for _, c := range base.Counts {
			base.TotalCount += c.Count
		}
		base.Truncated = limitReason != ""
		return base
	}
	var matches []SearchMatch
	for _, line := range lines {
		if line == "--" {
			continue
		}
		if m := matchLine.FindStringSubmatch(line); m != nil {
			n, _ := strconv.Atoi(m[3])
			matches = append(matches, SearchMatch{Path: m[1] + m[2], Line: n, Content: charPrefix(m[4], 500)})
			continue
		}
		if context > 0 {
			if p, n, content, ok := parseContextLine(line); ok {
				matches = append(matches, SearchMatch{Path: p, Line: n, Content: charPrefix(content, 500)})
			}
		}
	}
	total := len(matches)
	base.Matches = page(matches, offset, limit)
	base.TotalCount = total
	base.Truncated = total > offset+limit || limitReason != ""
	return base
}

func findLiteralPathExpressions(roots []string) []string {
	out := make([]string, len(roots))
	for i, root := range roots {
		out[i] = globMeta.ReplaceAllString(root, `\$1`)
	}
	return out
}

func macProtectedWarning(paths []string, style pathStyle) string {
	names := make([]string, len(paths))
	for i, p := range paths {
		names[i] = style.base(p)
	}
	return "Skipped macOS protected folders during broad search to avoid " +
		"an unattended privacy prompt: " + strings.Join(names, ", ") + ". Search a protected " +
		"folder directly when access is intentional."
}
