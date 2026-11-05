package tools

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
	"weak"

	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// fileOperations is the file backend every terminal environment exposes to the file tools
type fileOperations interface {
	readFile(path string, offset, limit int) *ReadResult
	ReadFileRaw(path string) *ReadResult
	readFileBytes(path string, maxBytes int) *ReadResult
	WriteFile(path, content string, preContent *string) *WriteResult
	patchReplace(path, oldString, newString string, replaceAll bool, probe *editProbe) *PatchResult
	patchV4A(patchContent string, probe *editProbe) *PatchResult
	DeleteFile(path string) *WriteResult
	MoveFile(src, dst string) *WriteResult
	search(pattern, path, target string, fileGlob string, limit, offset int, outputMode string, context int, order string) *SearchResult
}

var _ fileOperations = (*shellFileOps)(nil)

// shellFileOps runs every file operation as a shell command through one terminal environment
type shellFileOps struct {
	env         *Env
	terminalEnv Environment
	cwd         string
	ctx         context.Context
	// Guards the three caches below, shared by every context bound copy
	cacheMu              *sync.Mutex
	commandCache         map[string]bool
	rgResolutionCache    map[string]string
	rgModifiedCapability map[string]*string
}

// File backend over a terminal environment, cwd falling back to the live env cwd then the root
func newShellFileOps(env *Env, terminalEnv Environment, cwd string) *shellFileOps {
	if cwd == "" && terminalEnv != nil {
		cwd = terminalEnv.Cwd()
	}
	if cwd == "" {
		cwd = "/"
	}
	return &shellFileOps{
		env: env, terminalEnv: terminalEnv, cwd: cwd, ctx: context.Background(),
		cacheMu:              &sync.Mutex{},
		commandCache:         map[string]bool{},
		rgResolutionCache:    map[string]string{},
		rgModifiedCapability: map[string]*string{},
	}
}

// Copy sharing the caches whose commands run under the call context for interrupts and workers
func (s *shellFileOps) withContext(ctx context.Context) *shellFileOps {
	c := *s
	if ctx == nil {
		ctx = context.Background()
	}
	c.ctx = ctx
	return &c
}

// Live backend cwd, else the cwd captured at construction
func (s *shellFileOps) liveCwd() string {
	if s.terminalEnv != nil {
		if c := s.terminalEnv.Cwd(); c != "" {
			return c
		}
	}
	return s.cwd
}

// Runs a command in the explicit cwd, else the live env cwd, else the init cwd
func (s *shellFileOps) exec(command, cwd string, timeout time.Duration) *ExecuteResult {
	return s.execStdin(command, cwd, timeout, nil)
}

// Command run with optional piped stdin, naming a failed cd wrapper and a lost stdin write
func (s *shellFileOps) execStdin(command, cwd string, timeout time.Duration, stdin *string) *ExecuteResult {
	opts := ExecOptions{Stdin: stdin}
	if timeout > 0 {
		opts.Timeout = max(1, int(timeout/time.Second))
	}
	effectiveCwd := cwd
	if effectiveCwd == "" {
		effectiveCwd = s.liveCwd()
	}
	opts.Cwd = effectiveCwd
	res, err := s.terminalEnv.Execute(s.ctx, command, opts)
	if err != nil {
		panic(err)
	}
	exitCode, output := res.ReturnCode, res.Output
	cwdError := ""
	if exitCode == 126 && strings.Contains(output, "cd: ") {
		hint := ""
		if t := s.terminalEnv.EnvType(); t != "" && isContainerBackend(t) {
			hint = "; for container backends use a path inside the container, e.g. /workspace"
		}
		output = "working directory " + pyReprStr(effectiveCwd) + " does not exist on the active terminal " +
			"backend (check terminal.cwd or the session cwd" + hint + "). " + pyStrip(output)
		cwdError = output
	}
	if res.StdinError != "" && exitCode == 0 {
		exitCode = 1
	}
	return &ExecuteResult{Stdout: output, ExitCode: exitCode, CwdError: cwdError}
}

// Whether a command exists on the backend, cached, rg resolved fresh until found
func (s *shellFileOps) hasCommand(cmd string) bool {
	if cmd == "rg" {
		return s.resolveCommand(cmd) != ""
	}
	s.cacheMu.Lock()
	cached, ok := s.commandCache[cmd]
	s.cacheMu.Unlock()
	if ok {
		return cached
	}
	result := s.exec("command -v "+cmd+" >/dev/null 2>&1 && echo 'yes'", "", 0)
	if result.CwdError != "" {
		return false
	}
	found := pyStrip(result.Stdout) == "yes"
	s.cacheMu.Lock()
	s.commandCache[cmd] = found
	s.cacheMu.Unlock()
	return found
}

// File contents with stderr silenced, non zero exit for a missing file
func (s *shellFileOps) cat(path string) *ExecuteResult {
	return s.exec("cat "+s.escapeShellArg(path, true)+" 2>/dev/null", "", 0)
}

// First bytes of a file with stderr silenced
func (s *shellFileOps) head(path string, nbytes int) *ExecuteResult {
	return s.exec("head -c "+strconv.Itoa(nbytes)+" "+s.escapeShellArg(path, true)+" 2>/dev/null", "", 0)
}

// Snippet through the backend python3, retried with python when only that name exists
func (s *shellFileOps) runPythonSnippet(snippet string) *ExecuteResult {
	result := s.exec("python3 -c "+s.escapeShellArg(snippet, true), "", 0)
	if result.ExitCode != 0 && strings.Contains(result.Stdout, "python3") {
		result = s.exec("python -c "+s.escapeShellArg(snippet, true), "", 0)
	}
	return result
}

// Sentinel delimited segments of body and each extra command, body exit status, and the reply
func (s *shellFileOps) fencedRead(body string, more ...string) (segments []string, rc int, rcOK bool, reply *ExecuteResult, fenced bool) {
	sentinel := newFileOpsSentinel(bytesSentinelPrefix)
	mark := "echo " + sentinel
	var rest strings.Builder
	for _, cmd := range more {
		rest.WriteString(mark + "; " + cmd + "; ")
	}
	reply = s.exec("{ set +x; } 2>/dev/null; "+mark+"; "+body+"; __hb=$?; "+rest.String()+mark+"; echo $__hb", "", 0)
	parts := splitSegments(reply.Stdout, sentinel)
	if len(parts) != len(more)+3 {
		return nil, 0, false, reply, false
	}
	segments = parts[1 : len(parts)-1]
	fields := pyFields(stripTerminalFenceLeaks(parts[len(parts)-1]))
	if len(fields) == 0 {
		return segments, 0, false, reply, true
	}
	n, ok := pyIntParse(fields[0])
	return segments, n, ok, reply, true
}

// Whether data is exactly as long as the single integer the size segment holds
func matchesSize(data []byte, sizeSegment string) bool {
	tokens := pyFields(stripTerminalFenceLeaks(sizeSegment))
	if len(tokens) != 1 || !isASCIIDigits(tokens[0]) {
		return false
	}
	n, err := strconv.Atoi(tokens[0])
	return err == nil && n == len(data)
}

// Reports whether s is a non empty run of ASCII digits
func isASCIIDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// Python str.split with no argument
func pyFields(s string) []string { return strings.FieldsFunc(s, pyIsSpace) }

// First raw bytes of a file carried as base64, false when no clean base64 came back
func (s *shellFileOps) sampleFileBytes(path string, length int) ([]byte, bool) {
	segments, rc, rcOK, _, fenced := s.fencedRead("head -c " + strconv.Itoa(length) + " " + s.escapeShellArg(path, true) + " 2>/dev/null | base64")
	if !fenced || !rcOK || rc != 0 {
		return nil, false
	}
	return decodeBase64Sample(segments[0])
}

// Exact file bytes natively on a local POSIX host, else fenced base64 checked against wc
func (s *shellFileOps) readExactBytes(path string) ([]byte, *ExecuteResult) {
	if s.nativeReadEnabled() {
		if data, ok := nativeReadRegular(s.nativePath(path)); ok {
			return data, nil
		}
	}
	arg := s.escapeShellArg(path, true)
	segments, rc, rcOK, result, fenced := s.fencedRead("base64 < "+arg, "wc -c < "+arg)
	garbled := &ExecuteResult{Stdout: path + ": the backend returned a garbled byte-exact read", ExitCode: 1}
	if !fenced {
		if result.ExitCode != 0 {
			return nil, result
		}
		return nil, garbled
	}
	if !rcOK {
		return nil, garbled
	}
	if rc == 127 {
		return s.readExactBytesHex(path)
	}
	payload, size := segments[0], segments[1]
	if rc != 0 {
		return nil, failedRead(path, payload, rc)
	}
	data, ok := decodeBase64Sample(payload)
	if !ok || !matchesSize(data, size) {
		return nil, garbled
	}
	return data, nil
}

// Whole regular file read through one non blocking open, false for anything else
func nativeReadRegular(full string) ([]byte, bool) {
	f, err := nativeOpenRegular(full)
	if err != nil || f == nil {
		return nil, false
	}
	defer f.Close()
	data, err := io.ReadAll(f)
	if err != nil {
		return nil, false
	}
	return data, true
}

// Backend diagnostic of a read that ran and failed, else a bare exit status
func failedRead(path, payload string, rc int) *ExecuteResult {
	msg := pyStrip(stripTerminalFenceLeaks(payload))
	if msg == "" {
		msg = path + ": exit " + strconv.Itoa(rc)
	}
	return &ExecuteResult{Stdout: msg, ExitCode: rc}
}

// od transport for a backend without base64, a transport error and never a not found when od is missing
func (s *shellFileOps) readExactBytesHex(path string) ([]byte, *ExecuteResult) {
	arg := s.escapeShellArg(path, true)
	segments, rc, rcOK, result, fenced := s.fencedRead("od -An -v -tx1 < "+arg, "wc -c < "+arg)
	unavailable := &ExecuteResult{Stdout: path + ": this backend has neither base64 nor od, so a byte-exact read is unavailable", ExitCode: 1}
	if !fenced {
		if result.ExitCode != 0 {
			return nil, result
		}
		return nil, unavailable
	}
	if !rcOK || rc == 127 {
		return nil, unavailable
	}
	payload, size := segments[0], segments[1]
	if rc != 0 {
		return nil, failedRead(path, payload, rc)
	}
	data, err := pyBytesFromHex(strings.Join(pyFields(stripTerminalFenceLeaks(payload)), ""))
	if err != nil || !matchesSize(data, size) {
		return nil, &ExecuteResult{Stdout: path + ": the backend returned a garbled byte-exact read", ExitCode: 1}
	}
	return data, nil
}

// bytes.fromhex of a whitespace free string
func pyBytesFromHex(s string) ([]byte, error) {
	if len(s)%2 != 0 {
		return nil, hex.ErrLength
	}
	return hex.DecodeString(s)
}

var base64SampleRe = regexp.MustCompile(`^[A-Za-z0-9+/]+={0,2}$`)

// One base64 transport reply decoded after whitespace joining, false on any validation failure
func decodeBase64Sample(text string) ([]byte, bool) {
	encoded := strings.Join(pyFields(stripTerminalFenceLeaks(text)), "")
	if encoded == "" {
		return []byte{}, true
	}
	if !base64SampleRe.MatchString(encoded) {
		return nil, false
	}
	data, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return nil, false
	}
	return data, true
}

// Text only when valid UTF-8 apart from one sequence cut at the very end, NUL meaning binary
func isLikelyBinaryBytes(sample []byte) bool {
	if len(sample) == 0 {
		return false
	}
	for _, c := range sample {
		if c == 0 {
			return true
		}
	}
	for i := 0; i < len(sample); {
		r, size := utf8.DecodeRune(sample[i:])
		if r == utf8.RuneError && size == 1 {
			return i < len(sample)-3
		}
		i += size
	}
	return false
}

// Text layer binary check by extension, replacement characters, or over 30 percent control characters
func isLikelyBinary(path, contentSample string) bool {
	if hasBinaryExtension(path) {
		return true
	}
	if contentSample == "" {
		return false
	}
	head := []rune(contentSample)
	total := len(head)
	if len(head) > 1000 {
		head = head[:1000]
	}
	nonPrintable := 0
	for _, c := range head {
		if c == utf8.RuneError {
			return true
		}
		if c < 32 && c != '\n' && c != '\r' && c != '\t' {
			nonPrintable++
		}
	}
	return float64(nonPrintable)/float64(min(total, 1000)) > 0.30
}

// Image extensions whose files redirect to the vision tool
var imageExtensions = setOf(".png", ".jpg", ".jpeg", ".gif", ".webp", ".bmp", ".ico")

func isImagePath(path string) bool { return imageExtensions[pyLower(pySplitextExt(path))] }

// Numbered page with a compact gutter, each line clamped to tool_output.max_line_length characters
func (s *shellFileOps) addLineNumbers(content string, startLine int) string {
	maxLen := ToolOutputLimits(s.env).MaxLineLength
	content = strings.TrimSuffix(content, "\n")
	lines := strings.Split(content, "\n")
	var b strings.Builder
	for i, line := range lines {
		if i > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(strconv.Itoa(startLine + i))
		b.WriteByte('|')
		if pyLen(line) <= maxLen {
			b.WriteString(line)
		} else {
			b.WriteString(runePrefix(line, maxLen) + "... [truncated]")
		}
	}
	return b.String()
}

// Python len of the str s models, a surrogate or stray byte counting as one character
func pyLen(s string) int { return pystr.Len(s) }

var tildeUserRe = regexp.MustCompile(`^[a-zA-Z0-9._-]+$`)

// Mounted host path rewritten to its container path, then tilde expanded with the backend HOME
func (s *shellFileOps) expandPath(path string) string {
	hostRoot := s.terminalEnv.HostCwd()
	containerRoot := s.terminalEnv.HostCwdMount()
	if containerRoot == "" {
		containerRoot = "/workspace"
	}
	if translated := translateMountedHostPath(path, hostRoot, containerRoot); translated != "" {
		return translated
	}
	if path == "" || !strings.HasPrefix(path, "~") {
		return path
	}
	result := s.exec("echo $HOME", "", 0)
	home := pyStrip(result.Stdout)
	if result.ExitCode != 0 || home == "" {
		return path
	}
	if path == "~" {
		return home
	}
	if strings.HasPrefix(path, "~/") {
		return home + path[1:]
	}
	rest := path[1:]
	username := rest
	if i := strings.Index(rest, "/"); i >= 0 {
		username = rest[:i]
	}
	if username != "" && tildeUserRe.MatchString(username) {
		expanded := s.exec("echo ~"+username, "", 0)
		if out := pyStrip(expanded.Stdout); expanded.ExitCode == 0 && out != "" {
			return out + path[1+len(username):]
		}
	}
	return path
}

// Single quoted shell argument, Windows drive paths rewritten to the Git Bash form when translating
func (s *shellFileOps) escapeShellArg(arg string, translatePath bool) string {
	if translatePath {
		arg = bashSafePath(arg)
	} else if isWindowsHost && s.terminalEnv != nil && s.terminalEnv.EnvType() == "local" {
		arg = strings.ReplaceAll(arg, `\`, `\\`)
	}
	return "'" + strings.ReplaceAll(arg, "'", `'"'"'`) + "'"
}

// Single quoted argument for a native Windows binary in C slash form, plain quoting elsewhere
func (s *shellFileOps) escapeNativeToolArg(arg string) string {
	if isWindowsHost && arg != "" {
		arg = strings.ReplaceAll(msysToWindowsPath(arg), `\`, "/")
	}
	return "'" + strings.ReplaceAll(arg, "'", `'"'"'`) + "'"
}

// Atomic write through a same directory temp and rename, natively on a local POSIX host
func (s *shellFileOps) atomicWrite(path, content string) *ExecuteResult {
	if s.nativeReadEnabled() {
		data, err := pystr.SurrogateEscapeEncode(content)
		if err != nil {
			panic(err)
		}
		if msg := nativeAtomicWrite(s.nativePath(path), data); msg != "" {
			return &ExecuteResult{Stdout: msg, ExitCode: 1}
		}
		return &ExecuteResult{}
	}
	parent := pathDirnameOS(path)
	if parent == "" {
		parent = "."
	}
	qPath := s.escapeShellArg(path, true)
	qParent := s.escapeShellArg(parent, true)
	tmpl := s.escapeShellArg(".icebath-tmp.XXXXXX", true)
	script := "set -e; " +
		"d=" + qParent + "; t=" + qPath + "; " +
		`if [ -L "$t" ]; then ` +
		`rt="$(readlink -f "$t" 2>/dev/null || realpath "$t" 2>/dev/null || true)"; ` +
		`[ -n "$rt" ] && { t="$rt"; d="$(dirname "$t")"; }; ` +
		"fi; " +
		`mkdir -p "$d"; ` +
		`tmp="$(mktemp -p "$d" ` + tmpl + ` 2>/dev/null ` +
		`|| mktemp "$d/.icebath-tmp.$$.XXXXXX" 2>/dev/null ` +
		`|| { tmp="$d/.icebath-tmp.$$"; : > "$tmp" && echo "$tmp"; })"; ` +
		`[ -n "$tmp" ] || { echo "atomic write: could not create temp file" >&2; exit 1; }; ` +
		`trap 'rm -f \"$tmp\"' EXIT; ` +
		`if [ -e "$t" ]; then ` +
		`m="$(stat -c%a "$t" 2>/dev/null || stat -f%Lp "$t" 2>/dev/null || true)"; ` +
		`[ -n "$m" ] && chmod "$m" "$tmp" 2>/dev/null || true; ` +
		"fi; " +
		`cat > "$tmp"; ` +
		`if [ ! -e "$t" ]; then chmod "=rw" "$tmp" 2>/dev/null || true; fi; ` +
		`mv -f "$tmp" "$t"; ` +
		"trap - EXIT"
	return s.execStdin(script, "", 0, &content)
}

// Whether the on disk file starts with a UTF-8 byte order mark, always probing disk
func (s *shellFileOps) fileHasBOM(path string) bool {
	head := s.head(path, 3)
	return head.ExitCode == 0 && hasBOM(head.Stdout)
}

// Unified diff of two contents under a slash and b slash file headers
func unifiedDiffText(oldContent, newContent, filename string) string {
	return strings.Join(unifiedDiff(pySplitLinesKeepEnds(oldContent), pySplitLinesKeepEnds(newContent), "a/"+filename, "b/"+filename, 3), "")
}

// Echoed by the size probe when the path exists but is not a regular file
const notRegularSentinel = "__icebath_not_regular__"

// Echoed by the compound probes when the path does not exist
const missingSentinel = "__icebath_missing__"

const (
	readSentinelPrefix  = "__ICEBATH_RF_"
	writeSentinelPrefix = "__ICEBATH_WF_"
	bytesSentinelPrefix = "__ICEBATH_RB_"
)

// Per call separator line with 128 random bits kept outside the base64 alphabet
func newFileOpsSentinel(prefix string) string {
	token := make([]byte, 16)
	if _, err := rand.Read(token); err != nil {
		panic(err)
	}
	return prefix + hex.EncodeToString(token) + "__"
}

// Compound probe stdout split on its sentinel lines, the status after the last one
func splitSegments(output, sentinel string) []string {
	return strings.Split(output, sentinel+"\n")
}

// Error for a path that exists but would block if read
func notRegularError(path string) *ReadResult {
	return &ReadResult{Error: "Cannot read '" + path + "': not a regular file (directory, dangling symlink, " +
		"FIFO, socket, or device). Reading it could block indefinitely."}
}

// Byte size of a regular file with ok, missing, not_regular, bad_size, env_unavailable or the cwd error
func (s *shellFileOps) probeRegularFile(path string) (int64, string) {
	arg := s.escapeShellArg(path, true)
	statResult := s.exec("if [ -f "+arg+" ]; then wc -c < "+arg+" 2>/dev/null; "+
		"elif [ -e "+arg+" ] || [ -L "+arg+" ]; then echo "+notRegularSentinel+"; "+
		"else echo "+missingSentinel+"; fi", "", 0)
	out := pyStrip(stripTerminalFenceLeaks(statResult.Stdout))
	switch {
	case out == missingSentinel:
		return 0, "missing"
	case out == notRegularSentinel:
		return 0, "not_regular"
	case statResult.ExitCode != 0:
		if statResult.CwdError != "" {
			return 0, statResult.CwdError
		}
		return 0, "env_unavailable"
	}
	n, ok := pyIntParse(out)
	if !ok {
		return 0, "bad_size"
	}
	return int64(n), "ok"
}

// Environment unavailable read error, or the named cwd error verbatim
func envUnavailableError(path, status string) *ReadResult {
	if status != "env_unavailable" {
		return &ReadResult{Error: status}
	}
	return &ReadResult{Error: "Terminal environment unavailable: could not stat " + path + " " +
		"(the sandbox may still be starting or was removed). Retry shortly."}
}

// Byte layer binary verdict with its sample, else the text heuristic with no sample
func (s *shellFileOps) detectBinary(path string) (bool, []byte) {
	if sample, ok := s.sampleFileBytes(path, 1000); ok {
		return hasBinaryExtension(path) || isLikelyBinaryBytes(sample), sample
	}
	sampleOutput := stripTerminalFenceLeaks(s.head(path, 1000).Stdout)
	return isLikelyBinary(path, sampleOutput), nil
}

// Human name of binary content from its magic bytes
func identifyBinaryBytes(sample []byte) string {
	if name := IdentifyBinaryBytes(sample); name != "" {
		return name
	}
	return "unknown binary"
}

// One line binary file refusal naming the detected type and the size
func describeBinaryFile(sample []byte, fileSize int64) string {
	kind := identifyBinaryBytes(sample)
	var size string
	switch {
	case fileSize >= 1024*1024:
		size = fmt.Sprintf("%.1f MB", float64(fileSize)/(1024*1024))
	case fileSize >= 1024:
		size = fmt.Sprintf("%.1f KB", float64(fileSize)/1024)
	default:
		size = strconv.FormatInt(fileSize, 10) + " bytes"
	}
	return "Binary file (" + kind + ", " + size + ") — cannot display as text."
}

// Python interpreter for the backend, the host interpreter on a host filesystem backend
func (s *shellFileOps) pythonInterpreterCmd() string {
	if s.lspLocalOnly() {
		if py := hostPythonInterpreter(); py != "" {
			return s.escapeShellArg(py, true)
		}
	}
	return "python3"
}

// Snippet run base64 wrapped so it survives every quoting layer
func (s *shellFileOps) execPythonSnippet(snippet, py string) *ExecuteResult {
	encoded := base64.StdEncoding.EncodeToString([]byte(snippet))
	if py == "" {
		py = s.pythonInterpreterCmd()
	}
	return s.exec(py+` -c "import base64; exec(base64.b64decode('`+encoded+`').decode())"`, "", 0)
}

var (
	hostPythonOnce sync.Once
	hostPythonPath string
)

// First working python3 or python on the host PATH, skipping the Windows Store alias stubs
func hostPythonInterpreter() string {
	hostPythonOnce.Do(func() {
		for _, name := range []string{"python3", "python"} {
			p, err := exec.LookPath(name)
			if err != nil {
				continue
			}
			if isWindowsHost && strings.Contains(strings.ToLower(p), `\microsoft\windowsapps\`) {
				continue
			}
			hostPythonPath = p
			return
		}
	})
	return hostPythonPath
}

// Native reads and writes only on POSIX with the host filesystem, ICEBATH_NATIVE_FILE_READ off switch
func (s *shellFileOps) nativeReadEnabled() bool {
	flag := strings.ToLower(pyStrip(os.Getenv("ICEBATH_NATIVE_FILE_READ")))
	if flag == "" {
		flag = "1"
	}
	switch flag {
	case "0", "false", "no", "off":
		return false
	}
	return runtime.GOOS != "windows" && s.lspLocalOnly()
}

// Path resolved against the live backend cwd the way the shell resolves it
func (s *shellFileOps) nativePath(path string) string {
	if filepath.IsAbs(path) {
		return path
	}
	return pyOSPathJoin(s.liveCwd(), path)
}

// Snapshots of native reads keyed weakly by result so an unclaimed one dies with its result
var readSnapshots sync.Map

// Attaches the stat identity and digest a native read saw to its result
func attachReadSnapshot(r *ReadResult, v fileVersion) {
	key := weak.Make(r)
	readSnapshots.Store(key, v)
	runtime.AddCleanup(r, func(k weak.Pointer[ReadResult]) { readSnapshots.Delete(k) }, key)
}

// Version snapshot a native read attached to its result, false when the read had none
func readResultSnapshot(r *ReadResult) (fileVersion, bool) {
	v, ok := readSnapshots.Load(weak.Make(r))
	if !ok {
		return fileVersion{}, false
	}
	return v.(fileVersion), true
}

// Python posixpath or ntpath splitext suffix ignoring leading dots of the final component
func pySplitextExt(p string) string {
	sepIndex := strings.LastIndex(p, "/")
	if isWindowsHost {
		sepIndex = max(sepIndex, strings.LastIndex(p, `\`))
	}
	dotIndex := strings.LastIndex(p, ".")
	if dotIndex > sepIndex {
		for i := sepIndex + 1; i < dotIndex; i++ {
			if p[i] != '.' {
				return p[dotIndex:]
			}
		}
	}
	return ""
}

// Python posixpath or ntpath splitext root
func pySplitextRoot(p string) string {
	return p[:len(p)-len(pySplitextExt(p))]
}
