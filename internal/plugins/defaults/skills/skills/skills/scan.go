package skills

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/nickheyer/icebath/internal/tools"
	"github.com/nickheyer/icebath/pkg/fsx"
	"github.com/nickheyer/icebath/pkg/strs"
)

// One threat pattern or structural problem a scan found
type Finding struct {
	PatternID   string `json:"pattern_id"`
	Severity    string `json:"severity"`
	Category    string `json:"category"`
	File        string `json:"file"`
	Line        int    `json:"line"`
	Match       string `json:"match"`
	Description string `json:"description"`
}

// What a scan of a skill found and its verdict
type Result struct {
	Skill  string
	Source string
	// builtin, trusted, community or agent-created
	Trust string
	// safe, caution or dangerous
	Verdict   string
	Findings  []Finding
	ScannedAt string
	Summary   string
}

// Attestation of a scan, kept with a cached scan and with the install it allowed
type provenance struct {
	BundleHash     string    `json:"bundle_hash"`
	ScannerVersion string    `json:"scanner_version"`
	Source         string    `json:"source"`
	SourceURL      string    `json:"source_url"`
	Verdict        string    `json:"verdict"`
	Trust          string    `json:"trust_level"`
	Findings       []Finding `json:"findings"`
	Rules          []string  `json:"rules"`
	ScannedAt      string    `json:"scanned_at"`
	Summary        string    `json:"summary"`
	Fresh          bool      `json:"fresh"`
}

// Findings of a path a line cannot act on, which drop to a lower severity in a comment or a list of what to refuse
var pathReferences = map[string]bool{
	"ssh_dir_access": true, "aws_dir_access": true, "gpg_dir_access": true, "kube_dir_access": true, "docker_dir_access": true,
	"ssh_backdoor": true, "system_passwd_access": true,
}

// Comment markers of the languages a scan reads, by extension
var commentPrefixes = map[string][]string{
	".py": {"#"}, ".sh": {"#"}, ".bash": {"#"}, ".rb": {"#"}, ".pl": {"#"}, ".r": {"#"}, ".jl": {"#"}, ".yaml": {"#"}, ".yml": {"#"},
	".toml": {"#"}, ".conf": {"#"}, ".cfg": {"#", ";"}, ".ini": {"#", ";"}, ".js": {"//"}, ".ts": {"//"}, ".php": {"//", "#"},
}

var (
	denylistOwner = regexp.MustCompile(`(?i)^\s*(?:(?:const|let|var|export)\s+)?[\w.\-]*(?:deny|black|block|skip|exclu|ignor|forbid|refus|reject|never` +
		`|unsafe|sensitive|secret_?file|redact)[\w.\-]*\s*(?::[^=]*?)?(?:=|:\s*(?:$|[\[({]))`)
	actionOnLine = regexp.MustCompile(`(?i)\b(?:cat|less|more|head|tail|cp|mv|rm|scp|rsync|curl|wget|tee|chmod|chown|ssh|sudo|install|source|eval|exec` +
		`|system|popen|run|check_output|copyfile|copy2|sendfile)\b|\.(?:read|write|open|unlink|copy|append)\w*\s*\(` +
		`|\bopen\s*\(|readFileSync|writeFileSync|appendFileSync|>>`)
	quotedSpan = regexp.MustCompile(`'[^'\n]*'|"[^"\n]*"`)
)

// Line, from 0, that opened each line's statement through brackets left open or a trailing backslash
func statementOwners(lines []string) []int {
	owners := make([]int, 0, len(lines))
	depth, owner, continued := 0, 0, false
	for i, line := range lines {
		if depth <= 0 && !continued {
			owner = i
		}
		owners = append(owners, owner)
		bare := quotedSpan.ReplaceAllString(line, "")
		depth += strings.Count(bare, "(") + strings.Count(bare, "[") + strings.Count(bare, "{") - strings.Count(bare, ")") - strings.Count(bare, "]") -
			strings.Count(bare, "}")
		depth = max(depth, 0)
		continued = strings.HasSuffix(strings.TrimRight(line, " \t\r\n"), `\`)
	}
	return owners
}

// Severity and description of a path finding in a comment, informational, or in a list of what to refuse that no line acts on
func demote(id, severity, description, line, owner, suffix string) (string, string) {
	if !pathReferences[id] {
		return severity, description
	}
	trimmed := strings.TrimLeft(line, " \t")
	for _, prefix := range commentPrefixes[suffix] {
		if strings.HasPrefix(trimmed, prefix) {
			return "low", description + ", in a comment, so only for your information"
		}
	}
	if denylistOwner.MatchString(owner) && !actionOnLine.MatchString(line) {
		return "high", description + ", in a list of things to refuse, so check it before you install"
	}
	return severity, description
}

// Limits of a skill's folder: files, total size and size of one file, in kilobytes
const (
	maxFiles   = 50
	maxTotalKB = 5120
	maxFileKB  = 256
)

// Extensions of the files a scan reads
var scannable = map[string]bool{
	".md": true, ".txt": true, ".py": true, ".sh": true, ".bash": true, ".js": true, ".ts": true, ".rb": true, ".yaml": true, ".yml": true,
	".json": true, ".toml": true, ".cfg": true, ".ini": true, ".conf": true, ".html": true, ".css": true, ".xml": true, ".tex": true, ".r": true,
	".jl": true, ".pl": true, ".php": true,
}

// Extensions of binaries no skill should carry
var binaries = map[string]bool{
	".exe": true, ".dll": true, ".so": true, ".dylib": true, ".bin": true, ".dat": true, ".com": true, ".msi": true, ".dmg": true, ".app": true,
	".deb": true, ".rpm": true,
}

// Extensions of scripts, which may be executable
var scripts = map[string]bool{".sh": true, ".bash": true, ".py": true, ".rb": true, ".pl": true}

// Invisible and direction changing characters that hide text, in the order a scan names them
var invisible = []struct {
	r    rune
	name string
}{
	{0x200b, "zero-width space"}, {0x200c, "zero-width non-joiner"}, {0x200d, "zero-width joiner"}, {0x2060, "word joiner"},
	{0x2062, "invisible times"}, {0x2063, "invisible separator"}, {0x2064, "invisible plus"}, {0xfeff, "BOM/zero-width no-break space"},
	{0x202a, "LTR embedding"}, {0x202b, "RTL embedding"}, {0x202c, "pop directional"}, {0x202d, "LTR override"}, {0x202e, "RTL override"},
	{0x2066, "LTR isolate"}, {0x2067, "RTL isolate"}, {0x2068, "first strong isolate"}, {0x2069, "pop directional isolate"},
}

// Patterns read with the link targets of markdown prose blanked
var traversals = map[string]bool{"path_traversal": true, "path_traversal_deep": true}

// Lines, from 1, inside or on the edge of triple quoted strings
func docstringLines(lines []string) map[int]bool {
	doc := map[int]bool{}
	inside := false
	for i, line := range lines {
		was := inside
		double, single := strings.Count(line, `"""`), strings.Count(line, `'''`)
		if (double+single)%2 == 1 {
			inside = !inside
		}
		if was || inside || double > 0 || single > 0 {
			doc[i+1] = true
		}
	}
	return doc
}

// The line with the targets of its balanced inline links blanked, every other character where it was
func maskLinks(line string) string {
	runes := []rune(line)
	masked := slices.Clone(runes)
	from := 0
	for {
		start := -1
		for i := from; i+1 < len(runes); i++ {
			if runes[i] == ']' && runes[i+1] == '(' {
				start = i
				break
			}
		}
		if start < 0 {
			break
		}
		depth, escaped, closed := 1, false, -1
		for cursor := start + 2; cursor < len(runes) && closed < 0; cursor++ {
			switch ch := runes[cursor]; {
			case escaped:
				escaped = false
			case ch == '\\':
				escaped = true
			case ch == '(':
				depth++
			case ch == ')':
				if depth--; depth == 0 {
					closed = cursor
				}
			}
		}
		if closed < 0 {
			break
		}
		for k := start + 2; k < closed; k++ {
			masked[k] = ' '
		}
		from = closed + 1
	}
	return string(masked)
}

var (
	fenceLine       = regexp.MustCompile("^ {0,3}(`{3,}|~{3,})(.*)$")
	containerPrefix = regexp.MustCompile(`^(?: {0,3}(?:>|(?:[-*+]|\d{1,9}[.)]) {1,4}))+`)
)

// Lines of markdown with the link targets of its prose blanked, its code kept as written
func maskProse(lines []string) []string {
	out := make([]string, 0, len(lines))
	var fence byte
	length, open := 0, false
	for _, line := range lines {
		m := fenceLine.FindStringSubmatch(containerPrefix.ReplaceAllString(line, ""))
		code := false
		if open {
			if m != nil && m[1][0] == fence && len(m[1]) >= length && strings.TrimSpace(m[2]) == "" {
				open = false
			}
			code = true
		} else {
			if m != nil && !(m[1][0] == '`' && strings.Contains(m[2], "`")) {
				open, fence, length = true, m[1][0], len(m[1])
			}
			code = open || strings.HasPrefix(line, "\t") || strings.HasPrefix(line, "    ")
		}
		if code {
			out = append(out, line)
		} else {
			out = append(out, maskLinks(line))
		}
	}
	return out
}

// First n characters of a text, cut with an ellipsis when longer
func clipRunes(text string, n int) string {
	runes := []rune(text)
	if len(runes) <= n {
		return text
	}
	return string(runes[:n-3]) + "..."
}

// Threat patterns and invisible characters in one file a scan reads, each pattern once per line; none for a file of another kind or
// one that is not UTF-8
func scanFile(path, rel string) []Finding {
	if rel == "" {
		rel = filepath.Base(path)
	}
	suffix := strings.ToLower(filepath.Ext(path))
	if !scannable[suffix] && filepath.Base(path) != "SKILL.md" {
		return nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	text := strings.TrimPrefix(string(data), "\uFEFF")
	if !utf8.ValidString(text) {
		return nil
	}
	lines := strings.Split(text, "\n")
	doc := docstringLines(lines)
	masked := lines
	if suffix == ".md" {
		masked = maskProse(lines)
	}
	owners := statementOwners(lines)
	var findings []Finding
	for _, p := range threats {
		for i, line := range lines {
			if doc[i+1] {
				continue
			}
			scanned := line
			if traversals[p.id] {
				scanned = masked[i]
			}
			if hit, err := p.re.MatchString(scanned); err != nil || !hit {
				continue
			}
			severity, description := demote(p.id, p.severity, p.description, line, lines[owners[i]], suffix)
			findings = append(findings, Finding{p.id, severity, p.category, rel, i + 1, clipRunes(strings.TrimSpace(line), 120), description})
		}
	}
	for i, line := range lines {
		for _, ch := range invisible {
			if strings.ContainsRune(line, ch.r) {
				findings = append(findings, Finding{"invisible_unicode", "high", "injection", rel, i + 1, fmt.Sprintf("U+%04X (%s)", ch.r, ch.name),
					fmt.Sprintf("invisible unicode character %s (possible text hiding/injection)", ch.name)})
				break
			}
		}
	}
	return findings
}

// Findings that only inform when a community skill's markdown says them, since installing runs none of it
var docNotes = map[string]bool{"sudo_usage": true, "bind_all_interfaces": true}

func communityNote(f Finding) Finding {
	if (f.Severity == "critical" || f.Severity == "high") && docNotes[f.PatternID] && strings.HasSuffix(strings.ToLower(f.File), ".md") {
		f.Severity = "medium"
		f.Description += ", which does not run on install, so only for your information"
	}
	return f
}

// Structure and every file a scan reads of a skill's folder, or one file
func scanSkill(path, source string) Result {
	if source == "" {
		source = "community"
	}
	name, trust := filepath.Base(path), trustOf(source)
	var findings []Finding
	if fsx.IsDir(path) {
		ignore := ignored(path)
		findings = append(findings, structure(path, ignore)...)
		for _, rel := range walkFiles(path) {
			if !ignore(rel) {
				findings = append(findings, scanFile(filepath.Join(path, filepath.FromSlash(rel)), rel)...)
			}
		}
	} else if info, err := os.Stat(path); err == nil && info.Mode().IsRegular() {
		findings = append(findings, scanFile(path, filepath.Base(path))...)
	}
	if trust == "community" {
		for i := range findings {
			findings[i] = communityNote(findings[i])
		}
	}
	verdict := verdictOf(findings)
	return Result{Skill: name, Source: source, Trust: trust, Verdict: verdict, Findings: findings, ScannedAt: time.Now().UTC().Format(time.RFC3339Nano),
		Summary: summary(name, verdict, findings)}
}

// Digest of a skill's files and their paths, or of one file
func contentDigest(path string) (string, error) {
	h := sha256.New()
	if !fsx.IsDir(path) {
		data, err := os.ReadFile(path)
		if err != nil {
			return "", err
		}
		h.Write(data)
		return hex.EncodeToString(h.Sum(nil)), nil
	}
	for _, rel := range walkFiles(path) {
		data, err := os.ReadFile(filepath.Join(path, filepath.FromSlash(rel)))
		if err != nil {
			return "", err
		}
		h.Write([]byte(rel))
		h.Write([]byte{0})
		h.Write(data)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// Scan of a skill with its attestation; the cache folder, when given, answers for the same content from the same source scanned by
// these rules
func scanCached(path, source, sourceURL, cacheDir string) (Result, provenance, error) {
	digest, err := contentDigest(path)
	if err != nil {
		return Result{}, provenance{}, err
	}
	identity := sha256.Sum256([]byte(source + "\x00" + sourceURL))
	file := ""
	if cacheDir != "" {
		file = filepath.Join(cacheDir, digest+"-"+hex.EncodeToString(identity[:])[:16]+".json")
		var cached provenance
		if data, err := os.ReadFile(file); err == nil && json.Unmarshal(bytes.TrimPrefix(data, []byte("\uFEFF")), &cached) == nil && cached.BundleHash == "sha256:"+digest &&
			cached.ScannerVersion == scannerVersion && cached.Source == source && cached.SourceURL == sourceURL {
			cached.Fresh = false
			return Result{Skill: filepath.Base(path), Source: source, Trust: cached.Trust, Verdict: cached.Verdict, Findings: cached.Findings,
				ScannedAt: cached.ScannedAt, Summary: cached.Summary}, cached, nil
		}
	}
	result := scanSkill(path, source)
	prov := attest(result, "sha256:"+digest, sourceURL)
	if file != "" && os.MkdirAll(cacheDir, 0o755) == nil {
		if data, err := json.MarshalIndent(prov, "", "  "); err == nil {
			os.WriteFile(file, append(data, '\n'), 0o644)
		}
	}
	return result, prov, nil
}

// Attestation of a fresh scan
func attest(result Result, hash, sourceURL string) provenance {
	findings := result.Findings
	if findings == nil {
		findings = []Finding{}
	}
	rules := []string{}
	for _, f := range findings {
		if !slices.Contains(rules, f.PatternID) {
			rules = append(rules, f.PatternID)
		}
	}
	slices.Sort(rules)
	return provenance{BundleHash: hash, ScannerVersion: scannerVersion, Source: result.Source, SourceURL: sourceURL, Verdict: result.Verdict,
		Trust: result.Trust, Findings: findings, Rules: rules, ScannedAt: result.ScannedAt, Summary: result.Summary, Fresh: true}
}

// What a verdict means for an install by its source's trust: allow, block, or ask the user; and why
func decide(result Result, force bool) (string, string) {
	rules, ok := policy[result.Trust]
	if !ok {
		rules = policy["community"]
	}
	at := map[string]int{"safe": 0, "caution": 1, "dangerous": 2}
	index, ok := at[result.Verdict]
	if !ok {
		index = 2
	}
	decision, n := rules[index], len(result.Findings)
	hard := result.Verdict == "dangerous" && (result.Trust == "community" || result.Trust == "trusted")
	found := strs.Count(n, "finding")
	switch {
	case decision == "allow":
		return "allow", fmt.Sprintf("Allowed, because the source has %s trust and the scan says %s.", result.Trust, strings.ToLower(verdictWord(result.Verdict)))
	case force && !hard:
		return "allow", fmt.Sprintf("Installed because you passed --force, though the scan says %s with %s.", strings.ToLower(verdictWord(result.Verdict)), found)
	case decision == "ask":
		return "ask", fmt.Sprintf("Needs your yes, because the source has %s trust and the scan says %s with %s.", result.Trust,
			strings.ToLower(verdictWord(result.Verdict)), found)
	}
	blocked := fmt.Sprintf("Blocked, because the source has %s trust and the scan says %s with %s. ", result.Trust, strings.ToLower(verdictWord(result.Verdict)), found)
	if hard {
		return "block", blocked + "--force can't install a skill the scan calls dangerous."
	}
	return "block", blocked + "Add --force to install it anyway."
}

var severityOrder = map[string]int{"critical": 0, "high": 1, "medium": 2, "low": 3}

func severityRank(severity string) int {
	if rank, ok := severityOrder[severity]; ok {
		return rank
	}
	return 4
}

// The scan as the user reads it: the verdict, every finding from the worst, and what it means for an install
func report(result Result) string {
	lines := []string{fmt.Sprintf("Scan of %s, from %s, a source with %s trust: %s.", result.Skill, result.Source, result.Trust, verdictWord(result.Verdict))}
	if len(result.Findings) > 0 {
		sorted := slices.Clone(result.Findings)
		slices.SortStableFunc(sorted, func(a, b Finding) int { return severityRank(a.Severity) - severityRank(b.Severity) })
		for _, f := range sorted {
			lines = append(lines, fmt.Sprintf("  %s risk, %s, at %s:%d: “%s”", strs.Capital(f.Severity), f.Category, f.File, f.Line, clipRunes(f.Match, 60)))
		}
		lines = append(lines, "")
	}
	_, reason := decide(result, false)
	return strings.Join(append(lines, reason), "\n")
}

// Verdict of a scan in a word
func verdictWord(verdict string) string {
	switch verdict {
	case "safe":
		return "Safe"
	case "caution":
		return "Caution"
	case "dangerous":
		return "Dangerous"
	}
	return strs.Capital(verdict)
}

// Structural problems of a skill's folder: links that break or leave it, too many or too large files, binaries, unexpected executables
func structure(dir string, ignore func(string) bool) []Finding {
	var findings []Finding
	add := func(id, severity, category, rel, match, description string) {
		findings = append(findings, Finding{id, severity, category, rel, 0, match, description})
	}
	root := resolved(dir)
	count, total := 0, int64(0)
	filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil || p == dir {
			return nil
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return nil
		}
		link := d.Type()&os.ModeSymlink != 0
		if !d.Type().IsRegular() && !link || ignore(filepath.ToSlash(rel)) {
			return nil
		}
		count++
		if link {
			target, err := filepath.EvalSymlinks(p)
			switch {
			case err != nil:
				add("broken_symlink", "medium", "traversal", rel, "broken symlink", "broken or circular symlink")
			case !inside(target, root):
				add("symlink_escape", "critical", "traversal", rel, "symlink -> "+target, "symlink points outside the skill directory")
			}
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		total += info.Size()
		if info.Size() > maxFileKB*1024 {
			add("oversized_file", "medium", "structural", rel, fmt.Sprintf("%dKB", info.Size()/1024),
				fmt.Sprintf("file is %dKB (limit: %dKB)", info.Size()/1024, maxFileKB))
		}
		ext := strings.ToLower(filepath.Ext(p))
		if binaries[ext] {
			add("binary_file", "critical", "structural", rel, "binary: "+ext, fmt.Sprintf("binary/executable file (%s) should not be in a skill", ext))
		}
		if !scripts[ext] && info.Mode()&0o111 != 0 {
			add("unexpected_executable", "medium", "structural", rel, "executable bit set", "file has executable permission but is not a recognized script type")
		}
		return nil
	})
	if count > maxFiles {
		add("too_many_files", "medium", "structural", "(directory)", fmt.Sprintf("%d files", count), fmt.Sprintf("skill has %d files (limit: %d)", count, maxFiles))
	}
	if total > maxTotalKB*1024 {
		add("oversized_skill", "low", "structural", "(directory)", fmt.Sprintf("%dKB total", total/1024),
			fmt.Sprintf("skill is %dKB total (limit: %dKB)", total/1024, maxTotalKB))
	}
	return findings
}

// Files a skill keeps out of scans and publishing, in gitignore style
var ignoreFiles = []string{".skillignore", ".clawhubignore"}

// Whether a path of the skill's folder is one its ignore files name; SKILL.md never is, the ignore files always are
func ignored(dir string) func(string) bool {
	var patterns []string
	for _, name := range ignoreFiles {
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(bytes.TrimPrefix(data, []byte("\uFEFF"))), "\n") {
			if line = strings.TrimSpace(line); line != "" && !strings.HasPrefix(line, "#") {
				patterns = append(patterns, line)
			}
		}
	}
	compiled := map[string]*regexp.Regexp{}
	match := func(name, pattern string) bool {
		re, ok := compiled[pattern]
		if !ok {
			re = tools.CompileGlob(pattern)
			compiled[pattern] = re
		}
		return re != nil && re.MatchString(name)
	}
	return func(rel string) bool {
		segments := strings.Split(rel, "/")
		base := segments[len(segments)-1]
		if base == "SKILL.md" {
			return false
		}
		if slices.Contains(ignoreFiles, base) {
			return true
		}
		for _, pattern := range patterns {
			anchored := strings.HasPrefix(pattern, "/")
			p := strings.Trim(pattern, "/")
			if p == "" {
				continue
			}
			below := strings.HasPrefix(rel, p+"/")
			if strings.HasSuffix(pattern, "/") {
				if rel == p || below || !anchored && strings.Contains("/"+rel+"/", "/"+p+"/") {
					return true
				}
				continue
			}
			if match(rel, p) {
				return true
			}
			if anchored {
				continue
			}
			if match(base, p) || below {
				return true
			}
			if !strings.Contains(p, "/") && slices.ContainsFunc(segments, func(s string) bool { return match(s, p) }) {
				return true
			}
		}
		return false
	}
}

// Prefixes skills.sh identifiers are written with, which say nothing of trust
var sourcePrefixes = []string{"skills-sh/", "skills.sh/", "skils-sh/", "skils.sh/"}

// Trust of a scan's source: the official skills built in, a trusted repository's or a path in it trusted, the agent's own its own
func trustOf(source string) string {
	src := stripPrefix(source, sourcePrefixes)
	switch src {
	case "agent-created":
		return "agent-created"
	case "official":
		return "builtin"
	}
	for repo := range trustedRepos {
		if src == repo || strings.HasPrefix(src, repo+"/") {
			return "trusted"
		}
	}
	return "community"
}

// Verdict of the worst finding: dangerous for a critical one, caution for a high one, safe otherwise
func verdictOf(findings []Finding) string {
	verdict := "safe"
	for _, f := range findings {
		switch f.Severity {
		case "critical":
			return "dangerous"
		case "high":
			verdict = "caution"
		}
	}
	return verdict
}

// One line naming the verdict and the kinds of findings
func summary(name, verdict string, findings []Finding) string {
	if len(findings) == 0 {
		return name + ": clean scan, no threats detected"
	}
	var categories []string
	for _, f := range findings {
		if !slices.Contains(categories, f.Category) {
			categories = append(categories, f.Category)
		}
	}
	slices.Sort(categories)
	return fmt.Sprintf("%s: %s - %d finding(s) in %s", name, verdict, len(findings), strings.Join(categories, ", "))
}
