package tools

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

// Read size guard in characters, a model agnostic proxy for tokens, configurable as file_read_max_chars
const defaultMaxReadChars = 100_000

// Above this size a wide read gets a hint toward targeted reads
const largeFileHintBytes = 512_000

// Device and fd paths whose reads hang the process, checked by path only
var blockedDevicePaths = setOf(
	"/dev/zero", "/dev/random", "/dev/urandom", "/dev/full",
	"/dev/stdin", "/dev/tty", "/dev/console",
	"/dev/stdout", "/dev/stderr",
	"/dev/fd/0", "/dev/fd/1", "/dev/fd/2",
)

// Proc files that leak secrets, argv, memory layout, or raw memory
var blockedProcSuffixes = []string{
	"/fd/0", "/fd/1", "/fd/2",
	"/environ", "/cmdline", "/maps", "/smaps", "/smaps_rollup", "/numa_maps",
	"/mem", "/auxv", "/pagemap",
}

// Whole file rewrite hint thresholds, a large overwrite keeping most old lines is a patch done the expensive way
const (
	rewriteHintMinChars     = 20_000
	rewriteHintMinUnchanged = 0.80
	rewriteHintMaxChars     = 400_000
)

// V4A headers, group one the prefix, two the op, three the path
var (
	v4aToolSingleHeaderRe = regexp.MustCompile(`(?m)^(\*\*\*` + pyWSClass + `*(Update|Add|Delete)` + pyWSClass + `+File:` + pyWSClass + `*)(.+)$`)
	v4aToolMoveHeaderRe   = regexp.MustCompile(`(?m)^(\*\*\*` + pyWSClass + `*Move` + pyWSClass + `+File:` + pyWSClass + `*)(.+?)` + pyWSClass + `*->` + pyWSClass + `*(.+)$`)
)

// Live config document of the active profile, mtime cached by the loader
func liveConfig(env *Env) (config.Tree, error) {
	return config.Load(envHome(env), configLookup(env))
}

// file_read_max_chars from the live config, the default on a missing or invalid value
func maxReadChars(env *Env) int {
	tree, err := liveConfig(env)
	if err != nil {
		return defaultMaxReadChars
	}
	switch v := tree.Get("file_read_max_chars").(type) {
	case bool:
		if v {
			return 1
		}
	case int:
		if v > 0 {
			return v
		}
	case int64:
		if v > 0 {
			return int(v)
		}
	case float64:
		if v > 0 {
			return int(v)
		}
	}
	return defaultMaxReadChars
}

// Trims numbered content to the last complete line within the budget, clamping mid line when even one does not fit
func truncateToCharBudget(content string, maxChars int) (string, int, bool) {
	if utf8Len(content) <= maxChars {
		n := 0
		if content != "" {
			n = strings.Count(content, "\n") + 1
		}
		return content, n, false
	}
	lines := strings.Split(content, "\n")
	var kept []string
	running := 0
	for _, line := range lines {
		addition := utf8Len(line)
		if len(kept) > 0 {
			addition++
		}
		if running+addition > maxChars {
			break
		}
		kept = append(kept, line)
		running += addition
	}
	if len(kept) == 0 {
		kept = append(kept, runePrefix(lines[0], maxChars))
	}
	return strings.Join(kept, "\n"), len(kept), true
}

// Trims content to the budget and annotates the result with the continuation hint
func applyCharBudget(result *pyjson.Object, content string, offset int, totalLines any, maxChars int) string {
	trimmed, linesKept, _ := truncateToCharBudget(content, maxChars)
	nextOffset := offset + linesKept
	result.Set("content", trimmed)
	result.Set("truncated", true)
	result.Set("truncated_by", "bytes")
	result.Set("next_offset", nextOffset)
	hint := fmt.Sprintf("Output truncated at the %s-char read budget after %d line(s) (showing lines %d-%d of %s). Use offset=%d to continue.",
		commaInt(maxChars), linesKept, offset, nextOffset-1, pyDisplay(totalLines), nextOffset)
	first, _, _ := strings.Cut(trimmed, "\n")
	if utf8Len(first) >= maxChars {
		result.Set("truncated_lines", true)
		hint += " Note: the first line alone exceeded the budget and was " +
			"clamped mid-line; its remainder is not retrievable via offset."
	}
	result.Set("hint", hint)
	return trimmed
}

// Python str of a decoded result value for message text
func pyDisplay(v any) string {
	if n, ok := v.(json.Number); ok {
		return n.String()
	}
	return pyStr(v)
}

// File backend surface the file tools drive, the backend contract plus probes and gutters
type fileToolOps interface {
	fileOperations
	expandPath(path string) string
	probeRegularFile(path string) (int64, string)
	addLineNumbers(content string, startLine int) string
}

// One cached file backend of a task with the terminal env it runs through
type fileOpsHandle struct {
	ops fileToolOps
	// Copy of the backend whose commands run under the call context and attribute LSP verdicts to its owner
	bind     func(ctx context.Context) fileToolOps
	terminal Environment
	cwd      string
}

// True when the backend targets the host filesystem, the only case that may stat or rewrite to host paths
func (h *fileOpsHandle) usesHostPaths() bool {
	return h.terminal == nil || EnvUsesHostFilesystem(h.terminal)
}

// Backend bound to the call context
func (h *fileOpsHandle) forCall(ctx context.Context) fileToolOps {
	return h.bind(ctx)
}

var fileOpsCache = struct {
	sync.Mutex
	m map[string]*fileOpsHandle
}{m: map[string]*fileOpsHandle{}}

// Per task file backend lookup seam, tests swap it for a scripted backend
var getFileOps = getFileOpsShared

// Backend handle over a shell file ops instance whose per call copies carry the context
func newFileOpsHandle(env *Env, terminal Environment, cwd string) *fileOpsHandle {
	ops := newShellFileOps(env, terminal, cwd)
	return &fileOpsHandle{ops: ops, terminal: terminal, cwd: ops.cwd, bind: func(ctx context.Context) fileToolOps {
		return ops.withContext(ctx)
	}}
}

// Longest a file tool waits for its task environment to come up
const fileOpsEnvStartTimeout = 5 * time.Minute

// Builds the configured terminal environment of a task so a file tool before any command gets the right backend
func createTerminalEnvForFileOps(env *Env, rawTaskID, taskID string, cfg map[string]any) (string, Environment, error) {
	envType := pyStrOrEmpty(cfg["env_type"])
	overrides, err := ResolveTaskOverrides(env, rawTaskID)
	if err != nil {
		return envType, nil, err
	}
	cwd := ""
	if truthy(overrides["cwd"]) {
		cwd = pyStr(overrides["cwd"])
	} else if recorded, ok := GetSessionCwd(env, rawTaskID); ok && recorded != "" {
		cwd = recorded
	} else {
		cwd = pyStrOrEmpty(cfg["cwd"])
	}
	cwd = coerceSSHRemoteCwd(env, cwd, envType)
	hostCwd, err := resolveTaskHostCwd(env, cfg, rawTaskID)
	if err != nil {
		return envType, nil, err
	}
	if isContainerBackend(envType) && isUnusableContainerCwd(cwd, hostCwd) {
		fallback := pyStrOrEmpty(cfg["cwd"])
		if isMountedHostCwd(cwd, hostCwd) {
			fallback = "/workspace"
		}
		if cwd != fallback {
			envLog(env).Info(fmt.Sprintf("Ignoring host/relative cwd override %s for %s backend (won't exist in sandbox). Using %s instead.",
				pyReprStr(cwd), envType, pyReprStr(fallback)))
		}
		cwd = fallback
	}
	envLog(env).Info(fmt.Sprintf("Creating new %s environment for task %s...", envType, runePrefix(taskID, 8)))
	timeout, _ := config.AsInt(cfg["timeout"])
	ctx, cancel := context.WithTimeout(context.Background(), fileOpsEnvStartTimeout)
	defer cancel()
	created, err := createConfiguredEnv(ctx, env, cfg, envType, selectImage(envType, overrides, cfg), cwd, timeout, taskID, hostCwd)
	return envType, created, err
}

// Gets or creates the task's file backend on its terminal environment, subagents sharing the parent container
func getFileOpsShared(env *Env, taskID string) (*fileOpsHandle, error) {
	raw := taskID
	if raw == "" {
		raw = "default"
	}
	key, err := resolveContainerTaskID(env, raw)
	if err != nil {
		return nil, err
	}
	fileOpsCache.Lock()
	cached := fileOpsCache.m[key]
	fileOpsCache.Unlock()
	if cached != nil {
		termPool.Lock()
		live := termPool.active[key]
		if live != nil && live == cached.terminal {
			termPool.lastActivity[key] = time.Now()
			termPool.Unlock()
			return cached, nil
		}
		termPool.Unlock()
		if live == nil && cached.cwd != "" {
			if _, recorded := GetSessionCwd(env, raw); !recorded {
				RecordSessionCwd(env, raw, cached.cwd)
			}
		}
		fileOpsCache.Lock()
		if fileOpsCache.m[key] == cached {
			delete(fileOpsCache.m, key)
		}
		fileOpsCache.Unlock()
	}
	lock := creationLockFor(key)
	lock.Lock()
	handle, err := buildFileOpsHandle(env, raw, key)
	lock.Unlock()
	if err != nil {
		return nil, err
	}
	fileOpsCache.Lock()
	fileOpsCache.m[key] = handle
	fileOpsCache.Unlock()
	return handle, nil
}

// Backend over the live terminal env, else over a freshly created configured env, creation lock held
func buildFileOpsHandle(env *Env, raw, key string) (*fileOpsHandle, error) {
	termPool.Lock()
	terminal := termPool.active[key]
	if terminal != nil {
		termPool.lastActivity[key] = time.Now()
	}
	termPool.Unlock()
	if terminal != nil {
		return newFileOpsHandle(env, terminal, ""), nil
	}
	cfg, err := getEnvConfig(env)
	if err != nil {
		return nil, err
	}
	createdType, created, err := createTerminalEnvForFileOps(env, raw, key, cfg)
	if err != nil {
		return nil, err
	}
	termPool.Lock()
	termPool.active[key] = created
	termPool.lastActivity[key] = time.Now()
	termPool.Unlock()
	startCleanupThreadFn(env)
	envLog(env).Info(fmt.Sprintf("%s environment ready for task %s", createdType, runePrefix(key, 8)))
	return newFileOpsHandle(env, created, ""), nil
}

// Drops the cached file backend of one task or of every task
func forgetFileOps(taskID string) {
	fileOpsCache.Lock()
	defer fileOpsCache.Unlock()
	if taskID != "" {
		delete(fileOpsCache.m, taskID)
		return
	}
	fileOpsCache.m = map[string]*fileOpsHandle{}
}

// Rewrites V4A headers to the resolved host paths so the shell layer patches the files the tool locked
func rewriteV4APatchPathsForHost(patch string, pathToResolved, pathToEntry map[string]string, handle *fileOpsHandle) string {
	if !handle.usesHostPaths() {
		return patch
	}
	res := func(raw string, entry bool) string {
		raw = pyStrip(raw)
		table := pathToResolved
		if entry {
			table = pathToEntry
		}
		if r := table[raw]; r != "" {
			return r
		}
		return raw
	}
	patch = replaceAllSubmatchFunc(v4aToolSingleHeaderRe, patch, func(m []string) string {
		return m[1] + res(m[3], m[2] == "Delete")
	})
	return replaceAllSubmatchFunc(v4aToolMoveHeaderRe, patch, func(m []string) string {
		return m[1] + res(m[2], true) + " -> " + res(m[3], true)
	})
}

// Regexp substitution with a callback receiving every group, the re.sub with a function form
func replaceAllSubmatchFunc(re *regexp.Regexp, s string, fn func([]string) string) string {
	var b strings.Builder
	last := 0
	for _, loc := range re.FindAllStringSubmatchIndex(s, -1) {
		groups := make([]string, len(loc)/2)
		for i := range groups {
			if loc[2*i] >= 0 {
				groups[i] = s[loc[2*i]:loc[2*i+1]]
			}
		}
		b.WriteString(s[last:loc[0]])
		b.WriteString(fn(groups))
		last = loc[1]
	}
	b.WriteString(s[last:])
	return b.String()
}

// True for a concrete device, fd, or proc path that can hang reads or leak process state
func isBlockedDevicePath(env *Env, path string) bool {
	normalized := hostNormpath(expandTildeFor(env, path))
	if blockedDevicePaths[normalized] {
		return true
	}
	if !strings.HasPrefix(normalized, "/proc/") {
		return false
	}
	for _, suffix := range blockedProcSuffixes {
		if strings.HasSuffix(normalized, suffix) {
			return true
		}
	}
	return false
}

// os.path.dirname for the host flavour
func hostDirname(p string) string {
	if isWindowsHost {
		return filepath.Dir(p)
	}
	return posixDirname(p)
}

// True when the literal path, any symlink hop, or the final realpath is a blocked device, raising the readlink encode error
func isBlockedDevice(env *Env, filePath, baseDir string) (bool, error) {
	expanded, err := expandTildeErr(env, filePath)
	if err != nil {
		return false, err
	}
	if baseDir != "" && !filepath.IsAbs(expanded) {
		expanded = pyOSPathJoin(baseDir, expanded)
	}
	normalized := hostNormpath(expanded)
	if isBlockedDevicePath(env, normalized) {
		return true, nil
	}
	seen := map[string]bool{}
	current := normalized
	for i := 0; i < 20; i++ {
		if err := fsEncodeError(current); err != nil {
			return false, err
		}
		target, err := os.Readlink(current)
		if err != nil {
			break
		}
		if !filepath.IsAbs(target) {
			target = pyOSPathJoin(hostDirname(current), target)
		}
		target = hostNormpath(target)
		if isBlockedDevicePath(env, target) {
			return true, nil
		}
		if seen[target] {
			break
		}
		seen[target] = true
		current = target
	}
	if realpathEncodeError(normalized) != nil {
		return false, nil
	}
	return isBlockedDevicePath(env, hostNormpath(resolveNonStrict(normalized))), nil
}

// Task cwd resolution of a search hit, the raw path when it cannot resolve
func resolvedMatchPath(env *Env, path, taskID string) string {
	resolved, err := resolvePathForTaskID(env, taskID, path)
	if err != nil {
		return path
	}
	return resolved
}

// Removes credential, cache, and env paths from a search result in place and returns the omitted count
func filterReadBlockedSearchResults(env *Env, result *SearchResult, taskID string) int {
	omitted := 0
	allowed := func(path string) bool {
		if readBlockError(env, resolvedMatchPath(env, path, taskID)) != "" {
			omitted++
			return false
		}
		return true
	}
	if len(result.Matches) > 0 {
		kept := result.Matches[:0]
		for _, m := range result.Matches {
			if allowed(m.Path) {
				kept = append(kept, m)
			}
		}
		result.Matches = kept
	}
	if len(result.Files) > 0 {
		kept := result.Files[:0]
		for _, f := range result.Files {
			if allowed(f) {
				kept = append(kept, f)
			}
		}
		result.Files = kept
	}
	if len(result.Counts) > 0 {
		kept := result.Counts[:0]
		for _, c := range result.Counts {
			if allowed(c.Path) {
				kept = append(kept, c)
			}
		}
		result.Counts = kept
	}
	return omitted
}

// True for expected write denials that should not reach the error log
func isExpectedWriteError(err error) bool {
	if errors.Is(err, fs.ErrPermission) {
		return true
	}
	var errno syscall.Errno
	if errors.As(err, &errno) {
		return errno == syscall.EACCES || errno == syscall.EPERM || errno == syscall.EROFS
	}
	return false
}

// Human name of a non regular file type whose read would hang, empty for regular files and directories
func specialFileKind(path string) string {
	st, err := os.Stat(path)
	if err != nil {
		return ""
	}
	mode := st.Mode()
	switch {
	case mode.IsRegular() || mode.IsDir():
		return ""
	case mode&fs.ModeNamedPipe != 0:
		return "a FIFO (named pipe)"
	case mode&fs.ModeSocket != 0:
		return "a socket"
	case mode&fs.ModeCharDevice != 0:
		return "a character device"
	case mode&fs.ModeDevice != 0:
		return "a block device"
	}
	return "a special (non-regular) file"
}

// Empty ordered result object
func newDict() *pyjson.Object { return &pyjson.Object{Vals: map[string]any{}} }

// Removes a key from an ordered result object
func dictDel(o *pyjson.Object, key string) {
	if _, ok := o.Vals[key]; !ok {
		return
	}
	delete(o.Vals, key)
	for i, k := range o.Keys {
		if k == key {
			o.Keys = append(o.Keys[:i], o.Keys[i+1:]...)
			break
		}
	}
}

// Sets a key only when absent, the dict setdefault form
func dictSetDefault(o *pyjson.Object, key string, v any) {
	if _, ok := o.Vals[key]; !ok {
		o.Set(key, v)
	}
}

// Truthy value of a result key
func dictTruthy(o *pyjson.Object, key string) bool {
	v, _ := o.Get(key)
	if n, ok := v.(json.Number); ok {
		f, _ := n.Float64()
		return f != 0
	}
	return truthy(v)
}

// String value of a result key, empty when absent or not a string
func dictString(o *pyjson.Object, key string) string {
	v, _ := o.Get(key)
	s, _ := v.(string)
	return s
}

// Integer value of a result key and whether it held a JSON integer
func dictInt(o *pyjson.Object, key string) (int, bool) {
	v, _ := o.Get(key)
	switch n := v.(type) {
	case json.Number:
		i, err := n.Int64()
		return int(i), err == nil
	case int:
		return n, true
	}
	return 0, false
}

// Ordered dict of a result value through its JSON encoding, nested objects ordered and surrogates kept
func toDict(v any) *pyjson.Object {
	data, err := pyjson.EncodeWTF8(v)
	if err != nil {
		out := newDict()
		out.Set("error", err.Error())
		return out
	}
	value, err := pyjson.DecodeWTF8Ordered(data)
	if obj, ok := value.(*pyjson.Object); ok && err == nil {
		return obj
	}
	return newDict()
}

// Compact JSON of an ordered result object
func dictJSON(o *pyjson.Object) string {
	return JSON(o)
}

// Error envelope with a bounded message and extra keys in the given order
func fileToolError(message string, extra ...any) string {
	out := newDict()
	out.Set("error", boundErrorText(message))
	for i := 0; i+1 < len(extra); i += 2 {
		out.Set(extra[i].(string), extra[i+1])
	}
	return dictJSON(out)
}

// Renders an extractable document as paginated text, handled false to fall through to the normal read
func readExtractedDocument(ctx context.Context, env *Env, path, resolved string, offset, limit int, taskID string) (string, bool, error) {
	if !IsExtractableDocument(resolved) {
		return "", false, nil
	}
	handle, err := getFileOps(env, taskID)
	if err != nil {
		return "", false, err
	}
	binary := handle.forCall(ctx).readFileBytes(resolved, maxDocumentBytes)
	var extracted string
	var extractErr error
	if binary.Error != "" || !binary.IsBinary {
		msg := binary.Error
		if msg == "" {
			msg = "Document bytes unavailable"
		}
		extractErr = &ExtractionError{Msg: msg}
	} else if data, decodeErr := base64.StdEncoding.Strict().DecodeString(binary.Base64Content); decodeErr != nil {
		extractErr = decodeErr
	} else if extracted, err = ExtractDocumentBytes(ctx, env, data, resolved); err != nil {
		var ee *ExtractionError
		if !errors.As(err, &ee) {
			return "", false, err
		}
		extractErr = err
	}
	if extractErr != nil {
		envLog(env).Debug("document extraction failed", "path", path, "error", extractErr)
		docExt := strings.ToLower(pathSuffix(resolved))
		binaryDoc := nativeDocumentExtensions[docExt] || (extractableExtensions[docExt] && docExt != ".ipynb")
		var ee *ExtractionError
		if binaryDoc && errors.As(extractErr, &ee) && !strings.HasPrefix(ee.Error(), "Unsupported document type") {
			return fileToolError(fmt.Sprintf("Cannot read '%s' (%s): document extraction failed — %s. Use terminal utilities to inspect or convert the file.",
				path, docExt, ee.Error())), true, nil
		}
		return "", false, nil
	}
	lines := pySplitLines(extracted)
	totalLines := len(lines)
	endLine := offset + limit - 1
	lo, hi := min(offset-1, totalLines), min(endLine, totalLines)
	if lo < 0 {
		lo = 0
	}
	if hi < lo {
		hi = lo
	}
	pageText := strings.Join(lines[lo:hi], "\n")
	maxLineLength := ToolOutputLimits(env).MaxLineLength
	truncatedLines := false
	for _, line := range strings.Split(pageText, "\n") {
		if utf8Len(line) > maxLineLength {
			truncatedLines = true
			break
		}
	}
	ops := handle.forCall(ctx)
	result := newDict()
	content := ""
	if pageText != "" {
		content = ops.addLineNumbers(pageText, offset)
	}
	result.Set("content", content)
	result.Set("total_lines", totalLines)
	result.Set("file_size", binary.FileSize)
	result.Set("truncated", totalLines > endLine)
	result.Set("extracted_document", true)
	if totalLines > endLine {
		result.Set("hint", fmt.Sprintf("Use offset=%d to continue reading (showing %d-%d of %d lines)", endLine+1, offset, min(endLine, totalLines), totalLines))
	}
	maxChars := maxReadChars(env)
	if utf8Len(content) > maxChars {
		applyCharBudget(result, content, offset, totalLines, maxChars)
	}
	redacted := false
	if rendered := dictString(result, "content"); rendered != "" {
		red := redactFileText(env, rendered, isSecretFileArg(env, resolved))
		result.Set("content", red)
		redacted = red != rendered
	}
	if offset == 1 && !dictTruthy(result, "truncated") && !redacted && !truncatedLines && !dictTruthy(result, "truncated_lines") {
		markFullWriteBaseline(resolved, taskID, "")
		if err := updateReadTimestamp(env, resolved, taskID); err != nil {
			return "", false, err
		}
		fileState.recordRead(taskID, resolved, false)
	}
	return dictJSON(result), true, nil
}

// Unchanged stub for a repeated identical read, escalating to a hard block after two stubs
func dedupStubOrBlock(td *taskTracker, key dedupKey, path string) string {
	readTrackerLock.Lock()
	hits, _ := td.dedupHits.get(key)
	hits++
	td.dedupHits.set(key, hits)
	capReadTrackerData(td)
	readTrackerLock.Unlock()
	if hits >= 2 {
		return fileToolError(fmt.Sprintf("BLOCKED: You have called read_file on this "+
			"exact region %d times and the file "+
			"has NOT changed. STOP calling read_file for "+
			"this path — the content from your earlier "+
			"read_file result in this conversation is "+
			"still current. Proceed with your task using "+
			"the information you already have.", hits+1),
			"path", path, "already_read", hits+1, GuardrailRefusalKey, true)
	}
	out := newDict()
	out.Set("status", "unchanged")
	out.Set("message", readDedupStatusMessage)
	out.Set("path", path)
	out.Set("dedup", true)
	out.Set("content_returned", false)
	return dictJSON(out)
}

// Consecutive call key of one tool call shape
func consecutiveKey(parts ...any) string {
	s := make([]string, len(parts))
	for i, p := range parts {
		s[i] = fmt.Sprint(p)
	}
	return strings.Join(s, "\x00")
}

// Records a full skill file read for the review fork guard, registered by the skills package
var MarkBackgroundReviewSkillRead func(env *Env, resolved string)

// Options of one successful read's bookkeeping
type successfulRead struct {
	path, resolved string
	offset, limit  int
	key            dedupKey
	partial        bool
	redacted       bool
	endLine        int
	totalLines     int
	before         fileMeta
	hasBefore      bool
	snapshot       *fileVersion
}

// Bookkeeping after a real read, returning the consecutive read count
func recordSuccessfulRead(env *Env, td *taskTracker, taskID string, r successfulRead) int {
	var version fileVersion
	hasVersion := false
	if r.hasBefore {
		if r.snapshot != nil {
			version, hasVersion = *r.snapshot, true
		} else {
			version, hasVersion = readFileVersion(r.resolved)
		}
	}
	stable := false
	if hasVersion && version.meta == r.before {
		current, hasCurrent := fileMetadata(r.resolved)
		stable = hasCurrent && current == r.before
	}
	complete := false
	redacted := r.redacted
	readTrackerLock.Lock()
	td.dedupHits.del(r.key)
	td.dedupGenerationReads.set(r.key, struct{}{})
	td.readHistory.set(dedupKey{r.path, r.offset, r.limit}, struct{}{})
	count := bumpConsecutive(td, consecutiveKey("read", r.path, r.offset, r.limit))
	if mtime, ok := mtimeOf(r.resolved); ok {
		td.readTimestamps.set(r.resolved, mtime)
	}
	if stable && count < 4 {
		td.dedup.set(r.key, r.before)
		baseline, has := td.fullWriteBaselines.get(r.resolved)
		complete = has && baseline == version
		if !complete {
			complete = !r.partial
			if r.partial {
				complete, redacted = noteReadCoverage(td, r.resolved, version, r.offset, r.endLine, r.totalLines, redacted)
			}
			complete = complete && !redacted
		}
		if complete {
			td.fullWriteBaselines.set(r.resolved, version)
			td.blindPatches.del(r.resolved)
		}
	}
	if !complete {
		td.fullWriteBaselines.del(r.resolved)
	}
	if !stable || count >= 4 {
		td.dedup.del(r.key)
		td.dedupGenerationReads.del(r.key)
	}
	capReadTrackerData(td)
	readTrackerLock.Unlock()
	fileState.recordRead(taskID, r.resolved, !complete)
	if complete && isBackgroundReview(env) {
		if MarkBackgroundReviewSkillRead == nil {
			panic("tools: MarkBackgroundReviewSkillRead is nil, pkg/skills must install the review read mark")
		}
		MarkBackgroundReviewSkillRead(env, r.resolved)
	}
	return count
}

// Reads a file with pagination and line numbers behind the namespace, device, denylist, document, and dedup guards
func readFileTool(ctx context.Context, env *Env, path string, offsetArg, limitArg any, taskID string) string {
	out, err := readFileToolErr(ctx, env, path, offsetArg, limitArg, taskID)
	if err != nil {
		return fileToolError(err.Error())
	}
	return out
}

func readFileToolErr(ctx context.Context, env *Env, path string, offsetArg, limitArg any, taskID string) (string, error) {
	offset, limit := normalizeReadPagination(env, offsetArg, limitArg)
	if ntErr := ntNamespaceError(path, "Read"); ntErr != "" {
		return fileToolError(ntErr), nil
	}
	deviceBase := ""
	if !filepath.IsAbs(expandUser(path)) {
		base, err := resolveBaseDirFor(env, taskID, nil)
		if err != nil {
			return "", err
		}
		deviceBase = base
	}
	blocked, err := isBlockedDevice(env, path, deviceBase)
	if err != nil {
		return "", err
	}
	if blocked {
		return fileToolError(fmt.Sprintf("Cannot read '%s': this is a device file that would block or produce infinite output.", path)), nil
	}
	resolved, err := resolvePathForTaskID(env, taskID, path)
	if err != nil {
		return "", err
	}
	handle, err := getFileOps(env, taskID)
	if err != nil {
		return "", err
	}
	if handle.usesHostPaths() {
		if kind := specialFileKind(resolved); kind != "" {
			out := newDict()
			out.Set("success", false)
			out.Set("note", fmt.Sprintf("'%s' is %s, not a regular file — reading it would block indefinitely, so no read was attempted. Use terminal utilities if you need to interact with it.", path, kind))
			return JSONASCII(out), nil
		}
	}
	if blockErr := readBlockError(env, resolved); blockErr != "" {
		return fileToolError(blockErr), nil
	}
	if extracted, handled, err := readExtractedDocument(ctx, env, path, resolved, offset, limit, taskID); err != nil {
		return "", err
	} else if handled {
		return extracted, nil
	}
	if hasBinaryExtension(resolved) {
		return fileToolError(fmt.Sprintf("Cannot read binary file '%s' (%s). Use vision_analyze for images, or terminal to inspect binary files.",
			path, strings.ToLower(pathSuffix(resolved)))), nil
	}
	if cached, ok := checkNotFoundCache("read", resolved, taskID); ok {
		return cached, nil
	}
	key := dedupKey{resolved, offset, limit}
	readTrackerLock.Lock()
	td := taskData(taskID)
	cachedVersion, hasCached := td.dedup.get(key)
	served := td.dedupGenerationReads.has(key)
	readTrackerLock.Unlock()
	host := handle.usesHostPaths()
	var before fileMeta
	hasBefore := false
	if host {
		before, hasBefore = fileMetadata(resolved)
	}
	if hasCached && !isBackgroundReview(env) && hasBefore && before == cachedVersion && served {
		return dedupStubOrBlock(td, key, path), nil
	}
	target := path
	if host {
		target = resolved
	}
	result := handle.forCall(ctx).readFile(target, offset, limit)
	dict := toDict(result)
	errText := dictString(dict, "error")
	if strings.HasPrefix(errText, "File not found:") {
		if fields := ExpiredArchiveFields(path); len(fields) > 0 {
			dict.Set("expired", fields["expired"])
			dict.Set("hint", fields["hint"])
		}
		recordNotFound("read", resolved, taskID, dictJSON(dict))
	}
	if errText != "" || dictTruthy(dict, "is_binary") {
		return dictJSON(dict), nil
	}
	fileSize, _ := dictInt(dict, "file_size")
	maxChars := maxReadChars(env)
	content := result.Content
	if utf8Len(content) > maxChars {
		totalLines, ok := dict.Get("total_lines")
		if !ok {
			totalLines = "unknown"
		}
		content = applyCharBudget(dict, content, offset, totalLines, maxChars)
	}
	redacted := false
	if content != "" {
		unredacted := content
		content = redactFileText(env, unredacted, isSecretFileArg(env, resolved))
		redacted = content != unredacted
		dict.Set("content", content)
	}
	if content != "" {
		if conflicts := countConflictBlocks(content); conflicts > 0 {
			dict.Set("conflict_blocks", conflicts)
			dict.Set("_hint", fmt.Sprintf("%d unresolved git merge-conflict block(s) (<<<<<<< / ======= / >>>>>>>) in this "+
				"range. Resolve them (keep one side or combine, delete the markers) before editing around them.", conflicts))
		}
	}
	if fileSize > largeFileHintBytes && limit > 200 && dictTruthy(dict, "truncated") {
		dictSetDefault(dict, "_hint", fmt.Sprintf("This file is large (%s bytes). "+
			"Consider reading only the section you need with offset and limit "+
			"to keep context usage efficient.", commaInt(fileSize)))
	}
	totalLines, hasTotal := dictInt(dict, "total_lines")
	endLine := offset + limit - 1
	if dictString(dict, "truncated_by") == "bytes" {
		next, ok := dictInt(dict, "next_offset")
		if !ok {
			next = offset
		}
		endLine = next - 1
	} else if hasTotal && totalLines > 0 {
		endLine = min(endLine, totalLines)
	}
	read := successfulRead{path: path, resolved: resolved, offset: offset, limit: limit, key: key,
		partial:  offset > 1 || dictTruthy(dict, "truncated"),
		redacted: redacted || dictTruthy(dict, "truncated_lines"),
		endLine:  endLine, totalLines: totalLines, before: before, hasBefore: hasBefore}
	if snap, ok := readResultSnapshot(result); ok {
		read.snapshot = &snap
	}
	count := recordSuccessfulRead(env, td, taskID, read)
	if count >= 4 {
		return fileToolError(fmt.Sprintf("BLOCKED: You have read this exact file region %d times in a row. "+
			"The content has NOT changed. You already have this information. "+
			"STOP re-reading and proceed with your task.", count),
			"path", path, "already_read", count, GuardrailRefusalKey, true), nil
	}
	if count >= 3 {
		dict.Set("_warning", fmt.Sprintf("You have read this exact file region %d times consecutively. "+
			"The content has not changed since your last read. Use the information you already have. "+
			"If you are stuck in a loop, stop reading and proceed with writing or responding.", count))
	}
	return dictJSON(dict), nil
}

// Task resolved path, the entry form keeping a final symlink, empty when resolution fails for any reason
func resolveOrNone(env *Env, filePath, taskID string, entry bool) string {
	var resolved string
	var err error
	if entry {
		resolved, err = resolveEntryForTaskID(env, taskID, filePath)
	} else {
		resolved, err = resolvePathForTaskID(env, taskID, filePath)
	}
	if err != nil {
		return ""
	}
	return resolved
}

// Shared write and patch guards in order, hard denies before anything that prompts, one prompt for every path
func writePrecheckError(ctx context.Context, env *Env, paths, contentPaths []string, taskID string, crossProfile bool) (string, error) {
	for _, p := range paths {
		msg, err := checkSensitivePath(env, p, taskID)
		if err != nil {
			return "", err
		}
		if msg == "" && !crossProfile {
			if msg, err = checkCrossProfilePath(env, p, taskID); err != nil {
				return "", err
			}
		}
		if msg != "" {
			return msg, nil
		}
	}
	for _, p := range contentPaths {
		if msg := checkBinaryDocumentWrite(env, p, taskID); msg != "" {
			return msg, nil
		}
	}
	if msg := checkProtectedInstructionWrite(ctx, env, paths, taskID); msg != "" {
		return msg, nil
	}
	return checkApprovalRequiredWrite(ctx, env, paths, taskID)
}

// One pre edit warning per path, sibling writer over staleness over workspace divergence
func editWarnings(env *Env, paths []string, pathToResolved map[string]string, taskID string) ([]string, error) {
	var warnings []string
	for _, p := range paths {
		r := pathToResolved[p]
		w := ""
		if r != "" {
			w = fileState.checkStale(taskID, r)
		}
		if w == "" {
			var err error
			if w, err = checkFileStaleness(env, p, taskID); err != nil {
				return nil, err
			}
		}
		if w == "" && r != "" {
			w = pathResolutionWarningFor(env, taskID, p, r)
		}
		if w != "" {
			warnings = append(warnings, w)
		}
	}
	return warnings, nil
}

// Post success bookkeeping, the verification stale marker then per path read stamps and write records
func noteEdited(env *Env, taskID string, paths []string, pathToResolved map[string]string, sessionID string) error {
	marked := make([]string, len(paths))
	for i, p := range paths {
		marked[i] = p
		if r := pathToResolved[p]; r != "" {
			marked[i] = r
		}
	}
	markVerificationStale(env, taskID, sessionID, marked)
	for _, p := range paths {
		if err := updateReadTimestamp(env, p, taskID); err != nil {
			return err
		}
		if r := pathToResolved[p]; r != "" {
			fileState.noteWrite(taskID, r)
		}
	}
	return nil
}

// Hint when new content mostly re-sends what is already on disk, read through the task's own backend
func wholeFileRewriteHint(ctx context.Context, env *Env, taskID, resolved, newContent string) string {
	newLen := utf8Len(newContent)
	if resolved == "" || newLen < rewriteHintMinChars || newLen > rewriteHintMaxChars {
		return ""
	}
	handle, err := getFileOps(env, taskID)
	if err != nil {
		return ""
	}
	result := handle.forCall(ctx).ReadFileRaw(resolved)
	if result == nil || result.Error != "" {
		return ""
	}
	old := result.Content
	if n := utf8Len(old); n < rewriteHintMinChars || n > rewriteHintMaxChars {
		return ""
	}
	oldLines, newLines := pySplitLines(old), pySplitLines(newContent)
	if len(oldLines) == 0 {
		return ""
	}
	counts := map[string]int{}
	for _, l := range oldLines {
		counts[l]++
	}
	unchanged := 0
	for _, l := range newLines {
		if counts[l] > 0 {
			counts[l]--
			unchanged++
		}
	}
	longest := max(len(oldLines), len(newLines))
	ratio := float64(unchanged) / float64(longest)
	if ratio < rewriteHintMinUnchanged {
		return ""
	}
	changed := longest - unchanged
	return fmt.Sprintf("%s of %s lines were already on disk (%.0f%% unchanged); ~%s "+
		"line(s) actually changed. Re-sending a %s-char file costs output tokens for every "+
		"unchanged line; for edits like this use patch (old_string/new_string), which sends only the changed region.",
		commaInt(unchanged), commaInt(len(newLines)), ratio*100, commaInt(changed), commaInt(newLen))
}

// Writes content to a file behind the write guards and the stale overwrite blocker
func writeFileTool(ctx context.Context, env *Env, path, content, taskID string, crossProfile bool, sessionID string) string {
	out, err := writeFileToolErr(ctx, env, path, content, taskID, crossProfile, sessionID)
	if err != nil {
		if isExpectedWriteError(err) {
			envLog(env).Debug(fmt.Sprintf("write_file expected denial: %T: %v", err, err))
		} else {
			envLog(env).Error(fmt.Sprintf("write_file error: %T: %v", err, err))
		}
		return fileToolError(err.Error())
	}
	return out
}

// WriteFileTool exposes the write_file tool entry to packages that drive it directly
func WriteFileTool(ctx context.Context, env *Env, path, content, taskID string, crossProfile bool, sessionID string) string {
	return writeFileTool(ctx, env, path, content, taskID, crossProfile, sessionID)
}

func writeFileToolErr(ctx context.Context, env *Env, path, content, taskID string, crossProfile bool, sessionID string) (string, error) {
	msg, err := checkSensitivePath(env, path, taskID)
	if err != nil {
		return "", err
	}
	if msg == "" {
		msg = checkBinaryDocumentWrite(env, path, taskID)
	}
	if msg == "" {
		msg = checkProtectedInstructionWrite(ctx, env, []string{path}, taskID)
	}
	if msg == "" {
		if msg, err = checkApprovalRequiredWrite(ctx, env, []string{path}, taskID); err != nil {
			return "", err
		}
	}
	if msg == "" && !crossProfile {
		if msg, err = checkCrossProfilePath(env, path, taskID); err != nil {
			return "", err
		}
	}
	if msg == "" && isInternalFileToolContent(content) {
		msg = "Refusing to write internal read_file display text as file content. " +
			"Strip read_file line-number prefixes or reconstruct the intended " +
			"file contents before writing."
	}
	if msg != "" {
		return fileToolError(msg), nil
	}
	resolved := resolveOrNone(env, path, taskID, false)
	pathToResolved := map[string]string{path: resolved}
	if resolved != "" {
		unlock := fileState.lockPath(resolved)
		defer unlock()
	}
	blocker, err := staleOverwriteBlocker(env, path, resolved, taskID)
	if err != nil {
		return "", err
	}
	if blocker != "" {
		return dictJSON(staleWriteRefusal(path, blocker, resolved)), nil
	}
	warnings, err := editWarnings(env, []string{path}, pathToResolved, taskID)
	if err != nil {
		return "", err
	}
	rewriteHint := wholeFileRewriteHint(ctx, env, taskID, resolved, content)
	handle, err := getFileOps(env, taskID)
	if err != nil {
		return "", err
	}
	target := resolved
	if target == "" {
		target = path
	}
	result := handle.forCall(ctx).WriteFile(target, content, nil)
	dict := toDict(result)
	if len(warnings) > 0 {
		dict.Set("_warning", warnings[0])
	}
	if rewriteHint != "" && !dictTruthy(dict, "error") {
		dict.Set("hint", rewriteHint)
	}
	if resolved != "" {
		dict.Set("resolved_path", resolved)
	}
	if dictTruthy(dict, "error") {
		if err := updateReadTimestamp(env, path, taskID); err != nil {
			return "", err
		}
		return dictJSON(dict), nil
	}
	if resolved != "" {
		dict.Set("files_modified", []string{resolved})
		expected := ""
		if result.ContentSha256 != nil {
			expected = *result.ContentSha256
		}
		markFullWriteBaseline(resolved, taskID, expected)
	}
	if err := noteEdited(env, taskID, []string{path}, pathToResolved, sessionID); err != nil {
		return "", err
	}
	return dictJSON(dict), nil
}

// Every path named in V4A headers with traversal refused, plus the text writing and entry acting subsets
func collectV4AHeaderPaths(patch string) ([]string, []string, []string, string) {
	type header struct {
		raw        string
		writesText bool
	}
	var headers []header
	for _, m := range v4aToolSingleHeaderRe.FindAllStringSubmatch(patch, -1) {
		headers = append(headers, header{m[3], m[2] == "Update" || m[2] == "Add"})
	}
	for _, m := range v4aToolMoveHeaderRe.FindAllStringSubmatch(patch, -1) {
		headers = append(headers, header{m[2], false}, header{m[3], false})
	}
	var paths, contentPaths, entryPaths []string
	for _, h := range headers {
		v4aPath := pyStrip(h.raw)
		if hasTraversalComponent(v4aPath) {
			return nil, nil, nil, fileToolError(fmt.Sprintf("V4A patch header contains '..' traversal: %s. "+
				"Use the agent's cwd-relative path (no '..') or an absolute "+
				"path in '*** Update File:' / '*** Add File:' / "+
				"'*** Delete File:' / '*** Move File:' headers.", pyReprStr(v4aPath)))
		}
		paths = append(paths, v4aPath)
		if h.writesText {
			contentPaths = append(contentPaths, v4aPath)
		} else {
			entryPaths = append(entryPaths, v4aPath)
		}
	}
	return paths, contentPaths, entryPaths, ""
}

// One patch call in replace or V4A form, nil strings meaning absent arguments
type patchArgs struct {
	mode         string
	path         string
	oldString    *string
	newString    *string
	replaceAll   bool
	patch        string
	crossProfile bool
	sessionID    string
}

// Patches files in replace mode or V4A form behind the shared write guards
func patchTool(ctx context.Context, env *Env, a patchArgs, taskID string, probe *editProbe) string {
	out, err := patchToolErr(ctx, env, a, taskID, probe)
	if err != nil {
		return fileToolError(err.Error())
	}
	return out
}

func patchToolErr(ctx context.Context, env *Env, a patchArgs, taskID string, probe *editProbe) (string, error) {
	var paths []string
	if a.path != "" {
		paths = []string{a.path}
	}
	contentPaths := append([]string{}, paths...)
	var entryPaths []string
	if a.mode == "patch" && a.patch != "" {
		all, content, entry, errJSON := collectV4AHeaderPaths(a.patch)
		if errJSON != "" {
			return errJSON, nil
		}
		paths = append(paths, all...)
		contentPaths = append(contentPaths, content...)
		entryPaths = entry
	}
	precheck, err := writePrecheckError(ctx, env, paths, contentPaths, taskID, a.crossProfile)
	if err != nil {
		return "", err
	}
	if precheck != "" {
		return fileToolError(precheck), nil
	}
	pathToResolved := map[string]string{}
	for _, p := range paths {
		pathToResolved[p] = resolveOrNone(env, p, taskID, false)
	}
	pathToEntry := map[string]string{}
	for _, p := range entryPaths {
		pathToEntry[p] = resolveOrNone(env, p, taskID, true)
	}
	lockSet := map[string]bool{}
	for _, r := range pathToResolved {
		if r != "" {
			lockSet[r] = true
		}
	}
	for _, r := range pathToEntry {
		if r != "" {
			lockSet[r] = true
		}
	}
	lockOrder := make([]string, 0, len(lockSet))
	for r := range lockSet {
		lockOrder = append(lockOrder, r)
	}
	sort.Strings(lockOrder)
	unlocks := make([]func(), 0, len(lockOrder))
	for _, r := range lockOrder {
		unlocks = append(unlocks, fileState.lockPath(r))
	}
	dict, early, err := func() (*pyjson.Object, bool, error) {
		defer func() {
			for i := len(unlocks) - 1; i >= 0; i-- {
				unlocks[i]()
			}
		}()
		staleWarnings, err := editWarnings(env, paths, pathToResolved, taskID)
		if err != nil {
			return nil, false, err
		}
		handle, err := getFileOps(env, taskID)
		if err != nil {
			return nil, false, err
		}
		resolvedValues := make([]string, 0, len(paths))
		for _, p := range paths {
			resolvedValues = append(resolvedValues, pathToResolved[p])
		}
		known := knownFullContentSha256(resolvedValues, taskID)
		ops := handle.forCall(ctx)
		var result *PatchResult
		switch a.mode {
		case "replace":
			if a.path == "" {
				return patchErrorDict("path required"), true, nil
			}
			if a.oldString == nil || a.newString == nil {
				return patchErrorDict("old_string and new_string required"), true, nil
			}
			target := pathToResolved[a.path]
			if target == "" {
				target = a.path
			}
			result = ops.patchReplace(target, *a.oldString, *a.newString, a.replaceAll, probe)
		case "patch":
			if a.patch == "" {
				return patchErrorDict("patch content required"), true, nil
			}
			result = ops.patchV4A(rewriteV4APatchPathsForHost(a.patch, pathToResolved, pathToEntry, handle), probe)
		default:
			return patchErrorDict("Unknown mode: " + a.mode), true, nil
		}
		dict := toDict(result)
		if len(staleWarnings) > 0 {
			dict.Set("_warning", strings.Join(staleWarnings, " | "))
		}
		if !dictTruthy(dict, "error") {
			modified := make([]string, len(paths))
			for i, p := range paths {
				switch {
				case pathToEntry[p] != "":
					modified[i] = pathToEntry[p]
				case pathToResolved[p] != "":
					modified[i] = pathToResolved[p]
				default:
					modified[i] = p
				}
			}
			dict.Set("files_modified", modified)
			if len(modified) == 1 {
				dict.Set("resolved_path", modified[0])
			}
			if err := noteEdited(env, taskID, paths, pathToResolved, a.sessionID); err != nil {
				return nil, false, err
			}
			carryFullWriteBaselines(taskID, known, result.Writes, pathToResolved)
			var reset []string
			for _, p := range paths {
				if r := pathToResolved[p]; r != "" {
					reset = append(reset, r)
				}
			}
			resetPatchFailures(taskID, reset)
		}
		return dict, false, nil
	}()
	if err != nil {
		return "", err
	}
	if early {
		return dictJSON(dict), nil
	}
	if errText := pyDisplay(dict.Vals["error"]); dictTruthy(dict, "error") && strings.Contains(errText, "Could not find") {
		failureCount := 0
		if a.mode == "replace" && a.path != "" {
			target := pathToResolved[a.path]
			if target == "" {
				target = a.path
			}
			failureCount = recordPatchFailure(taskID, target)
		}
		if failureCount >= 3 {
			dict.Set("_hint", fmt.Sprintf("This is failure #%d patching %s. "+
				"Stop retrying with variations of the same old_string. "+
				"Either: (1) re-read the file fresh to verify current "+
				"content, (2) use a longer / more unique old_string with "+
				"surrounding context lines, or (3) re-read the file in "+
				"full, then use write_file to replace it if the targeted "+
				"region is hard to anchor.", failureCount, pyReprStr(a.path)))
		} else if !strings.Contains(errText, "Did you mean one of these sections?") {
			dict.Set("_hint", "old_string not found. Use read_file to verify the current "+
				"content, or search_files to locate the text.")
		}
	}
	return dictJSON(dict), nil
}

// Bounded error object returned straight from inside the locked patch section
func patchErrorDict(message string) *pyjson.Object {
	out := newDict()
	out.Set("error", boundErrorText(message))
	return out
}

// One search_files call after argument coercion
type searchArgs struct {
	pattern, target, path string
	fileGlob              string
	limit, offset         any
	outputMode            string
	context               int
	order                 string
}

// Searches file contents or names behind the repeated search, namespace, and denylist guards
func searchTool(ctx context.Context, env *Env, a searchArgs, taskID string) string {
	out, err := searchToolErr(ctx, env, a, taskID)
	if err != nil {
		return fileToolError(err.Error())
	}
	return out
}

func searchToolErr(ctx context.Context, env *Env, a searchArgs, taskID string) (string, error) {
	offset, limit := normalizeSearchPagination(a.offset, a.limit)
	readTrackerLock.Lock()
	td := taskData(taskID)
	count := bumpConsecutive(td, consecutiveKey("search", a.pattern, a.target, a.path, a.fileGlob, limit, offset, a.order))
	readTrackerLock.Unlock()
	if count >= 4 {
		return fileToolError(fmt.Sprintf("BLOCKED: You have run this exact search %d times in a row. "+
			"The results have NOT changed. You already have this information. "+
			"STOP re-searching and proceed with your task.", count),
			"pattern", a.pattern, "already_searched", count, GuardrailRefusalKey, true), nil
	}
	if ntErr := ntNamespaceError(a.path, "Search"); ntErr != "" {
		return fileToolError(ntErr), nil
	}
	resolvedSearchPath, err := resolvePathForTaskID(env, taskID, a.path)
	if err != nil {
		if !isPathResolutionFailure(err) && readBlockError(env, a.path) == "" {
			return "", err
		}
		resolvedSearchPath = a.path
	}
	if blockErr := readBlockError(env, resolvedSearchPath); blockErr != "" {
		return fileToolError(blockErr), nil
	}
	if cached, ok := checkNotFoundCache("search", resolvedSearchPath, taskID); ok {
		return cached, nil
	}
	handle, err := getFileOps(env, taskID)
	if err != nil {
		return "", err
	}
	result := handle.forCall(ctx).search(a.pattern, a.path, a.target, a.fileGlob, limit, offset, a.outputMode, a.context, a.order)
	omitted := filterReadBlockedSearchResults(env, result, taskID)
	for i := range result.Matches {
		if result.Matches[i].Content != "" {
			result.Matches[i].Content = redactFileText(env, result.Matches[i].Content, isSecretFileArg(env, resolvedMatchPath(env, result.Matches[i].Path, taskID)))
		}
	}
	dict := result.toDict(true)
	if omitted > 0 {
		dict.Set("_omitted", fmt.Sprintf("%d result(s) omitted because they target credential, "+
			"token, cache, or secret-bearing environment files.", omitted))
	}
	if strings.HasPrefix(dictString(dict, "error"), "Path not found:") {
		recordNotFound("search", resolvedSearchPath, taskID, dictJSON(dict))
	}
	if count >= 3 {
		dict.Set("_warning", fmt.Sprintf("You have run this exact search %d times consecutively. "+
			"The results have not changed. Use the information you already have.", count))
	}
	if dictTruthy(dict, "truncated") {
		dict.Set("_hint", fmt.Sprintf("Results truncated. Use offset=%d to see more, "+
			"or narrow with a more specific pattern or file_glob.", offset+limit))
	}
	return dictJSON(dict), nil
}

// File tools only require terminal backend availability
func checkFileRequirements(env *Env) bool { return CheckTerminalRequirements(env) }

const readFileDescription = "Read a text file with line numbers and pagination. Use this instead of cat/head/tail in terminal. Output format: 'LINE_NUM|CONTENT'. Suggests similar filenames if not found. Use offset and limit for large files. Reads exceeding ~100K characters are truncated on a line boundary and return a next_offset; continue with offset to read the rest. Documents auto-extract to readable text: .ipynb, Office (.docx/.xlsx/.pptx and legacy .doc/.ppt/.xls), PDF (text layer), OpenDocument, RTF, EPUB, SQLite (.db/.sqlite: schema, row counts, first rows). Cannot read images/binary — use vision_analyze for images."

var readFileSchema = map[string]any{
	"name":        "read_file",
	"description": readFileDescription,
	"parameters": map[string]any{
		"type": "object",
		"properties": map[string]any{
			"path":   map[string]any{"type": "string", "description": "Path to the file to read (absolute, relative, or ~/path)"},
			"offset": map[string]any{"type": "integer", "description": "Line number to start reading from (1-indexed, default: 1)", "default": 1, "minimum": 1},
			"limit":  map[string]any{"type": "integer", "description": "Maximum number of lines to read (default: 2000, max: 2000). Reads are additionally capped at a ~100K-character budget with a next_offset continuation.", "default": DefaultReadLimit, "maximum": 2000},
		},
		"required": []any{"path"},
	},
}

var writeFileSchema = map[string]any{
	"name":        "write_file",
	"description": "Write content to a file, completely replacing existing content. Use this instead of echo/cat heredoc in terminal. Creates parent directories automatically. OVERWRITES the entire file — use 'patch' for targeted edits. For an EXISTING file, call read_file first: write_file refuses (file untouched) when this task has no current full read/write of the file or the file changed on disk since; on refusal, read_file, merge, then retry. Auto-runs syntax checks on .py/.json/.yaml/.toml and other linted languages; only NEW errors introduced by this write are surfaced (pre-existing errors are filtered out). The result's verified:true means the on-disk content hash was confirmed — do NOT re-read the file to check the write landed.",
	"parameters": map[string]any{
		"type": "object",
		"properties": map[string]any{
			"path":    map[string]any{"type": "string", "description": "Path to the file to write (will be created if it doesn't exist, overwritten if it does)"},
			"content": map[string]any{"type": "string", "description": "Complete content to write to the file"},
		},
		"required": []any{"path", "content"},
	},
}

// Replace only properties shared by the base schema and the V4A layer
func patchBaseProperties() map[string]any {
	return map[string]any{
		"path": map[string]any{
			"type":        "string",
			"description": "File path to edit.",
		},
		"old_string": map[string]any{
			"type":        "string",
			"description": "Exact text to find and replace. Must be unique in the file unless replace_all=true. Include surrounding context lines to ensure uniqueness.",
		},
		"new_string": map[string]any{
			"type":        "string",
			"description": "Changed replacement text; it must differ from old_string. Pass empty string '' to delete the matched text.",
		},
		"replace_all": map[string]any{
			"type":        "boolean",
			"description": "Replace all occurrences instead of requiring a unique match (default: false)",
			"default":     false,
		},
	}
}

var patchSchema = map[string]any{
	"name": "patch",
	"description": "Targeted find-and-replace edits in files. Use this instead of sed/awk in terminal. " +
		"Uses fuzzy matching (9 strategies) so minor whitespace/indentation differences won't break it. " +
		"Returns a unified diff. Auto-runs syntax checks after editing. " +
		"Finds a unique string and replaces it.",
	"parameters": map[string]any{
		"type":       "object",
		"properties": patchBaseProperties(),
		"required":   []any{"path", "old_string", "new_string"},
	},
}

const patchV4ADescription = "Targeted find-and-replace edits in files. Use this instead of sed/awk in terminal. " +
	"Uses fuzzy matching (9 strategies) so minor whitespace/indentation differences won't break it. " +
	"Returns a unified diff. Auto-runs syntax checks after editing.\n\n" +
	"REPLACE MODE (mode='replace', default): find a unique string and replace it. " +
	"REQUIRED PARAMETERS: mode, path, old_string, new_string.\n" +
	"PATCH MODE (mode='patch'): apply V4A multi-file patches for bulk changes. " +
	"REQUIRED PARAMETERS: mode, patch."

// V4A mode and patch parameters layered on for OpenAI family mains
func patchV4AParams() (map[string]any, map[string]any) {
	return map[string]any{
		"type":        "string",
		"enum":        []any{"replace", "patch"},
		"description": "Edit mode. 'replace' (default): requires path + old_string + new_string. 'patch': requires patch content only.",
		"default":     "replace",
	}, map[string]any{
		"type":        "string",
		"description": "REQUIRED when mode='patch'. V4A format patch content. Format:\n*** Begin Patch\n*** Update File: path/to/file\n@@ context hint @@\n context line\n-removed line\n+added line\n*** End Patch",
	}
}

var openAIFamilyProviders = setOf("openai", "openai-chat", "openai-codex", "azure-openai", "codex")

var openAIFamilySlugPrefixes = []string{"gpt-", "gpt.", "chatgpt", "codex", "o1", "o3", "o4", "o5"}

// True when the active main provider or model is the OpenAI codex family trained on the V4A dialect
func isOpenAIFamilyMain(env *Env) bool {
	provider := strings.ToLower(strings.TrimSpace(readMainField(env, "provider", true)))
	model := strings.ToLower(strings.TrimSpace(readMainField(env, "model", false)))
	if openAIFamilyProviders[provider] {
		return true
	}
	slug := model
	if _, rest, ok := strings.Cut(model, "/"); ok {
		slug = rest
	}
	for _, p := range openAIFamilySlugPrefixes {
		if strings.HasPrefix(slug, p) {
			return true
		}
	}
	return strings.Contains(model, "openai/")
}

// Search note naming the TCC protected folders, present only on a macOS host
func macOSSearchNote() string {
	if runtime.GOOS != "darwin" {
		return ""
	}
	return " On macOS, broad searches above the user home automatically skip TCC-protected folders (Desktop, " +
		"Documents, Downloads, Library, Movies, Music, Pictures); target one directly when access is intentional."
}

var searchFilesSchema = map[string]any{
	"name":        "search_files",
	"description": "Search file contents or find files by name. Use this instead of grep/rg/find/ls in terminal. Ripgrep-backed, faster than shell equivalents." + macOSSearchNote() + "\n\nContent search (target='content'): Regex search inside files. Output modes: full matches with line numbers, file paths only, or match counts.\n\nFile search (target='files'): Find files by glob pattern (e.g., '*.py', '*config*'). Also use this instead of ls. Discovery order is the fast bounded default; exact global newest-first order is an explicit opt-in and may scan the full tree.",
	"parameters": map[string]any{
		"type": "object",
		"properties": map[string]any{
			"pattern":     map[string]any{"type": "string", "description": "Regex pattern for content search, or glob pattern (e.g., '*.py') for file search"},
			"target":      map[string]any{"type": "string", "enum": []any{"content", "files"}, "description": "'content' searches inside file contents, 'files' searches for files by name", "default": "content"},
			"path":        map[string]any{"type": "string", "description": "Directory or file to search in (default: current working directory)", "default": "."},
			"file_glob":   map[string]any{"type": "string", "description": "Filter files by pattern in grep mode (e.g., '*.py' to only search Python files)"},
			"limit":       map[string]any{"type": "integer", "description": "Maximum number of results to return (default: 50)", "default": 50},
			"offset":      map[string]any{"type": "integer", "description": "Skip first N results for pagination (default: 0)", "default": 0},
			"order":       map[string]any{"type": "string", "enum": []any{"discovery", "modified"}, "description": "File-search order: 'discovery' is fast bounded traversal order; 'modified' is exact global newest-first and may scan the full tree; ignored for content", "default": "discovery"},
			"output_mode": map[string]any{"type": "string", "enum": []any{"content", "files_only", "count"}, "description": "Output format for grep mode: 'content' shows matching lines with line numbers, 'files_only' lists file paths, 'count' shows match counts per file", "default": "content"},
			"context":     map[string]any{"type": "integer", "description": "Number of context lines before and after each match (grep mode only)", "default": 0},
		},
		"required": []any{"pattern"},
	},
}

// Task id of a call, default when the host bound none
func callTaskID(call *Call) string {
	if call.TaskID != "" {
		return call.TaskID
	}
	return "default"
}

func handleReadFile(ctx context.Context, call *Call) string {
	path := ""
	if v, ok := call.Args["path"]; ok && v != nil {
		path = pyStr(v)
	}
	offset, ok := call.Args["offset"]
	if !ok {
		offset = 1
	}
	limit, ok := call.Args["limit"]
	if !ok {
		limit = DefaultReadLimit
	}
	return readFileTool(ctx, call.Env, path, offset, limit, callTaskID(call))
}

func handleWriteFile(ctx context.Context, call *Call) string {
	tid := callTaskID(call)
	path, isString := call.Args["path"].(string)
	if !isString || path == "" {
		return fileToolError("write_file: missing required field 'path'. Re-emit the tool call with " +
			"both 'path' and 'content' set.")
	}
	raw, present := call.Args["content"]
	if !present {
		return fileToolError("write_file: missing required field 'content'. The tool call included a " +
			"path but no content argument — this is almost always a dropped-arg bug " +
			"under context pressure. Re-emit the tool call with the full content " +
			"payload, or use execute_code with icebath_tools.write_file() for very " +
			"large files.")
	}
	content, isString := raw.(string)
	if !isString {
		return fileToolError("write_file: 'content' must be a string, got " + pyJSONTypeNameOf(raw) + ".")
	}
	crossProfile := truthy(call.Args["cross_profile"])
	return recordFileEdit(call.Env, "write_file", "whole_file", func(probe *editProbe) string {
		return owned(ctx, call, func(ctx context.Context) string {
			return writeFileTool(ctx, call.Env, path, content, tid, crossProfile, call.SessionID)
		})
	})
}

// Context key carrying the session late LSP verdicts are attributed to
type lspOwnerKey struct{}

// Attributes LSP verdicts of writes under the returned context to the owner session
func WithLSPOwner(ctx context.Context, owner string) context.Context {
	return context.WithValue(ctx, lspOwnerKey{}, owner)
}

// Session an edit under the context attributes its late LSP verdicts to, empty when unbound
func LSPOwner(ctx context.Context) string {
	owner, _ := ctx.Value(lspOwnerKey{}).(string)
	return owner
}

// Runs an edit tool with its late LSP verdicts attributed to the calling session, else its task
func owned(ctx context.Context, call *Call, fn func(ctx context.Context) string) string {
	owner := call.SessionID
	if owner == "" {
		owner = callTaskID(call)
	}
	return fn(WithLSPOwner(ctx, owner))
}

// Optional string argument, nil when absent or null
func optionalStringArg(args map[string]any, key string) *string {
	v, ok := args[key]
	if !ok || v == nil {
		return nil
	}
	s := pyStr(v)
	return &s
}

func handlePatch(ctx context.Context, call *Call) string {
	tid := callTaskID(call)
	mode := "replace"
	if v, ok := call.Args["mode"]; ok {
		mode = pyStr(v)
	}
	a := patchArgs{mode: mode, oldString: optionalStringArg(call.Args, "old_string"), newString: optionalStringArg(call.Args, "new_string"),
		replaceAll: truthy(call.Args["replace_all"]), crossProfile: truthy(call.Args["cross_profile"]), sessionID: call.SessionID}
	if p, ok := call.Args["path"].(string); ok {
		a.path = p
	}
	if p, ok := call.Args["patch"].(string); ok {
		a.patch = p
	}
	return recordFileEdit(call.Env, "patch", mode, func(probe *editProbe) string {
		return owned(ctx, call, func(ctx context.Context) string {
			return patchTool(ctx, call.Env, a, tid, probe)
		})
	})
}

var searchTargetAliases = map[string]string{"grep": "content", "find": "files"}

// String argument with a default when absent, other values in their Python str form
func stringArgDefault(args map[string]any, key, def string) string {
	v, ok := args[key]
	if !ok {
		return def
	}
	return pyStr(v)
}

func handleSearchFiles(ctx context.Context, call *Call) string {
	target := stringArgDefault(call.Args, "target", "content")
	if alias, ok := searchTargetAliases[target]; ok {
		target = alias
	}
	path := "."
	if v, ok := call.Args["path"]; ok && v != nil {
		if s, isString := v.(string); !isString || pyStrip(s) != "" {
			path = pyStr(v)
		}
	}
	fileGlob := ""
	if v, ok := call.Args["file_glob"]; ok && v != nil {
		fileGlob = pyStr(v)
	}
	limit, ok := call.Args["limit"]
	if !ok {
		limit = 50
	}
	offset, ok := call.Args["offset"]
	if !ok {
		offset = 0
	}
	a := searchArgs{pattern: stringArgDefault(call.Args, "pattern", ""), target: target, path: path, fileGlob: fileGlob,
		limit: limit, offset: offset, outputMode: stringArgDefault(call.Args, "output_mode", "content"),
		context: argI(call.Args, "context", 0), order: stringArgDefault(call.Args, "order", "discovery")}
	return searchTool(ctx, call.Env, a, callTaskID(call))
}

// One word capability upgrade of the read_file description when hosted OCR has a trusted route
func readFileSchemaOverrides(env *Env) map[string]any {
	if HostedOCRAvailable(env) {
		return map[string]any{"description": strings.Replace(readFileDescription, "PDF (text layer)", "PDF (scanned or text)", 1)}
	}
	return nil
}

// Layers the V4A patch mode onto the replace only schema for OpenAI family mains
func patchSchemaOverrides(env *Env) map[string]any {
	if !isOpenAIFamilyMain(env) {
		return nil
	}
	mode, patch := patchV4AParams()
	props := map[string]any{"mode": mode}
	for k, v := range patchBaseProperties() {
		props[k] = v
	}
	props["patch"] = patch
	return map[string]any{
		"description": patchV4ADescription,
		"parameters":  map[string]any{"type": "object", "properties": props, "required": []any{"mode"}},
	}
}

func registerFileTools() {
	Register(Def{Name: "read_file", Toolset: "file", Schema: readFileSchema, Handler: handleReadFile, Check: checkFileRequirements,
		Emoji: "\U0001F4D6", MaxResultChars: 100_000, DynamicSchema: readFileSchemaOverrides})
	Register(Def{Name: "write_file", Toolset: "file", Schema: writeFileSchema, Handler: handleWriteFile, Check: checkFileRequirements,
		Emoji: "✍️", MaxResultChars: 100_000})
	Register(Def{Name: "patch", Toolset: "file", Schema: patchSchema, Handler: handlePatch, Check: checkFileRequirements,
		Emoji: "\U0001F527", MaxResultChars: 100_000, DynamicSchema: patchSchemaOverrides})
	Register(Def{Name: "search_files", Toolset: "file", Schema: searchFilesSchema, Handler: handleSearchFiles, Check: checkFileRequirements,
		Emoji: "\U0001F50E", MaxResultChars: 100_000})
}
