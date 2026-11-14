package prompt

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/nickheyer/icebath/internal/fileutil"
	"github.com/nickheyer/icebath/internal/home"
	"github.com/nickheyer/icebath/internal/tool"
	"github.com/nickheyer/icebath/internal/tool/env"
)

const (
	contextFileReadTimeout               = 5 * time.Second
	contextFileMaxChars                  = 20_000
	contextTruncateHeadRatio     float64 = 0.7
	contextTruncateTailRatio     float64 = 0.2
	contextFileWindowFraction    float64 = 0.06
	contextFileDynamicCeiling            = 500_000
	charsPerToken                        = 4
	omittedHeadingLimit                  = 15
	byteOrderMark                        = string(rune(0xFEFF))
	legacyProtocolHeading                = "## Messaging other agents"
	soulFileName                         = "SOUL.md"
	contextKindIcebath                   = "icebath_md"
	contextKindAgents                    = "agents_md"
	contextKindClaude                    = "claude_md"
	contextKindCursor                    = "cursorrules"
	projectContextHeader                 = "# Project Context\n\nThe following project context files have been loaded and should be followed:\n\n"
	truncationWarningSign                = "⚠" + string(rune(0xFE0F))
	truncationRemedyQueued               = "trim the file, pin a larger context_file_max_chars, or use a larger-context model!"
	truncationRemedyReadablePref         = "the full file stays readable with read_file: "
)

var (
	headingLine       = regexp.MustCompile(`^#{1,6} `)
	legacyProtocolRun = regexp.MustCompile(`\n*` + regexp.QuoteMeta(legacyProtocolHeading) + `[ \t]*\n`)
)

type Distributions interface {
	Owns(ctx context.Context, profileHome, name string) (bool, error)
}

type SourceStatus string

const (
	SourceLoaded     SourceStatus = "loaded"
	SourceTruncated  SourceStatus = "truncated"
	SourceShadowed   SourceStatus = "shadowed"
	SourceBlocked    SourceStatus = "blocked"
	SourceFlagged    SourceStatus = "flagged"
	SourceEmpty      SourceStatus = "empty"
	SourceUnreadable SourceStatus = "unreadable"
	SourceSuppressed SourceStatus = "suppressed"
)

type Source struct {
	Label     string       `json:"label"`
	Path      string       `json:"path"`
	Chars     int          `json:"chars"`
	EstTokens int          `json:"est_tokens"`
	Loaded    bool         `json:"loaded"`
	Status    SourceStatus `json:"status"`
}

type ContextQuery struct {
	Cwd                 string
	ContextLength       int
	SkipSoul            bool
	InstallTreeFallback bool
}

type candidate struct {
	kind    string
	label   string
	path    string
	content string
}

type truncation struct {
	maxChars int
	warnings *[]string
}

func (b *Builder) readTimeout(ctx context.Context) time.Duration {
	cfg, err := b.cfg.LoadReadonly(ctx)
	if err != nil {
		slog.Debug(fmt.Sprintf("Could not read context_file_read_timeout from config: %v", err))
		return contextFileReadTimeout
	}
	if cfg.ContextFileReadTimeout > 0 {
		return time.Duration(cfg.ContextFileReadTimeout * float64(time.Second))
	}
	return contextFileReadTimeout
}

func dynamicContextFileMaxChars(contextLength int) int {
	if contextLength <= 0 {
		return contextFileMaxChars
	}
	budget := int(float64(contextLength*charsPerToken) * contextFileWindowFraction)
	return max(contextFileMaxChars, min(budget, contextFileDynamicCeiling))
}

func (b *Builder) maxChars(ctx context.Context, contextLength int) int {
	cfg, err := b.cfg.LoadReadonly(ctx)
	if err != nil {
		slog.Debug(fmt.Sprintf("Could not read context_file_max_chars from config: %v", err))
		return dynamicContextFileMaxChars(contextLength)
	}
	if cfg.ContextFileMaxChars != nil && *cfg.ContextFileMaxChars > 0 {
		return *cfg.ContextFileMaxChars
	}
	return dynamicContextFileMaxChars(contextLength)
}

type readResult struct {
	text string
	err  error
}

func readTextWithin(ctx context.Context, path string, timeout time.Duration) (string, bool, error) {
	done := make(chan readResult, 1)
	go func() {
		text, err := fileutil.ReadText(path)
		done <- readResult{text, err}
	}()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case r := <-done:
		return r.text, true, r.err
	case <-timer.C:
		slog.Warn(fmt.Sprintf("Context file %s read timed out after %.1fs; skipping", path, timeout.Seconds()))
		return "", false, nil
	case <-ctx.Done():
		return "", false, ctx.Err()
	}
}

func (b *Builder) readContextFile(ctx context.Context, path string) (string, error) {
	if !exists(path) {
		return "", nil
	}
	text, _, err := readTextWithin(ctx, path, b.readTimeout(ctx))
	if err != nil {
		if cerr := ctx.Err(); cerr != nil {
			return "", cerr
		}
		slog.Debug(fmt.Sprintf("Could not read %s: %v", path, err))
		return "", nil
	}
	return trim(text), nil
}

func scanContextContent(content, filename string, userAuthored bool) string {
	content = strings.TrimPrefix(content, byteOrderMark)
	findings := tool.ScanThreats(content, tool.ThreatContext)
	if len(findings) == 0 {
		return content
	}
	joined := strings.Join(findings, ", ")
	if userAuthored {
		slog.Warn(fmt.Sprintf("Context file %s matched injection pattern(s) %s; loaded anyway because it is the "+
			"user's own file in ICEBATH_HOME — review it if you did not write that text", filename, joined))
		return content
	}
	slog.Warn(fmt.Sprintf("Context file %s blocked: %s", filename, joined))
	return "[BLOCKED: " + filename + " contained potential prompt injection (" + joined + "). Content not loaded.]"
}

func findGitRoot(start string) (string, bool) {
	for _, p := range ancestors(resolvePath(start)) {
		if exists(filepath.Join(p, ".git")) {
			return p, true
		}
	}
	return "", false
}

func findIcebathMD(cwd string) (string, bool) {
	stopAt, rooted := findGitRoot(cwd)
	current := resolvePath(cwd)
	dirs := []string{current}
	if rooted {
		dirs = ancestors(current)
	}
	for _, dir := range dirs {
		for _, name := range []string{".icebath.md", "ICEBATH.md"} {
			if p := filepath.Join(dir, name); isFile(p) {
				return p, true
			}
		}
		if rooted && dir == stopAt {
			return "", false
		}
	}
	return "", false
}

func stripYAMLFrontmatter(content string) string {
	content = strings.TrimLeft(content, byteOrderMark)
	if !strings.HasPrefix(content, "---") {
		return content
	}
	end := strings.Index(content[3:], "\n---")
	if end < 0 {
		return content
	}
	if body := strings.TrimLeft(content[3+end+4:], "\n"); body != "" {
		return body
	}
	return content
}

func omittedHeadings(content []rune, start, end int) []string {
	var headings []string
	offset, fenced := 0, false
	for _, line := range splitLinesKeep(string(content)) {
		stripped := trim(line)
		switch {
		case strings.HasPrefix(stripped, "```"), strings.HasPrefix(stripped, "~~~"):
			fenced = !fenced
		case !fenced && start <= offset && offset < end:
			if loc := headingLine.FindStringIndex(stripped); loc != nil {
				if r, _ := utf8.DecodeRuneInString(stripped[loc[1]:]); loc[1] < len(stripped) && !isSpace(r) {
					headings = append(headings, trim(strings.TrimLeft(stripped, "#")))
				}
			}
		}
		offset += utf8.RuneCountInString(line)
	}
	if len(headings) > omittedHeadingLimit {
		return append(headings[:omittedHeadingLimit:omittedHeadingLimit], "...")
	}
	return headings
}

func truncateContent(content, filename, readPath string, t truncation) string {
	runes := []rune(content)
	if len(runes) <= t.maxChars {
		return content
	}
	if readPath == "" {
		readPath = filename
	}
	remedy := truncationRemedyReadablePref + readPath
	if t.warnings != nil {
		remedy = truncationRemedyQueued
	}
	msg := truncationWarningSign + "  Context file " + filename + " TRUNCATED: " + strconv.Itoa(len(runes)) +
		" chars exceeds limit of " + strconv.Itoa(t.maxChars) + " — " + remedy
	slog.Warn(msg)
	if t.warnings != nil {
		*t.warnings = append(*t.warnings, msg)
	}
	headChars := int(float64(t.maxChars) * contextTruncateHeadRatio)
	tailChars := int(float64(t.maxChars) * contextTruncateTailRatio)
	omitted := omittedHeadings(runes, headChars, len(runes)-tailChars)
	sections := ""
	if len(omitted) > 0 {
		sections = " Omitted sections: " + strings.Join(omitted, "; ") + "."
	}
	marker := "\n\n[...truncated " + filename + ": kept " + strconv.Itoa(headChars) + "+" + strconv.Itoa(tailChars) + " of " +
		strconv.Itoa(len(runes)) + " chars. The middle is " +
		"omitted." + sections + " If you need the full instructions, read the complete file with the read_file " +
		"tool: " + readPath + "]\n\n"
	tail := runes
	if tailChars > 0 {
		tail = runes[len(runes)-tailChars:]
	}
	return string(runes[:headChars]) + marker + string(tail)
}

func stripLegacyProtocol(text string) string {
	if !strings.Contains(text, legacyProtocolHeading) {
		return text
	}
	var out strings.Builder
	pos := 0
	for pos < len(text) {
		loc := legacyProtocolRun.FindStringIndex(text[pos:])
		if loc == nil {
			break
		}
		out.WriteString(text[pos : pos+loc[0]])
		out.WriteString("\n")
		body := pos + loc[1]
		next := strings.Index(text[body:], "\n## ")
		if next < 0 {
			pos = len(text)
			break
		}
		pos = body + next
	}
	out.WriteString(text[pos:])
	return trim(out.String()) + "\n"
}

func (b *Builder) loadSoul(ctx context.Context, t truncation) (string, error) {
	if b.safeWorker {
		return "", nil
	}
	if err := b.cfg.EnsureHome(ctx); err != nil {
		if cerr := ctx.Err(); cerr != nil {
			return "", cerr
		}
		slog.Debug(fmt.Sprintf("Could not ensure ICEBATH_HOME before loading SOUL.md: %v", err))
	}
	h, err := b.homes.Home(ctx)
	if err != nil {
		return "", err
	}
	soulPath := home.Soul.In(h)
	if !exists(soulPath) {
		return "", nil
	}
	text, _, err := readTextWithin(ctx, soulPath, b.readTimeout(ctx))
	if err != nil {
		if cerr := ctx.Err(); cerr != nil {
			return "", cerr
		}
		slog.Debug(fmt.Sprintf("Could not read SOUL.md from %s: %v", soulPath, err))
		return "", nil
	}
	content := trim(text)
	if content != "" {
		content = trim(stripLegacyProtocol(content))
	}
	if content == "" {
		return "", nil
	}
	if b.dists == nil {
		return "", &tool.UnfilledError{Name: "prompt.Options.Distributions"}
	}
	userAuthored := false
	owned, err := b.dists.Owns(ctx, filepath.Dir(soulPath), soulFileName)
	if err != nil {
		if cerr := ctx.Err(); cerr != nil {
			return "", cerr
		}
		slog.Debug(fmt.Sprintf("Could not read distribution manifest next to %s: %v", soulPath, err))
	} else {
		userAuthored = !owned
	}
	return truncateContent(scanContextContent(content, soulFileName, userAuthored), soulFileName, soulPath, t), nil
}

func (b *Builder) icebathMDCandidates(ctx context.Context, cwd string) ([]candidate, error) {
	path, ok := findIcebathMD(cwd)
	if !ok {
		return nil, nil
	}
	label := filepath.Base(path)
	if rel, ok := within(path, cwd); ok {
		label = rel
	}
	content, err := b.readContextFile(ctx, path)
	if err != nil {
		return nil, err
	}
	return []candidate{{contextKindIcebath, label, path, content}}, nil
}

func agentsMDDirectoryChain(cwd string) []string {
	current := resolvePath(cwd)
	root, ok := findGitRoot(current)
	if !ok || root == current {
		return []string{current}
	}
	rel, ok := within(current, root)
	if !ok {
		return []string{current}
	}
	chain := []string{root}
	dir := root
	for _, part := range strings.Split(rel, string(filepath.Separator)) {
		dir = filepath.Join(dir, part)
		chain = append(chain, dir)
	}
	return chain
}

func (b *Builder) agentsMDCandidates(ctx context.Context, cwd string) ([]candidate, error) {
	resolved := resolvePath(cwd)
	var found []candidate
	for _, dir := range agentsMDDirectoryChain(resolved) {
		for _, name := range []string{"AGENTS.override.md", "AGENTS.md", "agents.md"} {
			path := filepath.Join(dir, name)
			if !exists(path) {
				continue
			}
			content, err := b.readContextFile(ctx, path)
			if err != nil {
				return nil, err
			}
			label := name
			if dir != resolved {
				if rel, err := filepath.Rel(resolved, path); err == nil {
					label = rel
				} else {
					label = path
				}
			}
			found = append(found, candidate{contextKindAgents, label, path, content})
			if content != "" {
				break
			}
		}
	}
	return found, nil
}

func (b *Builder) claudeMDCandidates(ctx context.Context, cwd string) ([]candidate, error) {
	var found []candidate
	for _, name := range []string{"CLAUDE.md", "claude.md"} {
		path := filepath.Join(cwd, name)
		if !exists(path) {
			continue
		}
		content, err := b.readContextFile(ctx, path)
		if err != nil {
			return nil, err
		}
		found = append(found, candidate{contextKindClaude, name, path, content})
		if content != "" {
			break
		}
	}
	return found, nil
}

func cursorRuleFiles(dir string) []string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	fold := func(s string) string {
		if runtime.GOOS == "windows" {
			return strings.ToLower(s)
		}
		return s
	}
	var names []string
	for _, e := range entries {
		if strings.HasSuffix(fold(e.Name()), ".mdc") {
			names = append(names, e.Name())
		}
	}
	slices.SortFunc(names, func(a, c string) int { return strings.Compare(fold(a), fold(c)) })
	return names
}

func (b *Builder) cursorrulesCandidates(ctx context.Context, cwd string) ([]candidate, error) {
	type entry struct{ label, path string }
	entries := []entry{{".cursorrules", filepath.Join(cwd, ".cursorrules")}}
	rulesDir := filepath.Join(cwd, ".cursor", "rules")
	if isDir(rulesDir) {
		for _, name := range cursorRuleFiles(rulesDir) {
			entries = append(entries, entry{".cursor/rules/" + name, filepath.Join(rulesDir, name)})
		}
	}
	var found []candidate
	for _, e := range entries {
		if !exists(e.path) {
			continue
		}
		content, err := b.readContextFile(ctx, e.path)
		if err != nil {
			return nil, err
		}
		found = append(found, candidate{contextKindCursor, e.label, e.path, content})
	}
	return found, nil
}

func (b *Builder) discoverContextFiles(ctx context.Context, cwd string) ([]candidate, error) {
	var all []candidate
	for _, finder := range []func(context.Context, string) ([]candidate, error){
		b.icebathMDCandidates, b.agentsMDCandidates, b.claudeMDCandidates, b.cursorrulesCandidates,
	} {
		found, err := finder(ctx, cwd)
		if err != nil {
			return nil, err
		}
		all = append(all, found...)
	}
	return all, nil
}

func contextSection(content, label, warnName, path string, t truncation) string {
	body := "## " + label + "\n\n" + scanContextContent(content, label, false)
	return truncateContent(body, warnName, path, t)
}

func (b *Builder) loadIcebathMD(ctx context.Context, cwd string, t truncation) (string, error) {
	found, err := b.icebathMDCandidates(ctx, cwd)
	if err != nil {
		return "", err
	}
	for _, c := range found {
		if c.content != "" {
			return contextSection(stripYAMLFrontmatter(c.content), c.label, ".icebath.md", c.path, t), nil
		}
	}
	return "", nil
}

func (b *Builder) loadAgentsMD(ctx context.Context, cwd string, t truncation) (string, error) {
	found, err := b.agentsMDCandidates(ctx, cwd)
	if err != nil {
		return "", err
	}
	var sections []string
	seen := map[string]bool{}
	for _, c := range found {
		if c.content == "" || seen[c.content] {
			continue
		}
		seen[c.content] = true
		sections = append(sections, contextSection(c.content, c.label, c.label, c.path, t))
	}
	switch len(sections) {
	case 0:
		return "", nil
	case 1:
		return sections[0], nil
	}
	return truncateContent(strings.Join(sections, "\n\n"), "AGENTS.md (directory chain)",
		filepath.Join(resolvePath(cwd), "AGENTS.md"), t), nil
}

func (b *Builder) loadClaudeMD(ctx context.Context, cwd string, t truncation) (string, error) {
	found, err := b.claudeMDCandidates(ctx, cwd)
	if err != nil {
		return "", err
	}
	for _, c := range found {
		if c.content != "" {
			return contextSection(c.content, c.label, "CLAUDE.md", c.path, t), nil
		}
	}
	return "", nil
}

func (b *Builder) loadCursorrules(ctx context.Context, cwd string, t truncation) (string, error) {
	found, err := b.cursorrulesCandidates(ctx, cwd)
	if err != nil {
		return "", err
	}
	var body strings.Builder
	for _, c := range found {
		if c.content != "" {
			body.WriteString("## " + c.label + "\n\n" + scanContextContent(c.content, c.label, false) + "\n\n")
		}
	}
	if body.Len() == 0 {
		return "", nil
	}
	return truncateContent(body.String(), ".cursorrules", filepath.Join(cwd, ".cursorrules"), t), nil
}

func (b *Builder) contextCwd(q ContextQuery) (string, bool, error) {
	if q.Cwd != "" {
		return resolvePath(q.Cwd), false, nil
	}
	wd, err := os.Getwd()
	if err != nil {
		return "", false, err
	}
	cwd := resolvePath(wd)
	return cwd, !q.InstallTreeFallback && env.IsInstallTree(cwd), nil
}

func (b *Builder) contextFiles(ctx context.Context, q ContextQuery, t truncation) (string, error) {
	if b.safeWorker {
		return "", nil
	}
	cwd, suppressed, err := b.contextCwd(q)
	if err != nil {
		return "", err
	}
	var sections []string
	if suppressed {
		slog.Warn(fmt.Sprintf("skipping project-context discovery: working-directory resolution fell back to the icebath "+
			"install tree (%s) — set terminal.cwd to your project directory", cwd))
	} else {
		for _, load := range []func(context.Context, string, truncation) (string, error){
			b.loadIcebathMD, b.loadAgentsMD, b.loadClaudeMD, b.loadCursorrules,
		} {
			section, err := load(ctx, cwd, t)
			if err != nil {
				return "", err
			}
			if section != "" {
				sections = append(sections, section)
				break
			}
		}
	}
	if !q.SkipSoul {
		soul, err := b.loadSoul(ctx, t)
		if err != nil {
			return "", err
		}
		if soul != "" {
			sections = append(sections, soul)
		}
	}
	if len(sections) == 0 {
		return "", nil
	}
	return projectContextHeader + strings.Join(sections, "\n"), nil
}

func (b *Builder) ContextFiles(ctx context.Context, q ContextQuery) (string, []string, error) {
	var warnings []string
	text, err := b.contextFiles(ctx, q, truncation{maxChars: b.maxChars(ctx, q.ContextLength), warnings: &warnings})
	if err != nil {
		return "", nil, err
	}
	return text, warnings, nil
}

func emptyStatus(path string) SourceStatus {
	info, err := os.Stat(path)
	if err != nil || info.Size() > 0 {
		return SourceUnreadable
	}
	return SourceEmpty
}

func loadedStatus(content string, renderedLen, maxChars int, userAuthored bool) SourceStatus {
	if len(tool.ScanThreats(strings.TrimLeft(content, byteOrderMark), tool.ThreatContext)) > 0 {
		if userAuthored {
			return SourceFlagged
		}
		return SourceBlocked
	}
	if renderedLen > maxChars {
		return SourceTruncated
	}
	return SourceLoaded
}

func sourceEntry(label, path, content string, status SourceStatus) Source {
	return Source{Label: label, Path: path, Chars: utf8.RuneCountInString(content), EstTokens: roughTokens(content),
		Loaded: status == SourceLoaded || status == SourceTruncated || status == SourceFlagged, Status: status}
}

func (b *Builder) Sources(ctx context.Context, q ContextQuery) ([]Source, error) {
	cwd, suppressed, err := b.contextCwd(q)
	if err != nil {
		return nil, err
	}
	maxChars := b.maxChars(ctx, q.ContextLength)
	found, err := b.discoverContextFiles(ctx, cwd)
	if err != nil {
		return nil, err
	}
	sources := []Source{}
	winner := ""
	for _, c := range found {
		var status SourceStatus
		switch {
		case c.content == "":
			status = emptyStatus(c.path)
		case suppressed:
			status = SourceSuppressed
		case winner == "" || winner == c.kind:
			winner = c.kind
			status = loadedStatus(c.content, utf8.RuneCountInString("## "+c.label+"\n\n"+c.content), maxChars, false)
		default:
			status = SourceShadowed
		}
		sources = append(sources, sourceEntry(c.label, c.path, c.content, status))
	}
	if q.SkipSoul {
		return sources, nil
	}
	h, err := b.homes.Home(ctx)
	if err != nil {
		return nil, err
	}
	soulPath := home.Soul.In(h)
	if !exists(soulPath) {
		return sources, nil
	}
	content, err := b.readContextFile(ctx, soulPath)
	if err != nil {
		return nil, err
	}
	status := emptyStatus(soulPath)
	if content != "" {
		status = loadedStatus(content, utf8.RuneCountInString(content), maxChars, true)
	}
	return append(sources, sourceEntry(soulFileName, soulPath, content, status)), nil
}

func (b *Builder) agentContextQuery(ctx context.Context, in Inputs, skipSoul bool) (ContextQuery, error) {
	cwd, _, err := env.ResolveContextCwd(ctx, b.env.Settings(), !in.LaunchArtifact)
	if err != nil {
		return ContextQuery{}, err
	}
	platform := in.Platform
	return ContextQuery{Cwd: cwd, ContextLength: in.ContextLength, SkipSoul: skipSoul,
		InstallTreeFallback: platform == "cli" || platform == "tui"}, nil
}

func (b *Builder) AgentSources(ctx context.Context, in Inputs) ([]Source, error) {
	if in.SkipContextFiles {
		return []Source{}, nil
	}
	q, err := b.agentContextQuery(ctx, in, false)
	if err != nil {
		return nil, err
	}
	return b.Sources(ctx, q)
}

type sourceDisplay struct {
	glyph string
	note  string
}

var sourceDisplays = map[SourceStatus]sourceDisplay{
	SourceLoaded:     {"✓", ""},
	SourceTruncated:  {"◐", "truncated — over context_file_max_chars"},
	SourceShadowed:   {"○", "not loaded — higher-priority context type wins"},
	SourceBlocked:    {"✗", "not loaded — blocked by the prompt-injection scan"},
	SourceFlagged:    {"⚠", "loaded — matched prompt-injection pattern(s); review the file"},
	SourceEmpty:      {"○", "not loaded — empty file"},
	SourceUnreadable: {"✗", "not loaded — could not be read"},
	SourceSuppressed: {"○", "not loaded — cwd fell back to the icebath install tree"},
}

func groupThousands(n int) string {
	digits := strconv.Itoa(n)
	sign := ""
	if strings.HasPrefix(digits, "-") {
		sign, digits = "-", digits[1:]
	}
	var out strings.Builder
	for i, d := range digits {
		if i > 0 && (len(digits)-i)%3 == 0 {
			out.WriteByte(',')
		}
		out.WriteRune(d)
	}
	return sign + out.String()
}

func RenderSources(sources []Source) []string {
	if len(sources) == 0 {
		return []string{}
	}
	width := 0
	for _, s := range sources {
		width = max(width, utf8.RuneCountInString(s.Label))
	}
	lines := []string{"Context files"}
	for _, s := range sources {
		d, ok := sourceDisplays[s.Status]
		if !ok {
			d = sourceDisplay{"•", ""}
		}
		suffix := ""
		if d.note != "" {
			suffix = "  (" + d.note + ")"
		}
		count := groupThousands(s.EstTokens)
		lines = append(lines, d.glyph+" "+s.Label+strings.Repeat(" ", width-utf8.RuneCountInString(s.Label))+
			" ~"+strings.Repeat(" ", max(0, 9-len(count)))+count+" tokens"+suffix)
	}
	return lines
}
