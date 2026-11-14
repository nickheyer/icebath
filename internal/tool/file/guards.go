package file

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"unicode"

	"github.com/nickheyer/icebath/internal/home"
	"github.com/nickheyer/icebath/internal/tool"
	"github.com/nickheyer/icebath/internal/tool/approval"
	"github.com/nickheyer/icebath/internal/tool/env"
)

const (
	readDedupStatusMessage = "File unchanged since last read. The content from " +
		"the earlier read_file result in this conversation is " +
		"still current — refer to that instead of re-reading."
	approvalUnavailable   = "requires approval but the approval subsystem is unavailable."
	noHumanPresent        = "requires approval but no interactive user or gateway is present to approve it."
	instructionPatternKey = "protected_instruction_file"
	sshConfigPatternKey   = "ssh_config_write"
	fileStateExists       = "exists"
	fileStateAbsent       = "absent"
	fileStateUnavailable  = "unavailable"
)

var sensitivePathPrefixes = [...]string{
	"/etc/", "/boot/", "/usr/lib/systemd/",
	"/private/etc/",
	"/private/var/db/", "/private/var/root/",
}

type Confirmer interface {
	ApproveOnce(ctx context.Context, c *tool.Call, p tool.ApprovalPrompt) Confirmation
}

type Confirmation = approval.Confirmation

var _ Confirmer = (*approval.Gate)(nil)

type SkillLock interface {
	PathRefusal(ctx context.Context, paths []string) (string, error)
}

func isSensitiveExactPath(p string) bool {
	return p == "/var/run/docker.sock" || p == "/run/docker.sock"
}

func isProtectedInstructionBasename(lower string) bool {
	switch lower {
	case "agents.md", "claude.md", "soul.md", ".cursorrules":
		return true
	}
	return false
}

func dedupe(items []string) []string {
	seen := make(map[string]struct{}, len(items))
	var out []string
	for _, item := range items {
		if _, dup := seen[item]; !dup {
			seen[item] = struct{}{}
			out = append(out, item)
		}
	}
	return out
}

func (t *Tools) configResolved(ctx context.Context) string {
	if configPath, _, err := t.config.Paths(ctx); err == nil && configPath != "" {
		return realPath(configPath)
	}
	active, err := t.homes.Home(ctx)
	if err != nil {
		return ""
	}
	return realPath(home.Config.In(active))
}

func (t *Tools) exemptHomes(ctx context.Context) []string {
	active, err := t.homes.Home(ctx)
	if err != nil {
		return nil
	}
	resolved := realPath(active)
	profileHome, err := home.NamedProfileHome(resolved)
	if err != nil || profileHome == "" {
		return []string{resolved}
	}
	root := realPath(filepath.Dir(filepath.Dir(profileHome)))
	if root != "" && root != resolved {
		return []string{resolved, root}
	}
	return []string{resolved}
}

func (t *Tools) resolvedOrRaw(ctx context.Context, path, taskID string) string {
	resolved, err := t.paths.Resolve(ctx, path, taskID)
	if err != nil {
		return path
	}
	return resolved
}

func (t *Tools) checkSensitivePath(ctx context.Context, path, taskID string) (string, error) {
	if msg := NTNamespaceError(path, "Write"); msg != "" {
		return msg, nil
	}
	candidates := []string{t.resolvedOrRaw(ctx, path, taskID), hostNorm(t.paths.ExpandTilde(ctx, path))}
	if sshPathEscapesHome(candidates[0]) && t.paths.EnvKind(ctx, taskID) == "ssh" {
		return "Refusing to write to " + path + ": it climbs above the SSH home and the remote " +
			"home could not be detected, so its target cannot be checked. Pass an absolute path.", nil
	}
	for _, c := range candidates {
		sensitive := isSensitiveExactPath(c)
		for _, prefix := range sensitivePathPrefixes {
			sensitive = sensitive || strings.HasPrefix(c, prefix)
		}
		if sensitive {
			return "Refusing to write to sensitive system path: " + path + "\n" +
				"Use the terminal tool with sudo if you need to modify system files.", nil
		}
	}
	if configFile := t.configResolved(ctx); configFile != "" && (configFile == candidates[0] || configFile == candidates[1]) {
		return "Refusing to write to icebath config file: " + path + "\n" +
			"Agent cannot modify security-sensitive configuration. " +
			"Edit ~/.icebath/config.yaml directly or use 'icebath config' instead.", nil
	}
	reason, err := t.skillLock.PathRefusal(ctx, candidates)
	if err != nil {
		return "", err
	}
	if reason != "" {
		return "Refusing to write to " + path + ": " + reason, nil
	}
	return "", nil
}

func (t *Tools) protectedInstructionConfig(ctx context.Context) (bool, []string) {
	cfg, err := t.config.Load(ctx)
	if err != nil {
		return true, nil
	}
	var extra []string
	for _, pattern := range cfg.Security.ProtectedInstructionExtraPatterns {
		if pattern != "" {
			extra = append(extra, pattern)
		}
	}
	return cfg.Security.ProtectedInstructionFiles, extra
}

func (t *Tools) protectedInstructionReason(ctx context.Context, path, taskID string, extra []string) string {
	normalized := hostNorm(t.paths.ExpandTilde(ctx, path))
	resolved := realPath(normalized)
	if anchored, err := t.paths.Resolve(ctx, path, taskID); err == nil {
		resolved = realPath(anchored)
	}
	for _, exempt := range t.exemptHomes(ctx) {
		if resolved == exempt || strings.HasPrefix(resolved, exempt+string(filepath.Separator)) {
			return ""
		}
	}
	for _, candidate := range [...]string{normalized, resolved} {
		base := hostStyle().base(candidate)
		lower := strings.ToLower(base)
		if isProtectedInstructionBasename(lower) {
			return base
		}
		for _, pattern := range extra {
			if fnmatch(lower, strings.ToLower(pattern)) {
				return base
			}
		}
		parts := strings.Split(strings.TrimRight(strings.ReplaceAll(candidate, `\`, "/"), "/"), "/")
		if len(parts) >= 2 && parts[len(parts)-2] == ".icebath" {
			return candidate
		}
	}
	return ""
}

func (t *Tools) requestInstructionApproval(ctx context.Context, c *tool.Call, reasons []string) string {
	targets := strings.Join(dedupe(reasons), ", ")
	description := "Write to protected agent-instruction file(s): " + targets + ". " +
		"These files steer future agent behavior; approval is always " +
		"required (not bypassed by auto-approve)."
	display := "<write to " + targets + ">"
	blocked := func(why string) string {
		return "BLOCKED: write to protected agent-instruction file(s) (" + targets + ") " +
			why + " The user has NOT consented to this write. Do NOT retry it or " +
			"attempt the same edit via another path (terminal, execute_code, " +
			"etc.)."
	}
	verdict := t.confirm.ApproveOnce(ctx, c, tool.ApprovalPrompt{
		Command:     display,
		Description: description,
		PatternKey:  instructionPatternKey,
		PatternKeys: []string{instructionPatternKey},
	})
	timed := false
	if verdict.Gateway {
		if verdict.NotifyFailed {
			return blocked("requires approval but the approval request could not be delivered.")
		}
		if verdict.Withdrawn != "" {
			return blocked("approval was withdrawn before the user answered (" + verdict.Withdrawn + ").")
		}
		timed = !verdict.Resolved
	} else {
		if verdict.NoHuman {
			return blocked(noHumanPresent)
		}
		if verdict.Choice == "cancelled" {
			cause := verdict.Cause
			if cause == "" {
				cause = "no answer"
			}
			return blocked("approval prompt could not be delivered or was not answered (" + cause + ").")
		}
		timed = verdict.Choice == "timeout"
	}
	if timed {
		return blocked("approval prompt timed out without a user response. Silence is not consent.")
	}
	switch verdict.Choice {
	case "once", "session", "always":
		return ""
	}
	return blocked("was denied by the user.")
}

func (t *Tools) checkProtectedInstructionWrite(ctx context.Context, c *tool.Call, paths []string, taskID string) string {
	enabled, extra := t.protectedInstructionConfig(ctx)
	if !enabled {
		return ""
	}
	var reasons []string
	for _, p := range paths {
		if reason := t.protectedInstructionReason(ctx, p, taskID, extra); reason != "" {
			reasons = append(reasons, reason)
		}
	}
	if len(reasons) == 0 {
		return ""
	}
	return t.requestInstructionApproval(ctx, c, reasons)
}

func (t *Tools) checkApprovalRequiredWrite(ctx context.Context, c *tool.Call, paths []string, taskID string) string {
	var targets []string
	for _, p := range paths {
		if t.safety.WriteApprovalRequired(ctx, t.resolvedOrRaw(ctx, p, taskID)) {
			targets = append(targets, p)
		}
	}
	if len(targets) == 0 {
		return ""
	}
	display := strings.Join(dedupe(targets), ", ")
	description := "Write to SSH client config file(s): " + display + ". " +
		"The SSH config can carry ProxyCommand / Match exec directives that " +
		"run commands, so writes require your approval."
	blocked := func(why string) string {
		return "BLOCKED: write to SSH config file(s) (" + display + ") " +
			why + " Do NOT retry it via another path (terminal, execute_code) " +
			"without the user's explicit consent."
	}
	if c.Approver == nil {
		return blocked(approvalUnavailable)
	}
	decision := c.Approver.Action(ctx, c, tool.ActionRequest{
		Tool:        sshConfigPatternKey,
		PatternKey:  sshConfigPatternKey,
		Description: description,
		Target:      "<write to " + display + ">",
		DenyCron:    blocked("requires approval but this cron session denies it."),
		DenySingleQuery: blocked("requires approval but single-query (-q) sessions run " +
			"without a user present to approve it. To allow flagged " +
			"actions in single-query mode, set approvals.single_query_mode: " +
			"approve in config.yaml."),
		FailClosed: true,
		NoHuman:    blocked(noHumanPresent),
	})
	if decision.Approved {
		return ""
	}
	if decision.Message != "" {
		return decision.Message
	}
	return blocked("was denied.")
}

func (t *Tools) containerMirrorPrefix(ctx context.Context, taskID string) string {
	live, ok, err := t.terminal.Active(ctx, taskID)
	if err != nil {
		return ""
	}
	if ok {
		if live.Kind() == "docker" && live.Persistent() {
			return env.DefaultContainerBase
		}
		return ""
	}
	cfg, err := t.terminal.Settings().EnvConfig(ctx)
	if err != nil {
		return ""
	}
	if cfg.Kind == "docker" && cfg.ContainerPersistent {
		return env.DefaultContainerBase
	}
	return ""
}

func (t *Tools) checkMirrorPath(ctx context.Context, path, taskID string) string {
	resolved := t.resolvedOrRaw(ctx, path, taskID)
	if warning := SandboxMirrorWarning(resolved); warning != "" {
		return warning
	}
	return ContainerMirrorWarning(resolved, t.containerMirrorPrefix(ctx, taskID))
}

func (t *Tools) targetRegularFileState(ctx context.Context, c *tool.Call, path, taskID string) string {
	resolved, resolveErr := t.paths.Resolve(ctx, path, taskID)
	ops, err := t.ops(ctx, c)
	if err != nil {
		return fileStateUnavailable
	}
	if ops.env.HostFilesystem() {
		probe := resolved
		if resolveErr != nil {
			probe = t.paths.ExpandTilde(ctx, path)
		}
		if fi, err := os.Stat(probe); err == nil && fi.Mode().IsRegular() {
			return fileStateExists
		}
		return fileStateAbsent
	}
	target := resolved
	if resolveErr != nil || target == "" {
		target = path
	}
	expanded, err := ops.expandPath(ctx, target)
	if err != nil {
		return fileStateUnavailable
	}
	_, status, err := ops.probeRegularFile(ctx, expanded)
	if err != nil {
		return fileStateUnavailable
	}
	switch status {
	case statusOK, statusBadSize:
		return fileStateExists
	case statusMissing, statusNotRegular:
		return fileStateAbsent
	}
	return fileStateUnavailable
}

func (t *Tools) checkBinaryDocumentWrite(ctx context.Context, c *tool.Call, path, taskID string) string {
	ext := strings.ToLower(hostStyle().ext(path))
	if HasOpaqueDocumentExtension(path) {
		return "Refusing to write plain text to binary document '" + path + "' (" + ext + "). " +
			"A text write cannot produce a valid document container and would " +
			"corrupt the file (read_file showed you EXTRACTED text, not the real " +
			"bytes). Use the microsoft-office skill or a library like " +
			"python-docx/openpyxl/python-pptx via the terminal to create or edit " +
			"this document."
	}
	if IsSQLiteSidecar(path) {
		return "Refusing to write plain text to binary SQLite sidecar '" + path + "' (" + ext + "). " +
			"A -wal/-shm/-journal file holds raw database pages that SQLite " +
			"reads on the next open; text there corrupts the database. Use the " +
			"sqlite3 CLI or a SQLite library via the terminal to modify the " +
			"database instead."
	}
	pdf := IsPDFPath(path)
	if !pdf && !HasBinaryExtension(path) {
		return ""
	}
	switch t.targetRegularFileState(ctx, c, path, taskID) {
	case fileStateExists:
		if pdf {
			return "Refusing to overwrite existing PDF '" + path + "' with plain text. " +
				"read_file showed you EXTRACTED text, not the real bytes — writing " +
				"text back would destroy the document. Use the pdf skill or a PDF " +
				"library via the terminal to modify it. (Creating a NEW .pdf file " +
				"is allowed.)"
		}
		return "Refusing to overwrite existing binary file '" + path + "' (" + ext + ") " +
			"with plain text — read_file showed you extracted or mojibake " +
			"text, not the real bytes, and writing text back would destroy " +
			"the file. Use a binary-aware tool via the terminal to modify it " +
			"(for SQLite databases, the sqlite3 CLI or a SQLite library). " +
			"(Creating a NEW file with this extension is allowed.)"
	case fileStateUnavailable:
		return "Refusing to write to '" + path + "': could not establish whether " +
			"the target file already exists where this write would execute " +
			"(the terminal environment may be starting, unreachable, or was " +
			"removed). The file was NOT modified — retry once the environment " +
			"is reachable."
	}
	return ""
}

func (t *Tools) staleOverwriteBlocker(path, resolved, taskID string) string {
	if GuardDisabled() {
		return ""
	}
	if resolved != "" {
		if stale := t.tasks.state.CheckStale(taskID, resolved); stale != "" {
			return stale
		}
		if t.tasks.tracker.ReadModTimeDrifted(resolved, taskID) {
			return path + " was modified since you last read it (external edit or " +
				"concurrent agent). Re-read the file before writing."
		}
	}
	if resolved == "" || t.tasks.tracker.HasFullWriteBaseline(resolved, taskID) {
		return ""
	}
	if _, err := os.Stat(resolved); err != nil {
		return ""
	}
	if t.tasks.tracker.IsOwnBlindPatch(resolved, taskID) {
		return "Your patch changed this file without a full view of its current content. Read the current file " +
			"in full before a whole-file overwrite, or continue with targeted patches."
	}
	return resolved + " exists but this task has not seen its full current content " +
		"(never read, patched without a prior full read, or only a redacted/partial view). Read the " +
		"file — every page of it, if it needs offset/limit — or use patch for a " +
		"targeted edit; a stale conversation copy must not overwrite the current " +
		"disk content."
}

func staleWriteRefusal(path, reason, resolved string) fieldList {
	out := fieldList{
		{Key: "error", Value: "Refusing to overwrite " + path + ": " + reason + " " +
			"The file was NOT modified. Reload the current contents with read_file " +
			"(every page, for a file that needs offset/limit), merge the requested " +
			"change, then call write_file again. For small edits, prefer patch so " +
			"existing unrelated changes are preserved."},
		{Key: "stale_write_blocked", Value: true},
		{Key: "path", Value: path},
	}
	if resolved != "" {
		out.set("resolved_path", resolved)
	}
	return out
}

func isInternalFileStatusText(content string) bool {
	stripped := trimSpace(content)
	return stripped != "" && strings.Contains(stripped, readDedupStatusMessage) &&
		charLen(stripped) <= 2*charLen(readDedupStatusMessage)
}

func digitValue(r rune) (int, bool) {
	if r >= '0' && r <= '9' {
		return int(r - '0'), true
	}
	if r < 0x80 || !unicode.IsDigit(r) {
		return 0, false
	}
	for _, block := range unicode.Nd.R16 {
		if rune(block.Lo) <= r && r <= rune(block.Hi) {
			return int(r-rune(block.Lo)) % 10, true
		}
	}
	for _, block := range unicode.Nd.R32 {
		if rune(block.Lo) <= r && r <= rune(block.Hi) {
			return int(r-rune(block.Lo)) % 10, true
		}
	}
	return 0, false
}

func decimalText(s string) (string, bool) {
	if s == "" {
		return "", false
	}
	digits := make([]byte, 0, len(s))
	for _, r := range s {
		value, ok := digitValue(r)
		if !ok {
			return "", false
		}
		digits = append(digits, byte('0'+value))
	}
	text := strings.TrimLeft(string(digits), "0")
	if text == "" {
		text = "0"
	}
	return text, true
}

func decimalSuccessor(n string) string {
	digits := []byte(n)
	for i := len(digits) - 1; i >= 0; i-- {
		if digits[i] != '9' {
			digits[i]++
			return string(digits)
		}
		digits[i] = '0'
	}
	return "1" + string(digits)
}

func looksLikeNumberedReadContent(content string) bool {
	var lines []string
	for _, line := range splitLines(content, false) {
		if !isBlank(line) {
			lines = append(lines, line)
		}
	}
	if len(lines) < 2 {
		return false
	}
	var numbered []string
	for _, line := range lines {
		prefix, _, found := strings.Cut(trimLeftSpace(line), "|")
		if !found {
			continue
		}
		if number, ok := decimalText(prefix); ok {
			numbered = append(numbered, number)
		}
	}
	if len(numbered) < 2 || float64(len(numbered))/float64(len(lines)) < 0.6 {
		return false
	}
	consecutive := 0
	for i := 1; i < len(numbered); i++ {
		if numbered[i] == decimalSuccessor(numbered[i-1]) {
			consecutive++
		}
	}
	return consecutive >= len(numbered)-1
}

func isInternalFileToolContent(content string) bool {
	return isInternalFileStatusText(content) || looksLikeNumberedReadContent(content)
}

type classRange struct {
	lo, hi rune
}

type globToken struct {
	star, any, never, negate bool
	literal                  rune
	set                      []classRange
	class                    bool
}

func globClass(body []rune) globToken {
	var chunks [][]rune
	hyphen := false
	for _, r := range body {
		hyphen = hyphen || r == '-'
	}
	if !hyphen {
		chunks = [][]rune{body}
	} else {
		start := 0
		k := 1
		if body[0] == '!' {
			k = 2
		}
		for {
			at := -1
			for i := k; i < len(body); i++ {
				if body[i] == '-' {
					at = i
					break
				}
			}
			if at < 0 {
				break
			}
			chunks = append(chunks, body[start:at])
			start = at + 1
			k = at + 3
		}
		if rest := body[start:]; len(rest) > 0 {
			chunks = append(chunks, rest)
		} else {
			last := len(chunks) - 1
			chunks[last] = append(append([]rune(nil), chunks[last]...), '-')
		}
		for i := len(chunks) - 1; i > 0; i-- {
			prev, next := chunks[i-1], chunks[i]
			if prev[len(prev)-1] > next[0] {
				merged := append(append([]rune(nil), prev[:len(prev)-1]...), next[1:]...)
				chunks[i-1] = merged
				chunks = append(chunks[:i], chunks[i+1:]...)
			}
		}
	}
	if len(chunks) == 1 && len(chunks[0]) == 0 {
		return globToken{never: true}
	}
	if len(chunks) == 1 && len(chunks[0]) == 1 && chunks[0][0] == '!' {
		return globToken{any: true}
	}
	token := globToken{class: true}
	if len(chunks[0]) > 0 && chunks[0][0] == '!' {
		token.negate = true
		chunks[0] = chunks[0][1:]
	}
	for i, chunk := range chunks {
		first, last := 0, len(chunk)
		if i > 0 {
			first = 1
		}
		if i < len(chunks)-1 {
			last--
			token.set = append(token.set, classRange{chunk[len(chunk)-1], chunks[i+1][0]})
		}
		for j := first; j < last; j++ {
			token.set = append(token.set, classRange{chunk[j], chunk[j]})
		}
	}
	return token
}

func globTokens(pattern []rune) []globToken {
	var out []globToken
	n := len(pattern)
	for i := 0; i < n; {
		c := pattern[i]
		i++
		switch c {
		case '*':
			if len(out) == 0 || !out[len(out)-1].star {
				out = append(out, globToken{star: true})
			}
		case '?':
			out = append(out, globToken{any: true})
		case '[':
			j := i
			if j < n && pattern[j] == '!' {
				j++
			}
			if j < n && pattern[j] == ']' {
				j++
			}
			for j < n && pattern[j] != ']' {
				j++
			}
			if j >= n {
				out = append(out, globToken{literal: '['})
				continue
			}
			out = append(out, globClass(pattern[i:j]))
			i = j + 1
		default:
			out = append(out, globToken{literal: c})
		}
	}
	return out
}

func (g globToken) matches(r rune) bool {
	switch {
	case g.never:
		return false
	case g.any:
		return true
	case g.class:
		in := false
		for _, span := range g.set {
			if span.lo <= r && r <= span.hi {
				in = true
				break
			}
		}
		return in != g.negate
	}
	return g.literal == r
}

func fnmatch(name, pattern string) bool {
	if runtime.GOOS == "windows" {
		name = strings.ReplaceAll(strings.ToLower(name), "/", `\`)
		pattern = strings.ReplaceAll(strings.ToLower(pattern), "/", `\`)
	}
	text := []rune(name)
	tokens := globTokens([]rune(pattern))
	ti, pi := 0, 0
	starToken, starText := -1, 0
	for ti < len(text) {
		switch {
		case pi < len(tokens) && tokens[pi].star:
			starToken, starText = pi, ti
			pi++
		case pi < len(tokens) && tokens[pi].matches(text[ti]):
			ti++
			pi++
		case starToken >= 0:
			starText++
			ti = starText
			pi = starToken + 1
		default:
			return false
		}
	}
	for pi < len(tokens) && tokens[pi].star {
		pi++
	}
	return pi == len(tokens)
}
