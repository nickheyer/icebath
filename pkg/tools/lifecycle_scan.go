package tools

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/nickheyer/icebath/pkg/state"
)

// MaxReferencedScriptBytes caps one referenced script read by the lifecycle walk
const MaxReferencedScriptBytes = 1024 * 1024

const maxReferencedScriptDepth = 8

const binarySniffBytes = 4096

// Walk budgets, variables so operators and tests can lower them at runtime
var (
	maxLifecycleScanBytes       = MaxReferencedScriptBytes
	maxLifecycleScanLines       = 16384
	maxLifecycleScanLineBytes   = 64 * 1024
	maxLifecycleScanPaths       = 1024
	maxLifecycleScanRemoteReads = 64
)

var (
	lifecycleShellExecutables = setOf("sh", "bash", "dash", "ksh", "zsh")
	shellOptionsWithValues    = setOf("-O", "+O", "-o", "+o")
	shellCommandFlags         = setOf("-c", "--command")
	cloudPlaceholderMarkers   = setOf("Mobile Documents", "CloudStorage")
	stringCommandOptions      = map[string][]string{"env": {"-S", "--split-string"}, "su": {"-c", "--command"}, "runuser": {"-c", "--command"}}
	lifecycleBinaryMagics     = [][]byte{
		[]byte("\x7fELF"), {0xfe, 0xed, 0xfa, 0xce}, {0xfe, 0xed, 0xfa, 0xcf}, {0xce, 0xfa, 0xed, 0xfe},
		{0xcf, 0xfa, 0xed, 0xfe}, {0xca, 0xfe, 0xba, 0xbe}, []byte("MZ"), []byte("!<arch>"), {0x1f, 0x8b}, []byte("PK\x03\x04"),
	}
)

// ReadRemoteScriptFunc reads a script through a remote backend, false when nothing came back
type ReadRemoteScriptFunc func(path string) (string, bool)

// GatewayLifecycleBlocked is the refusal raised for a cron job holding a gateway lifecycle command
type GatewayLifecycleBlocked struct{ Message string }

func (e *GatewayLifecycleBlocked) Error() string { return e.Message }

// Shared work budget for one complete referenced script walk with the named fail closed refusal
type lifecycleScanBudget struct {
	bytesRemaining       int
	linesRemaining       int
	pathsRemaining       int
	remoteReadsRemaining int
	refusal              string
}

func newLifecycleScanBudget() *lifecycleScanBudget {
	return &lifecycleScanBudget{
		bytesRemaining:       maxLifecycleScanBytes,
		linesRemaining:       maxLifecycleScanLines,
		pathsRemaining:       maxLifecycleScanPaths,
		remoteReadsRemaining: maxLifecycleScanRemoteReads,
	}
}

// Charges text before tokenization, false when it does not fit
func (b *lifecycleScanBudget) chargeText(text string) bool {
	encoded := len(text)
	if encoded > b.bytesRemaining {
		return false
	}
	lines := strings.Count(text, "\n") + 1
	if lines > b.linesRemaining {
		return false
	}
	longest := 0
	for _, line := range strings.Split(text, "\n") {
		if n := len([]rune(line)); n > longest {
			longest = n
		}
	}
	if longest > maxLifecycleScanLineBytes {
		return false
	}
	b.bytesRemaining -= encoded
	b.linesRemaining -= lines
	return true
}

// Charges one unique referenced path before any read
func (b *lifecycleScanBudget) chargePath() bool {
	if b.pathsRemaining <= 0 {
		return false
	}
	b.pathsRemaining--
	return true
}

// Charges one remote backend read
func (b *lifecycleScanBudget) chargeRemoteRead() bool {
	if b.remoteReadsRemaining <= 0 {
		return false
	}
	b.remoteReadsRemaining--
	return true
}

// Per read byte cap never above the per file cap and never negative, negative input means unset
func cappedReadLimit(maxBytes int) int {
	if maxBytes < 0 {
		return MaxReferencedScriptBytes
	}
	if maxBytes > MaxReferencedScriptBytes {
		return MaxReferencedScriptBytes
	}
	return maxBytes
}

// LifecycleScanRootWithinBudget reports whether text may enter an optional tokenizer pass on a fresh budget
func LifecycleScanRootWithinBudget(text string) bool {
	return newLifecycleScanBudget().chargeText(text)
}

func (b *lifecycleScanBudget) exhausted(what string, depth int) bool {
	envLog(nil).Warn("lifecycle guard scan budget exhausted, failing closed", "what", what, "depth", depth)
	b.refusal = fmt.Sprintf("the scan budget was exhausted (%s at depth %d)", what, depth)
	return true
}

// Names why an executed script failed closed without being scanned
func unreadableReason(path string) string {
	if state.HasLiveConnection(path) {
		return fmt.Sprintf("`%s` is a SQLite database open in this gateway process", path)
	}
	st, err := os.Stat(path)
	if err != nil {
		return fmt.Sprintf("`%s` could not be read", path)
	}
	if !st.Mode().IsRegular() {
		return fmt.Sprintf("`%s` is not a regular file", path)
	}
	return fmt.Sprintf("`%s` is larger than the scan cap (%d bytes) or the remaining walk budget", path, MaxReferencedScriptBytes)
}

func (b *lifecycleScanBudget) refuseUnreadable(path, reason string) bool {
	envLog(nil).Warn("lifecycle guard cannot scan referenced script, failing closed", "path", path, "reason", reason)
	b.refusal = reason
	return true
}

// Lenient resolve that keeps the input on any failure
func resolveLenient(path string) string {
	if strings.IndexByte(path, 0) >= 0 {
		return path
	}
	return resolveNonStrict(path)
}

// Reports whether a path sits inside a macOS FileProvider backed subtree
func isCloudPlaceholderPath(path string) bool {
	parts := strings.FieldsFunc(path, func(r rune) bool { return r == '/' || r == '\\' })
	for i := 1; i < len(parts); i++ {
		if parts[i-1] == "Library" && cloudPlaceholderMarkers[parts[i]] {
			return true
		}
	}
	return false
}

// Lexical or resolved cloud placeholder check
func onCloudPath(path string) bool {
	return isCloudPlaceholderPath(path) || isCloudPlaceholderPath(resolveLenient(path))
}

// Sanitizes a tokenized path candidate once at the ingestion boundary
func expandCandidatePath(candidate string) (string, bool) {
	if candidate == "" || strings.IndexByte(candidate, 0) >= 0 {
		return "", false
	}
	return expandUser(candidate), true
}

// Candidate anchored on the cwd or the process cwd when it is a real path
func resolvedOrNothing(candidate, cwd string) (string, bool) {
	path, ok := expandCandidatePath(candidate)
	if !ok {
		return "", false
	}
	if filepath.IsAbs(path) {
		return path, true
	}
	base := cwd
	if base == "" {
		wd, err := os.Getwd()
		if err != nil {
			return "", false
		}
		base = wd
	}
	return filepath.Join(base, path), true
}
// Exported form for pkg/cron, same answer as the package resolver
func ResolveLifecycleScriptPath(h, scriptPath string) (string, bool) {
	return resolveScriptPath(h, scriptPath)
}


// Resolves a cron script value the way the scheduler does, relative names live under home scripts
func resolveScriptPath(h, scriptPath string) (string, bool) {
	raw, ok := expandCandidatePath(scriptPath)
	if !ok {
		return "", false
	}
	if filepath.IsAbs(raw) {
		return raw, true
	}
	if strings.TrimSpace(h) == "" {
		return "", false
	}
	return filepath.Join(h, "scripts", raw), true
}

// Directory a script path resolves to, empty when it has none
func resolveScriptDirectory(h, scriptPath string) string {
	path, ok := resolveScriptPath(h, scriptPath)
	if !ok || !filepath.IsAbs(path) {
		return ""
	}
	return filepath.Dir(path)
}

// Values given to an option in both separated and equals form
func iterOptionValues(segment []string, start int, option string) []string {
	prefix := option + "="
	var out []string
	for position := start + 1; position < len(segment); position++ {
		token := segment[position]
		if token == option && position+1 < len(segment) {
			out = append(out, segment[position+1])
		} else if strings.HasPrefix(token, prefix) {
			out = append(out, token[len(prefix):])
		}
	}
	return out
}

// Scripts the token at an index executes
func referencesAt(segment []string, index int, cwd string) []string {
	if index >= len(segment) {
		return nil
	}
	executable := segment[index]
	name := lifecycleExecutableName(executable)
	var out []string
	if name == "." || name == "source" {
		if len(segment) > index+1 {
			if p, ok := resolvedOrNothing(segment[index+1], cwd); ok {
				out = append(out, p)
			}
		}
		return out
	}
	if lifecycleShellExecutables[name] {
		arguments := segment[index+1:]
		argIndex := 0
		for argIndex < len(arguments) {
			argument := arguments[argIndex]
			if argument == "--" {
				argIndex++
				break
			}
			if shellCommandFlags[argument] {
				break
			}
			if shellOptionsWithValues[argument] {
				argIndex += 2
				continue
			}
			if strings.HasPrefix(argument, "-") {
				argIndex++
				continue
			}
			break
		}
		if argIndex < len(arguments) && !shellCommandFlags[arguments[argIndex]] {
			if p, ok := resolvedOrNothing(arguments[argIndex], cwd); ok {
				out = append(out, p)
			}
		}
		return out
	}
	if strings.Trim(executable, "/") != "" && (strings.Contains(executable, "/") ||
		strings.HasSuffix(executable, ".sh") || strings.HasSuffix(executable, ".bash") || strings.HasSuffix(executable, ".zsh")) {
		if p, ok := resolvedOrNothing(executable, cwd); ok {
			out = append(out, p)
		}
	}
	return out
}

// Scripts executed directly or through a POSIX shell, read at the original and the peeled token
func iterReferencedShellScripts(command, cwd string) []string {
	var out []string
	for _, segment := range iterLifecycleCommandSegments(command) {
		index, ok := commandTokenIndex(segment)
		if !ok {
			continue
		}
		out = append(out, referencesAt(segment, index, cwd)...)
		if peeled := peelTransparentPrefixes(segment, index); peeled != index {
			out = append(out, referencesAt(segment, peeled, cwd)...)
		}
	}
	return out
}

// Code passed through a shell dash c, su dash c or env dash S for recursive scanning
func iterShellCommandPayloads(command string) []string {
	var out []string
	for _, segment := range iterLifecycleCommandSegments(command) {
		index, ok := commandTokenIndex(segment)
		if !ok {
			continue
		}
		for _, option := range stringCommandOptions[lifecycleExecutableName(segment[index])] {
			out = append(out, iterOptionValues(segment, index, option)...)
		}
		index, ok = executedCommandIndex(segment)
		if !ok || !lifecycleShellExecutables[lifecycleExecutableName(segment[index])] {
			continue
		}
		arguments := segment[index+1:]
		for argIndex := 0; argIndex+1 < len(arguments); argIndex++ {
			if shellCommandFlags[arguments[argIndex]] {
				out = append(out, arguments[argIndex+1])
				break
			}
		}
	}
	return out
}

// Reports whether data starts with a compiled binary signature, a shebang always wins
func hasBinaryMagic(data []byte) bool {
	if bytes.HasPrefix(data, []byte("#!")) {
		return false
	}
	for _, magic := range lifecycleBinaryMagics {
		if bytes.HasPrefix(data, magic) {
			return true
		}
	}
	return false
}

// Reads a referenced script under the connection lifecycle lock, text, presence and the unsafe flag
func readReferencedScript(path string, maxBytes int) (string, bool, bool) {
	var text string
	var ok, unsafe bool
	err := state.OfflineFileAccess(path, "read referenced script", func() {
		text, ok, unsafe = readReferencedScriptUnlocked(path, maxBytes)
	})
	if err != nil {
		var live *state.LiveConnectionError
		if errors.As(err, &live) {
			return "", false, true
		}
		return "", false, false
	}
	return text, ok, unsafe
}

// Bounded regular file only read, directories are nothing to scan and devices fail closed
func readReferencedScriptUnlocked(path string, maxBytes int) (string, bool, bool) {
	byteLimit := cappedReadLimit(maxBytes)
	if onCloudPath(path) {
		return "", false, true
	}
	if strings.IndexByte(path, 0) >= 0 {
		return "", false, false
	}
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return "", false, false
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return "", false, false
	}
	if !st.Mode().IsRegular() {
		return "", false, !st.IsDir()
	}
	data := make([]byte, 0, binarySniffBytes)
	buf := make([]byte, binarySniffBytes)
	n, err := f.Read(buf)
	if err != nil && n == 0 && !errors.Is(err, io.EOF) {
		return "", false, false
	}
	data = append(data, buf[:n]...)
	if hasBinaryMagic(data) {
		return "", false, false
	}
	if st.Size() > int64(byteLimit) {
		return "", false, true
	}
	for len(data) <= byteLimit {
		chunk := make([]byte, byteLimit+1-len(data))
		n, err := f.Read(chunk)
		if n > 0 {
			data = append(data, chunk[:n]...)
		}
		if err != nil || n == 0 {
			break
		}
	}
	if hasBinaryMagic(data) {
		return "", false, false
	}
	if len(data) > byteLimit {
		return "", false, true
	}
	if bytes.IndexByte(data, 0) >= 0 {
		data = bytes.ReplaceAll(data, []byte{0}, nil)
	}
	return termDecodeUTF8(data), true, false
}

// Applies the local read contract to text from an untrusted remote read callback
func sanitizeRemoteScriptText(text string, present bool, maxBytes int) (string, bool, bool) {
	if !present || text == "" || strings.IndexByte(text, 0) >= 0 {
		return "", false, false
	}
	if len(text) > cappedReadLimit(maxBytes) {
		return "", false, true
	}
	return text, true, false
}

// Reads a cron script with the bounded scanner, text and a named refusal when it failed closed
func readScriptForScanning(h, scriptPath string) (string, string) {
	resolved, ok := resolveScriptPath(h, scriptPath)
	if !ok {
		return "", ""
	}
	text, _, unsafe := readReferencedScript(resolved, -1)
	if unsafe {
		return "", unreadableReason(resolved)
	}
	return text, ""
}

// Recursive walk over the command, its shell payloads and the scripts it references
func containsUnsafeGatewayAction(command, cwd string, depth int, visited map[string]bool, budget *lifecycleScanBudget, readRemote ReadRemoteScriptFunc, executed bool) bool {
	if !budget.chargeText(command) {
		if executed {
			return budget.exhausted("text", depth)
		}
		return false
	}
	if DirectLifecycleScan(command) {
		return true
	}
	if depth >= maxReferencedScriptDepth {
		return executed
	}
	recurse := func(text, cwd string, executed bool) bool {
		return containsUnsafeGatewayAction(text, cwd, depth+1, visited, budget, readRemote, executed)
	}
	walkCommand := StripInertHeredocBodies(command)
	for _, payload := range iterShellCommandPayloads(walkCommand) {
		if recurse(payload, cwd, executed) {
			return true
		}
	}
	type candidate struct {
		path     string
		executed bool
	}
	var candidates []candidate
	for _, p := range iterReferencedShellScripts(walkCommand, cwd) {
		candidates = append(candidates, candidate{p, executed})
	}
	if walkCommand != command {
		for _, p := range iterReferencedShellScripts(command, cwd) {
			candidates = append(candidates, candidate{p, false})
		}
	}
	for _, c := range candidates {
		if onCloudPath(c.path) {
			if c.executed {
				return budget.refuseUnreadable(c.path, fmt.Sprintf("`%s` lives on a cloud-synced path (iCloud Drive / ~/Library/CloudStorage) that the guard refuses to open", c.path))
			}
			continue
		}
		resolved := resolveLenient(c.path)
		if visited[resolved] {
			continue
		}
		if !budget.chargePath() {
			if c.executed {
				return budget.exhausted("paths", depth)
			}
			break
		}
		visited[resolved] = true
		text, present, unsafe := readReferencedScript(c.path, budget.bytesRemaining)
		if unsafe {
			if c.executed {
				return budget.refuseUnreadable(c.path, unreadableReason(c.path))
			}
			continue
		}
		if !present && readRemote != nil {
			if !budget.chargeRemoteRead() {
				if c.executed {
					return budget.exhausted("remote reads", depth)
				}
				break
			}
			remoteText, remoteOK := readRemote(c.path)
			text, present, unsafe = sanitizeRemoteScriptText(remoteText, remoteOK, budget.bytesRemaining)
			if unsafe {
				if c.executed {
					return budget.refuseUnreadable(c.path, fmt.Sprintf("`%s` read from the backend exceeds the scan cap (%d bytes) or the remaining walk budget", c.path, MaxReferencedScriptBytes))
				}
				continue
			}
		}
		if !present || text == "" {
			continue
		}
		scriptCwd := filepath.Dir(resolved)
		if !filepath.IsAbs(resolved) {
			scriptCwd = cwd
		}
		if recurse(text, scriptCwd, c.executed) {
			return true
		}
	}
	return false
}

// ScanGatewayLifecycle reports whether a command is unsafe and the non lifecycle refusal when the walk failed closed
func ScanGatewayLifecycle(command, cwd string, readRemote ReadRemoteScriptFunc) (unsafe bool, refusal string) {
	budget := newLifecycleScanBudget()
	defer func() {
		if r := recover(); r != nil {
			envLog(nil).Warn("lifecycle guard referenced-script walk failed, falling back to direct-scan verdict", "panic", fmt.Sprint(r))
			unsafe, refusal = DirectLifecycleScan(command), ""
		}
	}()
	unsafe = containsUnsafeGatewayAction(command, cwd, 0, map[string]bool{}, budget, readRemote, true)
	if unsafe {
		return true, budget.refusal
	}
	return false, ""
}

// ContainsGatewayLifecycleCommandOrReferencedScript detects lifecycle or submit commands including nested scripts
func ContainsGatewayLifecycleCommandOrReferencedScript(command, cwd string, readRemote ReadRemoteScriptFunc) bool {
	unsafe, _ := ScanGatewayLifecycle(command, cwd, readRemote)
	return unsafe
}

// CheckGatewayLifecycle refuses a cron prompt or script holding a gateway lifecycle command
func CheckGatewayLifecycle(env *Env, prompt, script string) error {
	combined := prompt
	pythonScript := false
	refusal := ""
	h := envHome(env)
	if script != "" {
		resolvedScript, ok := resolveScriptPath(h, script)
		if ok && onCloudPath(resolvedScript) {
			return &GatewayLifecycleBlocked{"Blocked: the cron script lives on a cloud-synced path " +
				"(iCloud Drive / ~/Library/CloudStorage). Opening an " +
				"evicted FileProvider placeholder can hang the guard's " +
				"preflight scan indefinitely, so it is refused without " +
				"being read. Move the script to a local, non-cloud path " +
				"(e.g. ~/.icebath/scripts/) and recreate the job."}
		}
		pythonScript = ok && strings.EqualFold(filepath.Ext(resolvedScript), ".py")
		var scriptText string
		scriptText, refusal = readScriptForScanning(h, script)
		if scriptText != "" {
			combined = combined + "\n" + scriptText
		}
	}
	var unsafe bool
	switch {
	case refusal != "":
		unsafe = true
	case pythonScript:
		budget := newLifecycleScanBudget()
		if !budget.chargeText(combined) {
			unsafe = budget.exhausted("text", 0)
			refusal = budget.refusal
		} else {
			unsafe = lifecycleCommandScanWithDataExemption(combined)
		}
	default:
		cwd := ""
		if script != "" {
			cwd = resolveScriptDirectory(h, script)
		}
		unsafe, refusal = ScanGatewayLifecycle(combined, cwd, nil)
	}
	if unsafe && refusal != "" {
		return &GatewayLifecycleBlocked{fmt.Sprintf("Blocked: the lifecycle guard could not scan this cron job or referenced script: %s. ", refusal) +
			"Nothing in the job is known to contain a gateway lifecycle command, but a script " +
			"the job executes must be scannable (a regular text file under 1 MiB) before it can run."}
	}
	if unsafe {
		return &GatewayLifecycleBlocked{"Blocked: cron job contains a gateway lifecycle command or persistent " +
			"launchctl submit operation. This is blocked to prevent agent-driven " +
			"SIGTERM-respawn loops under launchd/systemd supervision " +
			"(#30719). Run `icebath gateway restart` from a shell outside " +
			"the running gateway instead."}
	}
	return nil
}
