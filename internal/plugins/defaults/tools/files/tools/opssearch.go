package tools

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/nickheyer/icebath/internal/terminal"
	"github.com/nickheyer/icebath/internal/tools/diff"
	"github.com/nickheyer/icebath/pkg/strs"
)

// Folders of dependencies, builds, caches, version control and backups that broad scans skip
var pruned = []string{".archive", ".backups", ".cache", ".git", ".gradle", ".hg", ".m2", ".mypy_cache", ".next", ".nox", ".npm", ".nuget", ".nuxt",
	".parcel-cache", ".pnpm-store", ".pytest_cache", ".ruff_cache", ".svelte-kit", ".svn", ".tox", ".Trash", ".turbo", ".venv", ".yarn", "__pycache__",
	"backup", "backups", "build", "coverage", "dist", "dist-packages", "node_modules", "out", "site-packages", "target", "third_party", "vendor", "venv"}

// Home folders macOS guards with privacy prompts that a broad search must not walk into
var protectedHome = []string{"Desktop", "Documents", "Downloads", "Library", "Movies", "Music", "Pictures"}

const (
	walkInterrupted     = "File search was interrupted while waiting for another filename search on the same root. Retry when ready."
	permissionHint      = "Nothing matched in the readable part of the search scope."
	findOrderError      = "Exact modification-time order requires GNU find with -printf support; install ripgrep 14+ or use order='discovery'."
	stderrKept          = 64 * 1024
	contextFetchExtra   = 200
	searchTimeoutMarker = "search_timeout"
)

var (
	spaceChars       = space[1 : len(space)-1]
	timeoutMarker    = regexp.MustCompile(`\n?\[Command timed out after [0-9]+s\]` + space + `*$`)
	searchLine       = regexp.MustCompile(`^(?:([A-Za-z]:)?[^` + spaceChars + `:][^\n]*?[:\-][0-9]|[^` + spaceChars + `:][^` + spaceChars + `]*$)`)
	permissionDenied = regexp.MustCompile(`^(rg|grep|find): (.+?): (?:IO error for operation on .+?: )?Permission denied(?: \(os error [0-9]+\))?$`)
	findQuotes       = regexp.MustCompile("^['‘]|['’]$")
	omittedLine      = regexp.MustCompile(`^rg: \.\.\. ([0-9]+) more diagnostic line\(s\) omitted$`)
	contextSeparator = regexp.MustCompile(`-([0-9]+)-`)
	matchLine        = regexp.MustCompile(`^([A-Za-z]:)?(.*?):([0-9]+):(.*)$`)
	driveRoot        = regexp.MustCompile(`^[A-Za-z]:[\\/]`)
	findMeta         = regexp.MustCompile(`([*?\[\]])`)
	regexMeta        = regexp.MustCompile(`[.\[\](){}?*+^$\\|]`)
	rgVersion        = regexp.MustCompile(`(?m)^ripgrep` + space + `+(0|[1-9][0-9]*)\.(?:0|[1-9][0-9]*)\.(?:0|[1-9][0-9]*)(?:-[0-9A-Za-z.-]+)?(?:\+[0-9A-Za-z.-]+)?(?:` +
		space + `+\(rev [^)]+\))?` + space + `*$`)
)

// Flag of each output mode that is not the matching lines, the same for rg and grep
var modeFlags = map[string]string{"files_only": "-l", "count": "-c"}

// Number text without its leading zeros
func lineNumber(digits string) string {
	if trimmed := strings.TrimLeft(digits, "0"); trimmed != "" {
		return trimmed
	}
	return "0"
}

// Items from offset, at most limit of them
func window[T any](items []T, offset, limit int) []T {
	start := min(max(offset, 0), len(items))
	return items[start:min(max(offset+limit, start), len(items))]
}

// Output without the marker of a run that timed out, and the reason the results stopped when it did
func timedOut(result run) (string, string) {
	if result.code == 124 {
		return timeoutMarker.ReplaceAllString(result.output, ""), searchTimeoutMarker
	}
	return result.output, ""
}

func lstrip(s string) string { return strings.TrimLeft(s, " \t\n\r\v\f") }

// Diagnostic lines and match lines of merged output, told apart by their shape
func splitDiagnostics(output string) (string, string) {
	var diagnostics, payload []string
	for _, line := range strings.Split(output, "\n") {
		switch {
		case strings.TrimSpace(line) == "":
		case strings.HasPrefix(lstrip(line), "rg: ") || strings.HasPrefix(lstrip(line), "grep: ") || strings.HasPrefix(lstrip(line), "find: "):
			diagnostics = append(diagnostics, line)
		case line == "--" || searchLine.MatchString(line):
			payload = append(payload, line)
		default:
			diagnostics = append(diagnostics, line)
		}
	}
	return strings.Join(diagnostics, "\n"), strings.Join(payload, "\n")
}

// Diagnostic lines the direct runner left out, summed over its tallies
func omittedCount(diagnostics string) int {
	total := 0
	for _, line := range diff.Split(diagnostics) {
		if m := omittedLine.FindStringSubmatch(strings.TrimSpace(line)); m != nil {
			n, _ := strconv.Atoi(m[1])
			total += n
		}
	}
	return total
}

// Whether every diagnostic line reports an unreadable path, a partial success
func permissionOnly(diagnostics string) bool {
	lines := 0
	for _, line := range diff.Split(diagnostics) {
		l := strings.TrimSpace(line)
		if l == "" || omittedLine.MatchString(l) {
			continue
		}
		if !permissionDenied.MatchString(l) {
			return false
		}
		lines++
	}
	return lines > 0
}

// Paths the diagnostics say could not be read, once each in the order seen
func unreadable(diagnostics string) []string {
	var paths []string
	for _, line := range diff.Split(diagnostics) {
		m := permissionDenied.FindStringSubmatch(strings.TrimSpace(line))
		if m == nil {
			continue
		}
		p := strings.TrimSpace(m[2])
		if m[1] == "find" {
			p = findQuotes.ReplaceAllString(p, "")
		}
		if p != "" && !slices.Contains(paths, p) {
			paths = append(paths, p)
		}
	}
	return paths
}

// Sentence naming the unreadable paths a search skipped, the first three named and the rest counted
func skippedWarning(paths []string, omitted int) string {
	shown := strings.Join(paths[:min(3, len(paths))], ", ")
	if extra := max(len(paths)-3, 0) + omitted; extra > 0 {
		shown += " (+" + strconv.Itoa(extra) + " more)"
	}
	return "Skipped " + strconv.Itoa(len(paths)+omitted) + " unreadable path(s) with restricted permissions: " + shown + ". Results may be " +
		"incomplete; fix permissions or run as a user that can read them to include them."
}

// Unreadable paths of a search and how many more the diagnostics left out
type skip struct {
	paths   []string
	omitted int
}

func skipOf(diagnostics string) skip {
	paths := unreadable(diagnostics)
	if len(paths) == 0 {
		return skip{}
	}
	return skip{paths, omittedCount(diagnostics)}
}

func (s skip) onto(r *searchResult) *searchResult {
	r.skipped, r.skippedOmitted = s.paths, s.omitted
	return r
}

// Warning with the skipped paths sentence put first
func (s skip) before(existing string) string {
	if len(s.paths) == 0 {
		return existing
	}
	if existing == "" {
		return skippedWarning(s.paths, s.omitted)
	}
	return skippedWarning(s.paths, s.omitted) + " " + existing
}

// Warnings of the searches of several roots as one skipped sentence then the other warnings once each
func mergeWarnings(children []*searchResult, found bool) []string {
	total := 0
	var shown, rest []string
	for _, child := range children {
		if child.warning == "" {
			continue
		}
		warning := child.warning
		if len(child.skipped) > 0 {
			sentence := skippedWarning(child.skipped, child.skippedOmitted)
			total += len(child.skipped) + child.skippedOmitted
			for _, p := range child.skipped {
				if len(shown) < 3 && !slices.Contains(shown, p) {
					shown = append(shown, p)
				}
			}
			warning = strings.ReplaceAll(warning, sentence, "\x00")
		}
		for _, piece := range strings.Split(warning, "\x00") {
			piece = strings.TrimSpace(piece)
			if found {
				piece = strings.TrimSpace(strings.ReplaceAll(piece, permissionHint, ""))
			}
			if piece != "" && !slices.Contains(rest, piece) {
				rest = append(rest, piece)
			}
		}
	}
	if total == 0 {
		return rest
	}
	return append([]string{skippedWarning(shown, max(total-len(shown), 0))}, rest...)
}

// Path, line number and content of a context line, split at its rightmost -N- separator
func contextLine(line string) (string, string, string, bool) {
	if line == "" || line == "--" {
		return "", "", "", false
	}
	all := contextSeparator.FindAllStringSubmatchIndex(line, -1)
	if len(all) == 0 || all[len(all)-1][0] == 0 {
		return "", "", "", false
	}
	m := all[len(all)-1]
	return line[:m[0]], line[m[2]:m[3]], line[m[1]:], true
}

// Whether a content regex wants a line break: a newline in it, or \n after an even run of backslashes
func wantsNewline(pattern string) bool {
	if strings.Contains(pattern, "\n") {
		return true
	}
	for i := 0; i+1 < len(pattern); i++ {
		if pattern[i] != '\\' {
			continue
		}
		j := i
		for j < len(pattern) && pattern[j] == '\\' {
			j++
		}
		if (j-i)%2 == 1 && j < len(pattern) && pattern[j] == 'n' {
			return true
		}
		i = j - 1
	}
	return false
}

// Note for a line oriented search that found nothing for a regex wanting a line break, which it can never match
func newlineNote(r *searchResult, pattern string) *searchResult {
	if r.total != 0 || !wantsNewline(pattern) {
		return r
	}
	if r.err != "" && !(strings.Contains(r.err, `literal "\n" is not allowed`) && strings.Contains(r.err, "--multiline")) {
		return r
	}
	r.err, r.skipped, r.skippedOmitted = "", nil, 0
	r.warning = "0 results found. Note: search_files content search is line-oriented and does not run ripgrep with -U/--multiline, so `\\n` in " +
		"the regex does not match line breaks. Use context=N to inspect neighboring lines, or escape as `\\\\n` when searching for a literal backslash+n."
	return r
}

// Search result of rg or grep output, failing only when an exit status of 2 left nothing that matched
func parseSearch(result run, mode string, limit, offset, context int, warning string) *searchResult {
	output, reason := timedOut(result)
	diagnostics, payload := splitDiagnostics(output)
	if result.code == 126 || result.code == 127 {
		message := strings.TrimSpace(result.output)
		if message == "" {
			message = "Search error"
		}
		return &searchResult{err: "Search failed: " + message}
	}
	if result.code == 2 && strings.TrimSpace(payload) == "" {
		message := strings.TrimSpace(diagnostics)
		if message == "" {
			message = strings.TrimSpace(result.output)
		}
		if message == "" {
			message = "Search error"
		}
		if permissionOnly(message) {
			s := skipOf(message)
			warning = s.before(warning)
			if warning == "" {
				warning = permissionHint
			} else if !strings.Contains(warning, permissionHint) {
				warning += " " + permissionHint
			}
			return s.onto(&searchResult{warning: warning})
		}
		return &searchResult{err: "Search failed: " + message}
	}
	s := skipOf(diagnostics)
	skipped := s.before("")
	var others []string
	for _, line := range diff.Split(diagnostics) {
		l := strings.TrimSpace(line)
		if (strings.HasPrefix(lstrip(line), "rg: ") || strings.HasPrefix(lstrip(line), "grep: ")) && !permissionDenied.MatchString(l) && !omittedLine.MatchString(l) {
			others = append(others, l)
		}
	}
	if result.code == 2 && len(others) > 0 {
		incomplete := "The search reported errors for some paths; results may be incomplete (" + others[0] + ")."
		if skipped != "" {
			skipped = incomplete + " " + skipped
		} else {
			skipped = incomplete
		}
	}
	if skipped != "" {
		if warning == "" {
			warning = skipped
		} else {
			warning = skipped + " " + warning
		}
	}
	var lines []string
	for _, line := range strings.Split(strings.TrimSpace(payload), "\n") {
		if line != "" {
			lines = append(lines, line)
		}
	}
	limited := reason != ""
	switch mode {
	case "files_only":
		return s.onto(&searchResult{files: window(lines, offset, limit), total: len(lines), truncated: limited, limitReason: reason, warning: warning})
	case "count":
		r := &searchResult{truncated: limited, limitReason: reason, warning: warning}
		for _, line := range lines {
			at := strings.LastIndex(line, ":")
			if at < 0 {
				continue
			}
			if n, err := strconv.Atoi(strings.TrimSpace(line[at+1:])); err == nil {
				r.counts.set(line[:at], n)
			}
		}
		for _, c := range r.counts {
			r.total += c.n
		}
		return s.onto(r)
	}
	var matches []match
	for _, line := range lines {
		if line == "--" {
			continue
		}
		if m := matchLine.FindStringSubmatch(line); m != nil {
			matches = append(matches, match{m[1] + m[2], lineNumber(m[3]), strs.Head(m[4], 500)})
			continue
		}
		if context > 0 {
			if path, digits, content, ok := contextLine(line); ok {
				matches = append(matches, match{path, lineNumber(digits), strs.Head(content, 500)})
			}
		}
	}
	return s.onto(&searchResult{matches: window(matches, offset, limit), total: len(matches), truncated: len(matches) > offset+limit || limited,
		limitReason: reason, warning: warning})
}

// Whether every root is a POSIX path on a macOS machine
func posixRoots(roots []string) bool {
	if runtime.GOOS != "darwin" {
		return false
	}
	return !slices.ContainsFunc(roots, func(root string) bool { return driveRoot.MatchString(root) || strings.Contains(root, `\`) })
}

// Whether the path exists where commands run
func (o *ops) exists(path string) (bool, run) {
	if o.native {
		_, err := os.Stat(o.full(path))
		return err == nil, run{}
	}
	result := o.exec("test -e "+o.path(path)+" && echo exists || echo not_found", "", 0)
	return strings.Contains(result.output, "exists"), result
}

// Content regex or file name glob search, with several paths in one searched together and macOS privacy folders skipped
func (o *ops) search(pattern, path, target, glob string, limit, offset int, mode string, context int, order string) *searchResult {
	if target == "files" && order != "discovery" && order != "modified" {
		return &searchResult{err: "Invalid file search order " + strconv.Quote(order) + "; expected 'discovery' or 'modified'."}
	}
	path = o.expand(path)
	found, probe := o.exists(path)
	if probe.cwdError != "" {
		return &searchResult{err: probe.cwdError}
	}
	if !o.native && !found && !strings.Contains(probe.output, "not_found") {
		return &searchResult{err: "Terminal environment unavailable: could not stat " + path + " (the sandbox may still be starting or was removed). Retry shortly."}
	}
	if !found {
		if multi := o.searchSeveral(pattern, path, target, glob, limit, offset, mode, context, order); multi != nil {
			return multi
		}
		return o.notFound(path)
	}
	var result *searchResult
	if target == "files" {
		result = o.searchFiles(pattern, []string{path}, false, limit, offset, order)
	} else {
		result = o.searchContent(pattern, path, glob, limit, offset, mode, context)
	}
	if exclusions := o.exclusions(path); len(exclusions) > 0 && result.err == "" {
		names := make([]string, len(exclusions))
		for i, item := range exclusions {
			names[i] = filepath.Base(item)
		}
		protected := "Skipped macOS protected folders during broad search to avoid an unattended privacy prompt: " + strings.Join(names, ", ") +
			". Search a protected folder directly when access is intentional."
		if result.warning != "" {
			result.warning += " " + protected
		} else {
			result.warning = protected
		}
	}
	return result
}

// Error of a search root that is not there, naming up to five entries of its folder that look like it
func (o *ops) notFound(path string) *searchResult {
	parent := dirname(path)
	if parent == "" {
		parent = "."
	}
	query := strings.ToLower(basename(path))
	parts := []string{"Path not found: " + path}
	if query != "" && strings.Contains(o.exec("test -d "+o.path(parent)+" && echo yes || echo no", "", 0).output, "yes") {
		if entries, ok := o.list(parent, 20); ok {
			var candidates []string
			for _, entry := range entries {
				lower := strings.ToLower(entry)
				if strings.Contains(lower, query) || strings.Contains(query, lower) || strings.HasPrefix(lower, strs.Head(query, 3)) {
					candidates = append(candidates, join(parent, entry))
				}
			}
			if len(candidates) > 0 {
				parts = append(parts, "Similar paths: "+strings.Join(candidates[:min(5, len(candidates))], ", "))
			}
		}
	}
	return &searchResult{err: strings.Join(parts, ". ")}
}

// Searches every existing part of a path that holds several, split at commas or whitespace; nil when it holds only one
func (o *ops) searchSeveral(pattern, path, target, glob string, limit, offset int, mode string, context int, order string) *searchResult {
	var parts []string
	if strings.Contains(path, ",") {
		for _, part := range strings.Split(path, ",") {
			if p := strings.TrimSpace(part); p != "" {
				parts = append(parts, p)
			}
		}
	} else {
		parts = fields(path)
	}
	if len(parts) < 2 {
		return nil
	}
	var existing, missing []string
	for _, part := range parts {
		expanded := o.expand(part)
		if found, _ := o.exists(expanded); found {
			existing = append(existing, expanded)
		} else {
			missing = append(missing, expanded)
		}
	}
	if len(existing) == 0 {
		return nil
	}
	var merged *searchResult
	var children []*searchResult
	if target == "files" {
		merged = o.searchFiles(pattern, existing, true, limit, offset, order)
		children = []*searchResult{merged}
	} else {
		merged = &searchResult{}
		for _, root := range existing {
			sub := o.searchContent(pattern, root, glob, limit, offset, mode, context)
			if sub.err != "" {
				return sub
			}
			children = append(children, sub)
			merged.matches = append(merged.matches, sub.matches...)
			merged.files = append(merged.files, sub.files...)
			for _, c := range sub.counts {
				merged.counts.set(c.path, c.n)
			}
			merged.total += sub.total
			merged.truncated = merged.truncated || sub.truncated
		}
		merged.matches, merged.files = window(merged.matches, 0, limit), window(merged.files, 0, limit)
	}
	note := fmt.Sprintf("path contained %d entries; searched %d that exist", len(parts), len(existing))
	if len(missing) > 0 {
		note += "; skipped missing: " + strings.Join(missing[:min(3, len(missing))], ", ")
		if len(missing) > 3 {
			note += fmt.Sprintf(" (+%d more)", len(missing)-3)
		}
	}
	found := merged.total != 0 || len(merged.matches) > 0 || len(merged.files) > 0 || len(merged.counts) > 0
	warnings := append(mergeWarnings(children, found), note)
	if merged.err == "" {
		var protected []string
		for _, ex := range o.effectiveExclusions(existing) {
			protected = append(protected, filepath.Base(ex.absolute))
		}
		if len(protected) > 0 {
			warnings = append(warnings, "Skipped macOS protected folders during broad search to avoid an unattended privacy prompt: "+
				strings.Join(protected, ", ")+". Search a protected folder directly when access is intentional.")
		}
	}
	merged.warning = strings.Join(warnings, " ")
	return merged
}

// Path relative to a root and whether it is under it, POSIX style
func relativeTo(target, root string) (string, bool) {
	t, r := strings.Split(strings.Trim(posixClean(target), "/"), "/"), strings.Split(strings.Trim(posixClean(root), "/"), "/")
	if r[0] == "" {
		r = nil
	}
	if len(r) > len(t) || !slices.Equal(t[:len(r)], r) {
		return "", false
	}
	return strings.Join(t[len(r):], "/"), true
}

// macOS privacy folders under the search root, relative to it, when the root is the home or above it on a macOS machine's own files
func (o *ops) exclusions(path string) []string {
	if runtime.GOOS != "darwin" || !o.host || o.lease.Backend.Remote {
		return nil
	}
	root := path
	if !strings.HasPrefix(root, "/") {
		root = posixJoin(o.base, root)
	}
	home, _ := os.UserHomeDir()
	var out []string
	for _, name := range protectedHome {
		if rel, ok := relativeTo(posixJoin(home, name), root); ok && rel != "" {
			out = append(out, rel)
		}
	}
	return out
}

// One privacy folder to prune: the root it hangs off and its relative and absolute paths
type exclusion struct{ root, relative, absolute string }

// Privacy folders to prune across the roots, once each, never one the caller searches itself
func (o *ops) effectiveExclusions(roots []string) []exclusion {
	normalized := func(root string) string {
		if !strings.HasPrefix(root, "/") {
			root = posixJoin(o.base, root)
		}
		return posixClean(root)
	}
	explicit := map[string]bool{}
	for _, root := range roots {
		explicit[normalized(root)] = true
	}
	seen := map[string]bool{}
	var out []exclusion
	for _, root := range roots {
		for _, relative := range o.exclusions(root) {
			absolute := posixClean(posixJoin(normalized(root), relative))
			if explicit[absolute] || seen[absolute] {
				continue
			}
			seen[absolute] = true
			out = append(out, exclusion{root, relative, absolute})
		}
	}
	return out
}

// Filename walks running, by environment and root, so two never walk the same tree at once
type walks struct {
	mu      sync.Mutex
	changed chan struct{}
	active  map[string]bool
}

func newWalks() *walks { return &walks{changed: make(chan struct{}), active: map[string]bool{}} }

// Keys of the roots of a walk in this environment
func (o *ops) walkKeys(roots []string) []string {
	var keys []string
	for _, root := range roots {
		if !strings.HasPrefix(root, "/") && !filepath.IsAbs(root) {
			root = join(o.base, root)
		}
		key := fmt.Sprintf("%p", o.env) + "\x00" + fold(posixClean(root))
		if !slices.Contains(keys, key) {
			keys = append(keys, key)
		}
	}
	slices.Sort(keys)
	return keys
}

// Claims every root of a walk at once, waiting while another walk holds one, false when the call was interrupted meanwhile
func (w *walks) acquire(ctx context.Context, keys []string) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	for slices.ContainsFunc(keys, func(k string) bool { return w.active[k] }) {
		changed := w.changed
		w.mu.Unlock()
		select {
		case <-changed:
		case <-time.After(50 * time.Millisecond):
		case <-ctx.Done():
		}
		w.mu.Lock()
		if ctx.Err() != nil {
			return false
		}
	}
	if ctx.Err() != nil {
		return false
	}
	for _, k := range keys {
		w.active[k] = true
	}
	return true
}

func (w *walks) release(keys []string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, k := range keys {
		delete(w.active, k)
	}
	close(w.changed)
	w.changed = make(chan struct{})
}

// Error unless the ripgrep at the path reports version 14 or newer, which sorts by modification time; asked once per path
func (o *ops) orderError(executable string) string {
	o.cache.mu.Lock()
	cached, ok := o.cache.ordered[executable]
	o.cache.mu.Unlock()
	if ok {
		if cached == nil {
			return ""
		}
		return *cached
	}
	result := o.exec(o.executable(executable)+" --version", "", 10*time.Second)
	var message *string
	m := rgVersion.FindStringSubmatch(result.output)
	if major, err := strconv.Atoi(firstOr(m, 1)); result.code != 0 || m == nil || err != nil || major < 14 {
		text := "Exact modification-time order requires ripgrep 14 or newer; upgrade ripgrep or use order='discovery'."
		message = &text
	}
	o.cache.mu.Lock()
	o.cache.ordered[executable] = message
	o.cache.mu.Unlock()
	if message == nil {
		return ""
	}
	return *message
}

func firstOr(m []string, i int) string {
	if i < len(m) {
		return m[i]
	}
	return ""
}

// Hint for a content search that found nothing, from the first quick rg count that does: case blind, hidden or ignored files, or the
// pattern taken literally
func (o *ops) zeroHint(pattern, path, glob string) string {
	executable := o.rg()
	if executable == "" {
		return ""
	}
	probes := []struct {
		flags    []string
		template string
	}{
		{[]string{"-i"}, "0 exact matches, but %s case-insensitive match(es) in %d file(s): %s — the pattern's casing may be wrong."},
		{[]string{"--hidden", "--no-ignore"}, "0 matches in visible files, but %s match(es) in %d hidden or gitignored file(s): %s — these are excluded by default."},
		{[]string{"-F"}, "0 regex matches, but %s literal match(es) in %d file(s): %s — the pattern contains regex metacharacters that likely need " +
			"escaping (or pass a simpler substring)."},
	}
	for _, probe := range probes {
		if probe.flags[0] == "-F" && !regexMeta.MatchString(pattern) {
			continue
		}
		argv := append([]string{executable}, probe.flags...)
		argv = append(argv, "--count-matches")
		if glob != "" {
			argv = append(argv, "--glob", glob)
		}
		if probe.flags[0] == "--hidden" {
			argv = append(argv, pruneGlobs()...)
		}
		argv = append(argv, "--", pattern, o.toolPath(path))
		result := o.rgBounded(argv, "", 50, 30, false)
		total := 0
		var perFile []string
		for _, line := range diff.Split(strings.TrimSpace(result.output)) {
			p, n := "", line
			if at := strings.LastIndex(line, ":"); at >= 0 {
				p, n = line[:at], line[at+1:]
			}
			count, err := strconv.Atoi(n)
			if err != nil {
				continue
			}
			total += count
			perFile = append(perFile, p)
		}
		if total > 0 {
			paths := strings.Join(perFile[:min(5, len(perFile))], ", ")
			if extra := len(perFile) - 5; extra > 0 {
				paths += fmt.Sprintf(" (+%d more)", extra)
			}
			return fmt.Sprintf(probe.template, strconv.Itoa(total), len(perFile), paths)
		}
	}
	return ""
}

// Globs that prune the heavy folders at the root and at any depth
func pruneGlobs() []string {
	var out []string
	for _, name := range pruned {
		out = append(out, "--glob", "!"+name+"/**", "--glob", "!**/"+name+"/**")
	}
	return out
}

// Path as a native program of this machine takes it, forward slashed drive form on Windows
func (o *ops) toolPath(p string) string {
	if windows && o.host && p != "" {
		return strings.ReplaceAll(msysToWindows(p), `\`, "/")
	}
	return p
}

// Words joined as one shell command, each quoted, the first as an executable
func (o *ops) shell(argv []string) string {
	words := make([]string, len(argv))
	for i, word := range argv {
		if i == 0 {
			words[i] = o.executable(word)
		} else {
			words[i] = quote(word)
		}
	}
	return strings.Join(words, " ")
}

// First fetch lines of an rg run in the folder, the call's folder when empty: straight on this machine when direct access is on, through
// the shell and head otherwise; diagnostics kept ahead of the lines when asked
func (o *ops) rgBounded(argv []string, dir string, fetch, timeout int, diagnostics bool) run {
	if o.native {
		return o.rgDirect(argv, dir, fetch, timeout, diagnostics)
	}
	command := "set -o pipefail; "
	if dir != "" {
		command += "cd " + o.path(dir) + " && "
	}
	command += o.shell(argv)
	if !diagnostics {
		command += " 2>/dev/null"
	}
	return o.exec(command+" | head -n "+strconv.Itoa(fetch), "", time.Duration(timeout)*time.Second)
}

// rg run as a process of this machine with the environment commands get, its output read until fetch lines or the deadline
func (o *ops) rgDirect(argv []string, dir string, fetch, timeout int, diagnostics bool) run {
	if dir == "" {
		dir = o.base
	}
	if st, err := os.Stat(dir); err != nil || !st.IsDir() {
		if err == nil {
			err = &os.PathError{Op: "chdir", Path: dir, Err: syscall.ENOTDIR}
		}
		return run{output: "rg: " + osErrorText(err, dir), code: 2}
	}
	env := o.r.terminal.ChildEnv(o.cfg, nil)
	program := argv[0]
	if !strings.ContainsAny(program, `/\`) {
		key, _ := terminal.PathKey(env)
		program = lookIn(program, env[key])
	}
	ctx, cancel := context.WithTimeout(o.ctx, time.Duration(timeout)*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, program, argv[1:]...)
	cmd.Dir, cmd.Env = dir, terminal.Environ(env)
	terminal.NewSession(cmd)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return run{output: "rg: " + err.Error(), code: 2}
	}
	var stderr io.ReadCloser
	if diagnostics {
		if stderr, err = cmd.StderrPipe(); err != nil {
			return run{output: "rg: " + err.Error(), code: 2}
		}
	}
	if err := cmd.Start(); err != nil {
		return run{output: "rg: " + osErrorText(err, argv[0]), code: 2}
	}
	var errLines []byte
	omitted := 0
	errDone := make(chan struct{})
	go func() {
		defer close(errDone)
		if stderr == nil {
			return
		}
		reader := bufio.NewReader(stderr)
		other := false
		for {
			raw, rerr := reader.ReadBytes('\n')
			if len(raw) > 0 {
				switch line := strings.TrimSpace(decode(raw)); {
				case len(errLines) > 0 && len(errLines)+len(raw) > stderrKept:
					if !other && line != "" && !permissionDenied.MatchString(line) {
						errLines, other = append(errLines, raw...), true
					} else {
						omitted++
					}
				default:
					errLines = append(errLines, raw...)
				}
			}
			if rerr != nil {
				return
			}
		}
	}()
	var lines []byte
	count := 0
	bounded := false
	reader := bufio.NewReader(stdout)
	for {
		raw, rerr := reader.ReadBytes('\n')
		if len(raw) > 0 {
			lines = append(lines, raw...)
			if count++; count >= fetch {
				bounded = true
				break
			}
		}
		if rerr != nil {
			break
		}
	}
	if bounded {
		cmd.Process.Kill()
	}
	waitErr := cmd.Wait()
	<-errDone
	output := decode(lines)
	if diagnostics {
		text := decode(errLines)
		if omitted > 0 {
			text = strings.TrimRight(text, "\n") + "\n" + fmt.Sprintf("rg: ... %d more diagnostic line(s) omitted", omitted) + "\n"
		}
		output = text + output
	}
	switch {
	case bounded:
		return run{output: output}
	case errors.Is(ctx.Err(), context.DeadlineExceeded) && o.ctx.Err() == nil:
		return run{output: output + fmt.Sprintf("\n[Command timed out after %ds]", timeout), code: 124}
	case o.ctx.Err() != nil:
		return run{output: output + "\n[Command interrupted]", code: 130}
	}
	code := 0
	var exit *exec.ExitError
	if errors.As(waitErr, &exit) {
		code = exit.ExitCode()
	}
	return run{output: output, code: code}
}

// Executable of the name in the folders of a PATH value, the name itself when none holds it
func lookIn(name, path string) string {
	for _, dir := range filepath.SplitList(path) {
		if dir == "" {
			continue
		}
		candidate := filepath.Join(dir, name)
		if st, err := os.Stat(candidate); err == nil && !st.IsDir() && st.Mode()&0o111 != 0 {
			return candidate
		}
	}
	return name
}

// File name glob search across roots through rg's files mode, with folders found through find, else a bounded find
func (o *ops) searchFiles(pattern string, roots []string, several bool, limit, offset int, order string) *searchResult {
	glob := pattern
	if strings.HasPrefix(pattern, "**/") || strings.Contains(pattern, "/") {
		parts := strings.Split(pattern, "/")
		glob = parts[len(parts)-1]
	}
	if len(roots) == 0 {
		return &searchResult{err: "File search requires at least one search root in 'path'."}
	}
	keys := o.walkKeys(roots)
	if o.has("rg") {
		executable := o.rg()
		if order == "modified" {
			if message := o.orderError(executable); message != "" {
				return &searchResult{err: message}
			}
		}
		if !o.r.walks.acquire(o.ctx, keys) {
			return &searchResult{err: walkInterrupted}
		}
		defer o.r.walks.release(keys)
		return o.searchFilesRg(glob, roots, several, limit, offset, order, executable)
	}
	for _, root := range roots {
		if o.broadRoot(root) {
			return &searchResult{err: "Broad local file search without ripgrep is disabled because find cannot keep this traversal safely bounded. " +
				"Install ripgrep or search a narrower directory."}
		}
	}
	if !o.has("find") {
		return &searchResult{err: "File search requires 'rg' (ripgrep) or 'find'. Install ripgrep for best results: " +
			"https://github.com/BurntSushi/ripgrep#installation"}
	}
	quoted := make([]string, len(roots))
	var exemptions strings.Builder
	for i, root := range roots {
		if strings.HasPrefix(root, "-") {
			root = "./" + root
		}
		quoted[i] = o.path(root)
		exemptions.WriteString(" ! -path " + o.path(findMeta.ReplaceAllString(root, `\$1`)))
	}
	var protected []string
	for _, ex := range o.effectiveExclusions(roots) {
		protected = append(protected, ex.absolute)
	}
	prune := ""
	if len(protected) > 0 {
		prune = " " + o.pruneExpr(protected) + " -o"
	}
	fetch := offset + limit + 1
	base := "find -H " + strings.Join(quoted, " ") + prune + " " + hiddenPrune(quoted) + " -o " + `\( -type f -o \( -type d` + exemptions.String() + ` \) \) ` +
		"! -name '.*' -name " + o.path(glob)
	command := "set -o pipefail; " + base + " -print | head -n " + strconv.Itoa(fetch)
	if order == "modified" {
		command = "set -o pipefail; " + base + " -printf '%T@ %p\\n' | sort -rn | head -n " + strconv.Itoa(fetch)
	}
	if !o.r.walks.acquire(o.ctx, keys) {
		return &searchResult{err: walkInterrupted}
	}
	result := o.exec(command, "", 60*time.Second)
	o.r.walks.release(keys)
	output, reason := timedOut(result)
	found, diagnostics := splitFind(output, order)
	failure, partial := findVerdict(result.code, found, diagnostics, fetch, order)
	if failure != nil {
		return failure
	}
	if windows && o.host {
		for i, f := range found {
			found[i] = msysToWindows(f)
		}
	}
	return &searchResult{files: window(found, offset, limit), total: len(found), truncated: len(found) > offset+limit || reason != "",
		limitReason: reason, warning: partial, skipped: unreadable(strings.Join(diagnostics, "\n"))}
}

// Clause of find that prunes hidden folders but each root chosen
func hiddenPrune(roots []string) string {
	var exemptions strings.Builder
	for _, root := range roots {
		exemptions.WriteString(" ! -path " + root)
	}
	return `\( -type d -name '.*'` + exemptions.String() + ` \) -prune`
}

// Clause of find that prunes the paths
func (o *ops) pruneExpr(paths []string) string {
	terms := make([]string, len(paths))
	for i, p := range paths {
		terms[i] = "-path " + o.path(p)
	}
	return `\( ` + strings.Join(terms, " -o ") + ` \) -prune`
}

// Paths and diagnostics of a find run, the times of a modified order dropped
func splitFind(output, order string) ([]string, []string) {
	var found, diagnostics []string
	for _, line := range diff.Split(output) {
		switch {
		case strings.HasPrefix(line, "find: "):
			diagnostics = append(diagnostics, line)
		case order == "modified":
			if stamp, p, ok := strings.Cut(line, " "); ok && digitsOnly(strings.Replace(stamp, ".", "", 1)) {
				found = append(found, p)
			}
		case line != "":
			found = append(found, line)
		}
	}
	return found, diagnostics
}

func digitsOnly(s string) bool {
	return s != "" && !strings.ContainsFunc(s, func(r rune) bool { return r < '0' || r > '9' })
}

// Failure or partial warning of a bounded find run from its exit status and diagnostics
func findVerdict(code int, found, diagnostics []string, fetch int, order string) (*searchResult, string) {
	cut := code == 141 && len(found) >= fetch
	if code != 0 && code != 1 && code != 124 && !cut {
		if order == "modified" {
			return &searchResult{err: findOrderError}, ""
		}
		return &searchResult{err: "File search failed while running bounded find traversal."}, ""
	}
	text := strings.Join(diagnostics, "\n")
	skipped := unreadable(text)
	warning := ""
	if len(skipped) > 0 {
		warning = skippedWarning(skipped, 0)
	}
	if code != 1 {
		return nil, warning
	}
	switch {
	case len(found) == 0 && permissionOnly(text):
		return &searchResult{warning: warning + " " + permissionHint, skipped: skipped}, ""
	case len(found) == 0 && order == "modified":
		return &searchResult{err: findOrderError}, ""
	case len(found) == 0:
		detail := "."
		if len(diagnostics) > 0 {
			detail = ": " + diagnostics[0]
		}
		return &searchResult{err: "File search failed while running bounded find traversal" + detail}, ""
	}
	for _, d := range diagnostics {
		if other := strings.TrimSpace(d); other != "" && !permissionDenied.MatchString(other) {
			message := "Some paths could not be read: " + other + ". Results may be incomplete."
			if warning != "" {
				message += " " + warning
			}
			return nil, message
		}
	}
	if warning != "" {
		return nil, warning
	}
	return nil, "find traversal skipped one or more unreadable paths (restricted permissions); results are partial."
}

// Whether a search root of this machine's files is the file system root, the home, or above the home
func (o *ops) broadRoot(root string) bool {
	if !o.host {
		return false
	}
	normalized := func(p string) string {
		p = msysToWindows(p)
		if !filepath.IsAbs(p) {
			p = filepath.Join(o.base, p)
		}
		return fold(realpath(p))
	}
	target := normalized(root)
	home, _ := os.UserHomeDir()
	if target == fold(filepath.VolumeName(target)+string(filepath.Separator)) {
		return true
	}
	homeDir := normalized(home)
	return target == homeDir || under(homeDir, target)
}

// Paths of a search, newest first by the times the environment gives them, those that vanished last
func (o *ops) newestFirst(paths []string) []string {
	if len(paths) < 2 {
		return paths
	}
	script := `while IFS= read -r item || [ -n "$item" ]; do mtime="$(stat -c%Y "$item" 2>/dev/null || stat -f%m "$item" 2>/dev/null || true)"; ` +
		`printf '%s\n' "${mtime:-0}"; done`
	stdin := strings.Join(paths, "\n")
	lines := diff.Split(o.execStdin(script, "", 60*time.Second, &stdin).output)
	times := make([]float64, len(paths))
	for i := range paths {
		if i < len(lines) {
			times[i], _ = strconv.ParseFloat(strings.TrimSpace(lines[i]), 64)
		}
	}
	order := make([]int, len(paths))
	for i := range order {
		order[i] = i
	}
	slices.SortStableFunc(order, func(a, b int) int {
		switch {
		case times[a] > times[b]:
			return -1
		case times[a] < times[b]:
			return 1
		}
		return 0
	})
	out := make([]string, len(paths))
	for i, j := range order {
		out[i] = paths[j]
	}
	return out
}

// Whether the root or a folder above it is hidden
func (o *ops) hiddenRoot(root string) bool {
	if !strings.HasPrefix(root, "/") && !filepath.IsAbs(root) {
		root = join(o.base, root)
	}
	for _, part := range strings.Split(strings.ReplaceAll(root, `\`, "/"), "/") {
		if strings.HasPrefix(part, ".") && part != "." && part != ".." {
			return true
		}
	}
	return false
}

// Paths without those inside a hidden folder below the root
func (o *ops) withoutHidden(paths []string, root string) []string {
	var out []string
	for _, p := range paths {
		rel, ok := relativeTo(p, root)
		if !ok {
			rel = p
		}
		hidden := slices.ContainsFunc(strings.Split(rel, "/"), func(part string) bool { return strings.HasPrefix(part, ".") && part != "." && part != ".." })
		if !hidden {
			out = append(out, p)
		}
	}
	return out
}

// Paths without those git ignores, when the root is in a git worktree
func (o *ops) withoutIgnored(paths []string, root string) []string {
	if len(paths) == 0 || o.exec("git -C "+o.path(root)+" rev-parse --is-inside-work-tree >/dev/null 2>&1", "", 10*time.Second).code != 0 {
		return paths
	}
	stdin := strings.Join(paths, "\n")
	ignored := strs.Set(diff.Split(o.execStdin("git -C "+o.path(root)+" check-ignore --stdin", "", 10*time.Second, &stdin).output)...)
	return slices.DeleteFunc(slices.Clone(paths), func(p string) bool { return ignored[p] })
}

// Folders below the root matching the glob, found through find so empty folders show up too
func (o *ops) folders(glob, root string, fetch int) ([]string, string) {
	if fetch <= 0 || !o.has("find") {
		return nil, ""
	}
	base := "find -H " + o.path(root) + " -mindepth 1 " + hiddenPrune([]string{o.path(root)}) + " -o -type d ! -name '.*' -name " + o.path(glob)
	output, reason := timedOut(o.exec(base+" -printf '%T@ %p\\n' 2>/dev/null | sort -rn", "", 30*time.Second))
	if strings.TrimSpace(output) == "" && reason == "" {
		output, reason = timedOut(o.exec(base+" 2>/dev/null", "", 30*time.Second))
	}
	var dirs []string
	for _, line := range strings.Split(strings.TrimSpace(output), "\n") {
		if line == "" {
			continue
		}
		if stamp, p, ok := strings.Cut(line, " "); ok && digitsOnly(strings.ReplaceAll(stamp, ".", "")) {
			dirs = append(dirs, p)
		} else {
			dirs = append(dirs, line)
		}
	}
	if len(dirs) > 0 {
		if o.hiddenRoot(root) {
			dirs = o.withoutHidden(dirs, root)
		}
		dirs = o.withoutIgnored(dirs, root)
	}
	return window(o.newestFirst(dirs), 0, fetch), reason
}

// File name search through rg's files mode with the privacy folders excluded, folders that match added from find
func (o *ops) searchFilesRg(glob string, roots []string, several bool, limit, offset int, order, executable string) *searchResult {
	if !strings.Contains(glob, "/") && !strings.HasPrefix(glob, "*") {
		glob = "*" + glob
	}
	fetch := limit + offset + 1
	effective := o.effectiveExclusions(roots)
	scoped, common := false, ""
	commandRoots := roots
	var excludes []string
	if len(roots) > 1 && len(effective) > 0 && posixRoots(roots) {
		absolute := make([]string, len(roots))
		for i, root := range roots {
			if !strings.HasPrefix(root, "/") {
				root = posixJoin(o.base, root)
			}
			absolute[i] = posixClean(root)
		}
		scoped, common = true, commonPath(absolute)
		commandRoots = make([]string, len(absolute))
		for i, root := range absolute {
			commandRoots[i], _ = filepath.Rel(common, root)
		}
		for _, ex := range effective {
			rel, _ := filepath.Rel(common, ex.absolute)
			excludes = append(excludes, "!"+rel+"/**")
		}
	} else {
		for _, ex := range effective {
			excludes = append(excludes, "!"+ex.relative+"/**")
		}
	}
	argv := []string{executable, "--files"}
	if order == "modified" {
		argv = append(argv, "--sortr=modified")
	}
	argv = append(argv, "-g", glob)
	for _, exclude := range strs.Unique(excludes) {
		argv = append(argv, "--glob", exclude)
	}
	argv = append(argv, "--")
	for _, root := range commandRoots {
		argv = append(argv, o.toolPath(root))
	}
	dir := ""
	if scoped {
		dir = common
	}
	result := o.rgBounded(argv, dir, fetch, 60, true)
	output, reason := timedOut(result)
	var diagnosticLines, found []string
	for _, line := range diff.Split(output) {
		if strings.HasPrefix(lstrip(line), "rg: ") || strings.HasPrefix(lstrip(line), "grep: ") {
			diagnosticLines = append(diagnosticLines, line)
		} else if line != "" {
			found = append(found, line)
		}
	}
	diagnostics := strings.Join(diagnosticLines, "\n")
	var s skip
	if permissionOnly(diagnostics) {
		s = skipOf(diagnostics)
	}
	onlyPermissions := result.code == 2 && len(s.paths) > 0
	warning := s.before("")
	if scoped {
		for i, f := range found {
			if !strings.HasPrefix(f, "/") {
				found[i] = posixClean(posixJoin(common, f))
			}
		}
	}
	cut := result.code == 141 && len(found) >= fetch
	if code := result.code; code != 0 && code != 1 && code != 124 && !cut && !onlyPermissions {
		detail := strings.TrimSpace(diagnostics)
		if detail == "" {
			detail = "no diagnostics"
		}
		if order == "modified" {
			return &searchResult{err: "Exact modification-time order search failed (ripgrep 14+ required for --sortr=modified; the installed rg passed " +
				"the version check, so this is an invocation failure: " + detail + "). Use order='discovery'."}
		}
		return &searchResult{err: "File search failed while running ripgrep: " + detail}
	}
	if reason != searchTimeoutMarker && !several && o.has("find") {
		if dirs, dirReason := o.folders(glob, roots[0], fetch); len(dirs) > 0 {
			combined := slices.Clone(found)
			for _, d := range dirs {
				if !slices.Contains(found, d) {
					combined = append(combined, d)
				}
			}
			found = o.newestFirst(combined)
			if reason == "" {
				reason = dirReason
			}
		}
	}
	if onlyPermissions && len(found) == 0 {
		return s.onto(&searchResult{warning: warning + " " + permissionHint})
	}
	return s.onto(&searchResult{files: window(found, offset, limit), total: len(found), truncated: len(found) > offset+limit || reason != "",
		limitReason: reason, warning: warning})
}

// Longest folder every path is under
func commonPath(paths []string) string {
	common := strings.Split(strings.Trim(paths[0], "/"), "/")
	for _, p := range paths[1:] {
		parts := strings.Split(strings.Trim(p, "/"), "/")
		n := 0
		for n < len(common) && n < len(parts) && common[n] == parts[n] {
			n++
		}
		common = common[:n]
	}
	return "/" + strings.Join(common, "/")
}

// Content search through rg, else grep, with a hint when nothing matched and grep's note on regexes that want a line break
func (o *ops) searchContent(pattern, path, glob string, limit, offset int, mode string, context int) *searchResult {
	usedRg := o.has("rg")
	var result *searchResult
	switch {
	case usedRg:
		result = o.searchRg(pattern, path, glob, limit, offset, mode, context)
	case o.has("grep"):
		result = o.searchGrep(pattern, path, glob, limit, offset, mode, context)
	default:
		return &searchResult{err: "Content search requires ripgrep (rg) or grep. Install ripgrep: https://github.com/BurntSushi/ripgrep#installation"}
	}
	if result.err == "" && result.total == 0 && len(result.matches) == 0 && len(result.files) == 0 && len(result.counts) == 0 {
		if hint := o.zeroHint(pattern, path, glob); hint != "" {
			if result.warning == "" {
				result.warning = hint
			} else {
				result.warning += " " + hint
			}
		}
	}
	if usedRg {
		return result
	}
	return newlineNote(result, pattern)
}

// Content search with ripgrep, lines capped in width, multiline for a regex that wants a line break
func (o *ops) searchRg(pattern, path, glob string, limit, offset int, mode string, context int) *searchResult {
	argv := []string{o.rg(), "--line-number", "--no-heading", "--with-filename"}
	if mode != "files_only" && mode != "count" {
		argv = append(argv, "--max-columns", "2000", "--max-columns-preview")
	}
	multiline := wantsNewline(pattern)
	if multiline {
		argv = append(argv, "--multiline")
	}
	if context > 0 {
		argv = append(argv, "-C", strconv.Itoa(context))
	}
	for _, item := range o.exclusions(path) {
		argv = append(argv, "--glob", "!"+item+"/**")
	}
	if glob != "" {
		argv = append(argv, "--glob", glob)
	}
	if flag, ok := modeFlags[mode]; ok {
		argv = append(argv, flag)
	}
	argv = append(argv, "--", pattern, o.toolPath(path))
	note := ""
	if multiline {
		note = "Pattern contains \\n — multiline mode (-U) was enabled automatically so the regex can match across line boundaries."
	}
	fetch := limit + offset
	if context > 0 {
		fetch += contextFetchExtra
	}
	return parseSearch(o.rgBounded(argv, "", fetch, 60, true), mode, limit, offset, context, note)
}

// Words of a grep: the head, context, include glob, mode flag and the pattern
func grepWords(head []string, pattern, mode string, context int, glob string) []string {
	words := slices.Clone(head)
	if context > 0 {
		words = append(words, "-C", strconv.Itoa(context))
	}
	if glob != "" {
		words = append(words, "--include", quote(glob))
	}
	if flag, ok := modeFlags[mode]; ok {
		words = append(words, flag)
	}
	return append(words, "-e", quote(pattern))
}

// Recursive grep that skips hidden folders, through find when the root needs pruning
func (o *ops) searchGrep(pattern, path, glob string, limit, offset int, mode string, context int) *searchResult {
	var protected []string
	for _, item := range o.exclusions(path) {
		protected = append(protected, posixClean(posixJoin(path, item)))
	}
	var words []string
	if len(protected) > 0 || o.hiddenRoot(path) {
		root := path
		if root == "" {
			root = "."
		}
		words = []string{"find", "-H", o.path(root)}
		if len(protected) > 0 {
			words = append(words, o.pruneExpr(protected), "-o")
		}
		words = append(words, hiddenPrune([]string{o.path(root)}), "-o", "-type f")
		if glob != "" {
			words = append(words, "-name", quote(glob))
		}
		words = append(words, "-exec")
		words = append(words, grepWords([]string{"grep", "-nHE"}, pattern, mode, context, "")...)
		words = append(words, "{}", "+")
	} else {
		words = grepWords([]string{"grep", "-rnHE", "--exclude-dir='.*'"}, pattern, mode, context, glob)
		if strings.HasPrefix(path, "/") || strings.HasPrefix(path, `\\`) || driveRoot.MatchString(path) {
			words = append(words, o.path(path))
		} else {
			root := `"$PWD"`
			if relative := strings.TrimPrefix(path, "./"); relative != "" && relative != "." {
				root += "/" + o.path(relative)
			}
			words = append(words, root)
		}
	}
	fetch := limit + offset
	if context > 0 {
		fetch += contextFetchExtra
	}
	command := "set -o pipefail; " + strings.Join(words, " ") + " | head -n " + strconv.Itoa(fetch)
	if mode != "files_only" && mode != "count" {
		command += " | cut -c1-2000"
	}
	return parseSearch(o.exec(command, "", 60*time.Second), mode, limit, offset, context, "")
}
