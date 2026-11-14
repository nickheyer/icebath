// File reads, writes, replacements and patches over a task's execution environment
package file

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"os"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/tool"
	"github.com/nickheyer/icebath/internal/tool/env"
)

const (
	notRegularSentinel  = "__icebath_not_regular__"
	missingSentinel     = "__icebath_missing__"
	readSentinelPrefix  = "__ICEBATH_RF_"
	writeSentinelPrefix = "__ICEBATH_WF_"
	bytesSentinelPrefix = "__ICEBATH_RB_"
	nativeReadEnv       = "ICEBATH_NATIVE_FILE_READ"
	defaultMount        = "/workspace"
)

var (
	base64Shape = regexp.MustCompile(`^[A-Za-z0-9+/]+={0,2}$`)
	userName    = regexp.MustCompile(`^[a-zA-Z0-9._-]+$`)
)

type WriteGuard interface {
	WriteDenied(ctx context.Context, path, verb string, entry bool) (string, error)
}

type Linter interface {
	SyntaxProblem(ext, content string) (string, bool)
	WantsBaseline(ctx context.Context, o *Operations, ext string) (bool, error)
	Delta(ctx context.Context, o *Operations, path string, pre *string, post string) (LintResult, error)
	Diagnostics(ctx context.Context, o *Operations, path string, pre *string, post string, started time.Time) (string, error)
}

type Options struct {
	Env      tool.Environment
	Config   *config.Store
	Guard    WriteGuard
	Lint     Linter
	Backends *env.Providers
	Search   SearchOptions
	Cwd      string
}

type Operations struct {
	env      tool.Environment
	config   *config.Store
	guard    WriteGuard
	lint     Linter
	backends *env.Providers
	search   SearchOptions
	cwd      string

	mu       sync.Mutex
	commands map[string]bool
	ripgrep  string
	modified map[string]string
}

func New(o Options) (*Operations, error) {
	switch {
	case o.Env == nil:
		return nil, &tool.UnfilledError{Name: "file.Options.Env"}
	case o.Config == nil:
		return nil, &tool.UnfilledError{Name: "file.Options.Config"}
	case o.Guard == nil:
		return nil, &tool.UnfilledError{Name: "file.Options.Guard"}
	case o.Lint == nil:
		return nil, &tool.UnfilledError{Name: "file.Options.Lint"}
	case o.Search.Admission == nil:
		return nil, &tool.UnfilledError{Name: "file.Options.Search.Admission"}
	case o.Search.Ripgrep == nil:
		return nil, &tool.UnfilledError{Name: "file.Options.Search.Ripgrep"}
	case o.Search.RunEnv == nil:
		return nil, &tool.UnfilledError{Name: "file.Options.Search.RunEnv"}
	}
	cwd := o.Cwd
	if cwd == "" {
		cwd = o.Env.Cwd()
	}
	if cwd == "" {
		cwd = "/"
	}
	return &Operations{
		env: o.Env, config: o.Config, guard: o.Guard, lint: o.Lint, backends: o.Backends, search: o.Search, cwd: cwd,
		commands: make(map[string]bool), modified: make(map[string]string),
	}, nil
}

type execResult struct {
	stdout   string
	exitCode int
	cwdError string
}

func (o *Operations) liveCwd() string {
	if cwd := o.env.Cwd(); cwd != "" {
		return cwd
	}
	return o.cwd
}

func (o *Operations) containerBackend(ctx context.Context) (bool, error) {
	kind := o.env.Kind()
	switch kind {
	case "":
		return false, nil
	case "docker", "modal", "daytona", "vercel_sandbox":
		return true, nil
	}
	if o.backends == nil || env.IsBuiltinBackend(kind) {
		return false, nil
	}
	flags, ok, err := o.backends.Flags(ctx, kind, "")
	if err != nil {
		return false, err
	}
	return ok && flags.Container, nil
}

func (o *Operations) exec(ctx context.Context, command string, stdin *string) (execResult, error) {
	return o.execIn(ctx, command, "", 0, stdin)
}

func (o *Operations) execIn(ctx context.Context, command, cwd string, timeout time.Duration, stdin *string) (execResult, error) {
	if cwd == "" {
		cwd = o.liveCwd()
	}
	res, err := o.env.Execute(ctx, command, tool.ExecOptions{Cwd: cwd, Timeout: timeout, Stdin: stdin, RewriteCompoundBackground: true})
	if err != nil {
		return execResult{}, err
	}
	out := execResult{stdout: res.Output, exitCode: res.ExitCode}
	if res.ExitCode == 126 && strings.Contains(res.Output, "cd: ") {
		container, err := o.containerBackend(ctx)
		if err != nil {
			return execResult{}, err
		}
		hint := ""
		if container {
			hint = "; for container backends use a path inside the container, e.g. /workspace"
		}
		out.cwdError = fmt.Sprintf("working directory %s does not exist on the active terminal "+
			"backend (check terminal.cwd or the session cwd%s). %s", quote(cwd), hint, trimSpace(res.Output))
		out.stdout = out.cwdError
	}
	return out, nil
}

func (o *Operations) limits(ctx context.Context) tool.OutputLimits {
	cfg, err := o.config.Load(ctx)
	if err != nil {
		return tool.Limits(nil)
	}
	return tool.Limits(cfg)
}

func (o *Operations) paths() pathStyle {
	return pathStyle{windows: runtime.GOOS == "windows" && o.env.Local()}
}

func (o *Operations) nativeEnabled() bool {
	flag, set := os.LookupEnv(nativeReadEnv)
	if !set {
		flag = "1"
	}
	switch strings.ToLower(trimSpace(flag)) {
	case "0", "false", "no", "off":
		return false
	}
	return runtime.GOOS != "windows" && o.env.HostFilesystem()
}

func (o *Operations) nativePath(path string) string {
	if strings.HasPrefix(path, "/") {
		return path
	}
	return pathStyle{}.join(o.liveCwd(), path)
}

func (o *Operations) hostMount() (string, string) {
	hostRoot, containerRoot, _ := env.HostMount(o.env)
	return hostRoot, containerRoot
}

func (o *Operations) expandPath(ctx context.Context, path string) (string, error) {
	hostRoot, containerRoot := o.hostMount()
	if containerRoot == "" {
		containerRoot = defaultMount
	}
	if translated, ok := translateMountedHostPath(path, hostRoot, containerRoot); ok {
		return translated, nil
	}
	if !strings.HasPrefix(path, "~") {
		return path, nil
	}
	res, err := o.exec(ctx, "echo $HOME", nil)
	if err != nil {
		return "", err
	}
	home := trimSpace(res.stdout)
	if res.exitCode != 0 || home == "" {
		return path, nil
	}
	if path == "~" {
		return home, nil
	}
	if strings.HasPrefix(path, "~/") {
		return home + path[1:], nil
	}
	rest := path[1:]
	user := rest
	if slash := strings.Index(rest, "/"); slash >= 0 {
		user = rest[:slash]
	}
	if user != "" && userName.MatchString(user) {
		expanded, err := o.exec(ctx, "echo ~"+user, nil)
		if err != nil {
			return "", err
		}
		if dir := trimSpace(expanded.stdout); expanded.exitCode == 0 && dir != "" {
			return dir + path[1+len(user):], nil
		}
	}
	return path, nil
}

func shellQuote(arg string) string {
	return "'" + strings.ReplaceAll(arg, "'", `'"'"'`) + "'"
}

func shellArg(path string) string {
	return shellQuote(env.BashSafePath(path))
}

func (o *Operations) shellText(text string) string {
	if runtime.GOOS == "windows" && o.env.Local() {
		text = strings.ReplaceAll(text, `\`, `\\`)
	}
	return shellQuote(text)
}

func nativeToolArg(path string) string {
	if runtime.GOOS == "windows" && path != "" {
		path = strings.ReplaceAll(env.MsysToWindowsPath(path), `\`, "/")
	}
	return shellQuote(path)
}

func (o *Operations) hasCommand(ctx context.Context, name string) (bool, error) {
	if name == "rg" {
		path, err := o.resolveRipgrep(ctx)
		return path != "", err
	}
	o.mu.Lock()
	known, cached := o.commands[name]
	o.mu.Unlock()
	if cached {
		return known, nil
	}
	res, err := o.exec(ctx, "command -v "+name+" >/dev/null 2>&1 && echo 'yes'", nil)
	if err != nil {
		return false, err
	}
	if res.cwdError != "" {
		return false, nil
	}
	known = trimSpace(res.stdout) == "yes"
	o.mu.Lock()
	o.commands[name] = known
	o.mu.Unlock()
	return known, nil
}

func newSentinel(prefix string) (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return prefix + hex.EncodeToString(raw[:]) + "__", nil
}

func splitSegments(output, sentinel string) []string {
	return strings.Split(output, sentinel+"\n")
}

func (o *Operations) cat(ctx context.Context, path string) (execResult, error) {
	return o.exec(ctx, "cat "+shellArg(path)+" 2>/dev/null", nil)
}

func (o *Operations) head(ctx context.Context, path string, n int) (execResult, error) {
	return o.exec(ctx, fmt.Sprintf("head -c %d %s 2>/dev/null", n, shellArg(path)), nil)
}

type fencedReply struct {
	segments  []string
	fenced    bool
	status    int
	hasStatus bool
	reply     execResult
}

func (o *Operations) fencedRead(ctx context.Context, body string, more ...string) (fencedReply, error) {
	sentinel, err := newSentinel(bytesSentinelPrefix)
	if err != nil {
		return fencedReply{}, err
	}
	mark := "echo " + sentinel
	var rest strings.Builder
	for _, cmd := range more {
		rest.WriteString(mark + "; " + cmd + "; ")
	}
	res, err := o.exec(ctx, "{ set +x; } 2>/dev/null; "+mark+"; "+body+"; __hb=$?; "+rest.String()+mark+"; echo $__hb", nil)
	if err != nil {
		return fencedReply{}, err
	}
	segments := splitSegments(res.stdout, sentinel)
	if len(segments) != len(more)+3 {
		return fencedReply{reply: res}, nil
	}
	out := fencedReply{segments: segments[1 : len(segments)-1], fenced: true, reply: res}
	if tokens := spaceFields(stripFenceLeaks(segments[len(segments)-1])); len(tokens) > 0 {
		out.status, out.hasStatus = parseInt(tokens[0])
	}
	return out, nil
}

func matchesSize(data []byte, sizeSegment string) bool {
	tokens := spaceFields(stripFenceLeaks(sizeSegment))
	if len(tokens) != 1 || !allDigits(tokens[0]) {
		return false
	}
	n, ok := parseInt(tokens[0])
	return ok && n == len(data)
}

func decodeBase64Sample(text string) ([]byte, bool) {
	encoded := strings.Join(spaceFields(stripFenceLeaks(text)), "")
	if encoded == "" {
		return []byte{}, true
	}
	if !base64Shape.MatchString(encoded) {
		return nil, false
	}
	data, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return nil, false
	}
	return data, true
}

func (o *Operations) sampleFileBytes(ctx context.Context, path string) ([]byte, bool, error) {
	reply, err := o.fencedRead(ctx, "head -c 1000 "+shellArg(path)+" 2>/dev/null | base64")
	if err != nil {
		return nil, false, err
	}
	if !reply.fenced || !reply.hasStatus || reply.status != 0 {
		return nil, false, nil
	}
	data, ok := decodeBase64Sample(reply.segments[0])
	return data, ok, nil
}

func garbledRead(path string) *execResult {
	return &execResult{stdout: path + ": the backend returned a garbled byte-exact read", exitCode: 1}
}

func failedRead(path, payload string, status int) *execResult {
	text := trimSpace(stripFenceLeaks(payload))
	if text == "" {
		text = fmt.Sprintf("%s: exit %d", path, status)
	}
	return &execResult{stdout: text, exitCode: status}
}

func (o *Operations) readExactBytes(ctx context.Context, path string) ([]byte, *execResult, error) {
	if o.nativeEnabled() {
		if data, ok := readRegular(o.nativePath(path)); ok {
			return data, nil, nil
		}
	}
	arg := shellArg(path)
	reply, err := o.fencedRead(ctx, "base64 < "+arg, "wc -c < "+arg)
	if err != nil {
		return nil, nil, err
	}
	if !reply.fenced {
		if reply.reply.exitCode != 0 {
			return nil, &reply.reply, nil
		}
		return nil, garbledRead(path), nil
	}
	if !reply.hasStatus {
		return nil, garbledRead(path), nil
	}
	if reply.status == 127 {
		return o.readExactBytesHex(ctx, path)
	}
	payload, size := reply.segments[0], reply.segments[1]
	if reply.status != 0 {
		return nil, failedRead(path, payload, reply.status), nil
	}
	data, ok := decodeBase64Sample(payload)
	if !ok || !matchesSize(data, size) {
		return nil, garbledRead(path), nil
	}
	return data, nil, nil
}

func (o *Operations) readExactBytesHex(ctx context.Context, path string) ([]byte, *execResult, error) {
	arg := shellArg(path)
	reply, err := o.fencedRead(ctx, "od -An -v -tx1 < "+arg, "wc -c < "+arg)
	if err != nil {
		return nil, nil, err
	}
	unavailable := &execResult{stdout: path + ": this backend has neither base64 nor od, so a byte-exact read is unavailable", exitCode: 1}
	if !reply.fenced {
		if reply.reply.exitCode != 0 {
			return nil, &reply.reply, nil
		}
		return nil, unavailable, nil
	}
	if !reply.hasStatus || reply.status == 127 {
		return nil, unavailable, nil
	}
	payload, size := reply.segments[0], reply.segments[1]
	if reply.status != 0 {
		return nil, failedRead(path, payload, reply.status), nil
	}
	data, err := hex.DecodeString(strings.Join(spaceFields(stripFenceLeaks(payload)), ""))
	if err != nil || !matchesSize(data, size) {
		return nil, garbledRead(path), nil
	}
	return data, nil, nil
}

func likelyBinaryBytes(sample []byte) bool {
	if len(sample) == 0 {
		return false
	}
	for _, c := range sample {
		if c == 0 {
			return true
		}
	}
	at := firstInvalid(sample)
	if at < 0 {
		return false
	}
	return at < len(sample)-3
}

func likelyBinaryText(path, sample string) bool {
	if HasBinaryExtension(path) {
		return true
	}
	if sample == "" {
		return false
	}
	head := charPrefix(sample, 1000)
	if strings.Contains(head, replacementChar) {
		return true
	}
	total, control := 0, 0
	for _, r := range head {
		total++
		if r < 32 && r != '\n' && r != '\r' && r != '\t' {
			control++
		}
	}
	return float64(control)/float64(total) > 0.30
}

func (o *Operations) isImage(path string) bool {
	return isImageExtension(strings.ToLower(o.paths().ext(path)))
}

func (o *Operations) detectBinary(ctx context.Context, path string) (bool, []byte, bool, error) {
	sample, ok, err := o.sampleFileBytes(ctx, path)
	if err != nil {
		return false, nil, false, err
	}
	if ok {
		return HasBinaryExtension(path) || likelyBinaryBytes(sample), sample, true, nil
	}
	res, err := o.head(ctx, path, 1000)
	if err != nil {
		return false, nil, false, err
	}
	return likelyBinaryText(path, stripFenceLeaks(res.stdout)), nil, false, nil
}

func addLineNumbers(content string, startLine, maxLineLength int) string {
	lines := strings.Split(strings.TrimSuffix(content, "\n"), "\n")
	var b strings.Builder
	b.Grow(len(content) + 8*len(lines))
	for i, line := range lines {
		if i > 0 {
			b.WriteByte('\n')
		}
		fmt.Fprintf(&b, "%d|", startLine+i)
		if charLen(line) <= maxLineLength {
			b.WriteString(line)
			continue
		}
		b.WriteString(charPrefix(line, maxLineLength))
		b.WriteString("... [truncated]")
	}
	return b.String()
}

func anyLineLonger(content string, maxLineLength int) bool {
	for _, line := range strings.Split(content, "\n") {
		if charLen(line) > maxLineLength {
			return true
		}
	}
	return false
}

func fileDiff(oldContent, newContent, filename string) string {
	return diffText(oldContent, newContent, "a/"+filename, "b/"+filename)
}
