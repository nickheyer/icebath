package tools

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"unicode"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/fnmatch"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

// Realpath prefixes that block writes, only the sensitive macOS private var subtrees
var sensitivePathPrefixes = []string{"/etc/", "/boot/", "/usr/lib/systemd/", "/private/etc/", "/private/var/db/", "/private/var/root/"}

var sensitiveExactPaths = setOf("/var/run/docker.sock", "/run/docker.sock")

// Basenames of files that steer future agent behaviour, matched case insensitively in any directory
var protectedInstructionBasenames = setOf("agents.md", "claude.md", "soul.md", ".cursorrules")

const approvalUnavailableWhy = "requires approval but the approval subsystem is unavailable."

const noHumanWhy = "requires approval but no interactive user or gateway is present to approve it."

// Status text read_file returns for an unchanged reread, never valid file content
const readDedupStatusMessage = "File unchanged since last read. The content from " +
	"the earlier read_file result in this conversation is " +
	"still current — refer to that instead of re-reading."

// os.path.normpath for the host flavour
func hostNormpath(p string) string {
	if isWindowsHost {
		return filepath.Clean(p)
	}
	return posixNormpath(p)
}

// os.path.basename for the host flavour, empty after a trailing separator
func hostBasename(p string) string {
	cut := strings.LastIndexByte(p, '/')
	if isWindowsHost {
		cut = strings.LastIndexAny(p, `/\`)
		if cut < 0 && len(p) >= 2 && p[1] == ':' {
			cut = 1
		}
	}
	return p[cut+1:]
}

// Resolved config.yaml of the active profile, recomputed per call so multiplexed profiles never share it
func icebathConfigResolved(env *Env) string {
	return resolveNonStrict(home.ConfigPath(envHome(env)))
}

// Realpath of the active profile home
func realIcebathHome(env *Env) string {
	return resolveNonStrict(envHome(env))
}

// Realpaths the protected instruction gate stays out of, the active home and the root of a named profile
func icebathExemptHomes(env *Env) []string {
	h := realIcebathHome(env)
	if h == "" {
		return nil
	}
	profileHome := home.NamedProfileHome(h)
	if profileHome == "" {
		return []string{h}
	}
	root := resolveNonStrict(filepath.Dir(filepath.Dir(profileHome)))
	if root != "" && root != h {
		return []string{h, root}
	}
	return []string{h}
}

// Task resolved path, the raw input when only filesystem resolution failed
func resolvedOrRaw(env *Env, filePath, taskID string) (string, error) {
	resolved, err := resolvePathForTaskID(env, taskID, filePath)
	if err != nil {
		if isPathResolutionFailure(err) {
			return filePath, nil
		}
		return "", err
	}
	return resolved, nil
}

// Refusal when the path targets a system location, the profile config, or a locked skill, empty otherwise
func checkSensitivePath(env *Env, filePath, taskID string) (string, error) {
	if ntErr := ntNamespaceError(filePath, "Write"); ntErr != "" {
		return ntErr, nil
	}
	resolved, err := resolvedOrRaw(env, filePath, taskID)
	if err != nil {
		return "", err
	}
	candidates := []string{resolved, hostNormpath(expandTildeFor(env, filePath))}
	if sshPathEscapesHome(candidates[0]) && terminalEnvTypeForTask(env, taskID) == "ssh" {
		return "Refusing to write to " + filePath + ": it climbs above the SSH home and the remote " +
			"home could not be detected, so its target cannot be checked. Pass an absolute path.", nil
	}
	for _, c := range candidates {
		if sensitiveExactPaths[c] {
			return sensitivePathRefusal(filePath), nil
		}
		for _, prefix := range sensitivePathPrefixes {
			if strings.HasPrefix(c, prefix) {
				return sensitivePathRefusal(filePath), nil
			}
		}
	}
	if cfgPath := icebathConfigResolved(env); cfgPath != "" && inList(candidates, cfgPath) {
		return "Refusing to write to icebath config file: " + filePath + "\n" +
			"Agent cannot modify security-sensitive configuration. " +
			"Edit ~/.icebath/config.yaml directly or use 'icebath config' instead.", nil
	}
	if reason := SkillLockPathRefusal(env, candidates); reason != "" {
		return "Refusing to write to " + filePath + ": " + reason, nil
	}
	return "", nil
}

func sensitivePathRefusal(filePath string) string {
	return "Refusing to write to sensitive system path: " + filePath + "\n" +
		"Use the terminal tool with sudo if you need to modify system files."
}

// Gate switch and extra basename patterns from the live config, read failures keep the gate on
func protectedInstructionConfig(env *Env) (bool, []string) {
	tree, err := config.Load(envHome(env), configLookup(env))
	if err != nil {
		return true, nil
	}
	enabled := true
	if v, ok := tree.Lookup("security.protected_instruction_files"); ok {
		if b, isBool := v.(bool); isBool {
			enabled = b
		}
	}
	var extra []string
	if v, ok := tree.Lookup("security.protected_instruction_extra_patterns"); ok {
		if list, isList := v.([]any); isList {
			for _, p := range list {
				if truthy(p) {
					extra = append(extra, pyStr(p))
				}
			}
		}
	}
	return enabled, extra
}

// Short label when the path names a protected instruction file by input or realpath, empty otherwise
func protectedInstructionReason(env *Env, filePath, taskID string, enabled bool, extraPatterns []string) string {
	if !enabled {
		return ""
	}
	normalized := hostNormpath(expandTildeFor(env, filePath))
	resolved := resolveNonStrict(normalized)
	if r, err := resolvePathForTaskID(env, taskID, filePath); err == nil {
		resolved = resolveNonStrict(r)
	}
	sep := string(os.PathSeparator)
	for _, realHome := range icebathExemptHomes(env) {
		if resolved == realHome || strings.HasPrefix(resolved, realHome+sep) {
			return ""
		}
	}
	for _, candidate := range []string{normalized, resolved} {
		base := hostBasename(candidate)
		baseLower := strings.ToLower(base)
		if protectedInstructionBasenames[baseLower] {
			return base
		}
		for _, pattern := range extraPatterns {
			if fnmatch.FnmatchCase(baseLower, strings.ToLower(pattern)) {
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

// Ordered unique join of labels the way dict.fromkeys dedups them
func joinUniqueLabels(items []string) string {
	seen := map[string]bool{}
	var out []string
	for _, it := range items {
		if !seen[it] {
			seen[it] = true
			out = append(out, it)
		}
	}
	return strings.Join(out, ", ")
}

// Asks the human every time for a protected instruction write, empty when approved and fail closed otherwise
func requestProtectedInstructionApproval(ctx context.Context, env *Env, reasons []string, taskID string) string {
	targets := joinUniqueLabels(reasons)
	description := "Write to protected agent-instruction file(s): " + targets + ". " +
		"These files steer future agent behavior; approval is always " +
		"required (not bypassed by auto-approve)."
	display := "<write to " + targets + ">"
	blocked := func(why string) string {
		return "BLOCKED: write to protected agent-instruction file(s) (" + targets + ") " + why +
			" The user has NOT consented to this write. Do NOT retry it or " +
			"attempt the same edit via another path (terminal, execute_code, " +
			"etc.)."
	}
	timedOut := blocked("approval prompt timed out without a user response. Silence is not consent.")
	denied := blocked("was denied by the user.")
	if env == nil {
		return blocked(approvalUnavailableWhy)
	}
	sessionKey := getCurrentSessionKey(env, "default")
	var choice string
	var timed bool
	if notify := gatewayNotifyCb(sessionKey); notify != nil {
		approvalData := map[string]any{
			"command":         display,
			"pattern_key":     "protected_instruction_file",
			"pattern_keys":    []string{"protected_instruction_file"},
			"description":     description,
			"allow_permanent": false,
			"allow_session":   false,
		}
		decision := awaitGatewayDecision(ctx, env, sessionKey, notify, approvalData, "gateway")
		if decision.NotifyFailed {
			return blocked("requires approval but the approval request could not be delivered.")
		}
		if decision.Cancelled != "" {
			return blocked("approval was withdrawn before the user answered (" + decision.Cancelled + ").")
		}
		choice, timed = decision.Choice, !decision.Resolved
	} else {
		if env.Ask == nil {
			return blocked(noHumanWhy)
		}
		if NoUserCanAnswer(env) {
			return blocked(noHumanWhy)
		}
		hook := map[string]any{"command": display, "description": description, "pattern_key": "protected_instruction_file",
			"pattern_keys": []string{"protected_instruction_file"}, "session_key": sessionKey, "surface": "cli"}
		fireApprovalHook(env, "pre_approval_request", hook)
		answer := promptDangerousApproval(ctx, env, promptOptions{command: display, description: description, allowPermanent: false, allowSession: false})
		post := copyMap(hook)
		post["choice"] = answer.value
		fireApprovalHook(env, "post_approval_response", post)
		if answer.value == "cancelled" {
			cause := answer.cause
			if cause == "" {
				cause = "no answer"
			}
			return blocked("approval prompt could not be delivered or was not answered (" + cause + ").")
		}
		choice, timed = answer.value, answer.value == "timeout"
	}
	if !timed && (choice == "once" || choice == "session" || choice == "always") {
		return ""
	}
	if timed {
		return timedOut
	}
	return denied
}

// One protected instruction file gates the whole write or multi file patch with a single prompt
func checkProtectedInstructionWrite(ctx context.Context, env *Env, paths []string, taskID string) string {
	enabled, extra := protectedInstructionConfig(env)
	if !enabled {
		return ""
	}
	var reasons []string
	for _, p := range paths {
		if r := protectedInstructionReason(env, p, taskID, enabled, extra); r != "" {
			reasons = append(reasons, r)
		}
	}
	if len(reasons) == 0 {
		return ""
	}
	return requestProtectedInstructionApproval(ctx, env, reasons, taskID)
}

// Routine approval gate for writes to approval required paths like the SSH client config
func checkApprovalRequiredWrite(ctx context.Context, env *Env, paths []string, taskID string) (string, error) {
	var targets []string
	for _, p := range paths {
		resolved, err := resolvedOrRaw(env, p, taskID)
		if err != nil {
			return "", err
		}
		if isWriteApprovalRequired(env, resolved) {
			targets = append(targets, p)
		}
	}
	if len(targets) == 0 {
		return "", nil
	}
	displayTargets := joinUniqueLabels(targets)
	description := "Write to SSH client config file(s): " + displayTargets + ". " +
		"The SSH config can carry ProxyCommand / Match exec directives that " +
		"run commands, so writes require your approval."
	blocked := func(why string) string {
		return "BLOCKED: write to SSH config file(s) (" + displayTargets + ") " + why +
			" Do NOT retry it via another path (terminal, execute_code) " +
			"without the user's explicit consent."
	}
	if env == nil {
		return blocked(approvalUnavailableWhy), nil
	}
	result := runApprovalGate(ctx, env, approvalGateOptions{
		patternKey:      "ssh_config_write",
		description:     description,
		displayTarget:   "<write to " + displayTargets + ">",
		cronDenyMessage: blocked("requires approval but this cron session denies it."),
		singleQueryDenyMessage: blocked("requires approval but single-query (-q) sessions run " +
			"without a user present to approve it. To allow flagged " +
			"actions in single-query mode, set approvals.single_query_mode: " +
			"approve in config.yaml."),
		autoapproveLogPrefix:  "ssh_config_write",
		failClosedWhenNoHuman: true,
		noHumanBlockMessage:   blocked(noHumanWhy),
	})
	if result.Approved {
		return "", nil
	}
	if result.Message != "" {
		return result.Message, nil
	}
	return blocked("was denied."), nil
}

// Container side icebath mirror prefix for a persistent Docker task, empty otherwise
func containerMirrorPrefixForTask(env *Env, taskID string) string {
	key, err := resolveContainerTaskID(env, taskID)
	if err != nil {
		return ""
	}
	termPool.Lock()
	live := termPool.active[key]
	if live == nil {
		live = termPool.active[taskID]
	}
	termPool.Unlock()
	if live != nil {
		if d, ok := live.(*dockerEnv); ok && d.persistent {
			return "/root/.icebath"
		}
		return ""
	}
	cfg, err := getEnvConfig(env)
	if err != nil {
		return ""
	}
	persistent, has := cfg["container_persistent"]
	if cfg["env_type"] == "docker" && (!has || truthy(persistent)) {
		return "/root/.icebath"
	}
	return ""
}

// Soft guard warning when the path lands on a host or Docker sandbox mirror of icebath state
func checkCrossProfilePath(env *Env, filePath, taskID string) (string, error) {
	resolved, err := resolvedOrRaw(env, filePath, taskID)
	if err != nil {
		return "", err
	}
	if w := sandboxMirrorWarningFor(resolved); w != "" {
		return w, nil
	}
	return containerMirrorWarningFor(resolved, containerMirrorPrefixForTask(env, taskID)), nil
}

// Whether a regular file exists where the write executes, exists absent or unavailable
func targetRegularFileState(env *Env, filePath, taskID string) string {
	resolved, rerr := resolvePathForTaskID(env, taskID, filePath)
	handle, err := getFileOps(env, taskID)
	if err != nil {
		return "unavailable"
	}
	if handle.usesHostPaths() {
		probe := resolved
		if rerr != nil {
			probe = expandTildeFor(env, filePath)
		}
		if isRegularFile(probe) {
			return "exists"
		}
		return "absent"
	}
	target := resolved
	if rerr != nil || target == "" {
		target = filePath
	}
	_, status := handle.ops.probeRegularFile(handle.ops.expandPath(target))
	switch status {
	case "ok", "bad_size":
		return "exists"
	case "missing", "not_regular":
		return "absent"
	}
	return "unavailable"
}

// Refusal for a text write that would corrupt a binary document, sidecar, or existing binary file
func checkBinaryDocumentWrite(env *Env, filePath, taskID string) string {
	ext := strings.ToLower(pySplitextExt(filePath))
	if hasOpaqueDocumentExtension(filePath) {
		return "Refusing to write plain text to binary document '" + filePath + "' (" + ext + "). " +
			"A text write cannot produce a valid document container and would " +
			"corrupt the file (read_file showed you EXTRACTED text, not the real " +
			"bytes). Use the microsoft-office skill or a library like " +
			"python-docx/openpyxl/python-pptx via the terminal to create or edit " +
			"this document."
	}
	if isSqliteSidecar(filePath) {
		return "Refusing to write plain text to binary SQLite sidecar '" + filePath + "' (" + ext + "). " +
			"A -wal/-shm/-journal file holds raw database pages that SQLite " +
			"reads on the next open; text there corrupts the database. Use the " +
			"sqlite3 CLI or a SQLite library via the terminal to modify the " +
			"database instead."
	}
	pdf := isPDFPath(filePath)
	if !pdf && !hasBinaryExtension(filePath) {
		return ""
	}
	switch targetRegularFileState(env, filePath, taskID) {
	case "exists":
		if pdf {
			return "Refusing to overwrite existing PDF '" + filePath + "' with plain text. " +
				"read_file showed you EXTRACTED text, not the real bytes — writing " +
				"text back would destroy the document. Use the pdf skill or a PDF " +
				"library via the terminal to modify it. (Creating a NEW .pdf file " +
				"is allowed.)"
		}
		return "Refusing to overwrite existing binary file '" + filePath + "' (" + ext + ") " +
			"with plain text — read_file showed you extracted or mojibake " +
			"text, not the real bytes, and writing text back would destroy " +
			"the file. Use a binary-aware tool via the terminal to modify it " +
			"(for SQLite databases, the sqlite3 CLI or a SQLite library). " +
			"(Creating a NEW file with this extension is allowed.)"
	case "unavailable":
		return "Refusing to write to '" + filePath + "': could not establish whether " +
			"the target file already exists where this write would execute " +
			"(the terminal environment may be starting, unreachable, or was " +
			"removed). The file was NOT modified — retry once the environment " +
			"is reachable."
	}
	return ""
}

// Reason write_file must not replace the existing file before any disk mutation, empty to proceed
func staleOverwriteBlocker(env *Env, filePath, resolved, taskID string) (string, error) {
	if fileStateGuardDisabled() {
		return "", nil
	}
	if resolved != "" {
		if stale := fileState.checkStale(taskID, resolved); stale != "" {
			return stale, nil
		}
	}
	drifted, err := readMtimeDrifted(env, filePath, taskID)
	if err != nil {
		return "", err
	}
	if drifted {
		return filePath + " was modified since you last read it (external edit or " +
			"concurrent agent). Re-read the file before writing.", nil
	}
	if resolved == "" || hasFullWriteBaseline(resolved, taskID) {
		return "", nil
	}
	if _, err := os.Stat(resolved); err != nil {
		return "", nil
	}
	if isOwnBlindPatch(resolved, taskID) {
		return "Your patch changed this file without a full view of its current content. Read the current file " +
			"in full before a whole-file overwrite, or continue with targeted patches.", nil
	}
	return resolved + " exists but this task has not seen its full current content " +
		"(never read, patched without a prior full read, or only a redacted/partial view). Read the " +
		"file — every page of it, if it needs offset/limit — or use patch for a " +
		"targeted edit; a stale conversation copy must not overwrite the current " +
		"disk content.", nil
}

// Model facing refusal for write_file, stale_write_blocked tells it apart from I/O errors
func staleWriteRefusal(filePath, reason, resolved string) *pyjson.Object {
	out := &pyjson.Object{Vals: map[string]any{}}
	out.Set("error", "Refusing to overwrite "+filePath+": "+reason+" "+
		"The file was NOT modified. Reload the current contents with read_file "+
		"(every page, for a file that needs offset/limit), merge the requested "+
		"change, then call write_file again. For small edits, prefer patch so "+
		"existing unrelated changes are preserved.")
	out.Set("stale_write_blocked", true)
	out.Set("path", filePath)
	if resolved != "" {
		out.Set("resolved_path", resolved)
	}
	return out
}

// True for the read_file dedup status message verbatim or lightly framed
func isInternalFileStatusText(content string) bool {
	stripped := pyStrip(content)
	return stripped != "" && strings.Contains(stripped, readDedupStatusMessage) &&
		utf8Len(stripped) <= 2*utf8Len(readDedupStatusMessage)
}

// True for content dominated by consecutive read_file LINE_NUM pipe CONTENT display rows
func looksLikeReadFileLineNumberedContent(content string) bool {
	var lines []string
	for _, line := range pySplitLines(content) {
		if pyStrip(line) != "" {
			lines = append(lines, line)
		}
	}
	if len(lines) < 2 {
		return false
	}
	var numbered []int
	for _, line := range lines {
		prefix, _, sep := strings.Cut(strings.TrimLeftFunc(line, pyIsSpace), "|")
		if sep && pyIsDigitString(prefix) {
			n, err := strconv.Atoi(pyDigitsASCII(prefix))
			if err == nil {
				numbered = append(numbered, n)
			}
		}
	}
	if len(numbered) < 2 || float64(len(numbered))/float64(len(lines)) < 0.6 {
		return false
	}
	pairs := 0
	for i := 1; i < len(numbered); i++ {
		if numbered[i] == numbered[i-1]+1 {
			pairs++
		}
	}
	return pairs >= len(numbered)-1
}

// Python str.isdecimal over a non empty string, the digits int accepts
func pyIsDigitString(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if !unicode.IsDigit(r) {
			return false
		}
	}
	return true
}

// ASCII spelling of a Unicode decimal digit string, each Nd run of ten counted from its zero
func pyDigitsASCII(s string) string {
	var b strings.Builder
	for _, r := range s {
		start := r
		for start > 0 && unicode.IsDigit(start-1) {
			start--
		}
		b.WriteRune('0' + (r-start)%10)
	}
	return b.String()
}

// True when content is file tool display text rather than intended file bytes
func isInternalFileToolContent(content string) bool {
	return isInternalFileStatusText(content) || looksLikeReadFileLineNumberedContent(content)
}
