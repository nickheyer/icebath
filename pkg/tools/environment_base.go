package tools

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// Extra wait past the inner poll deadline before the outer backstop fires
const executeWaitBoundGrace = 2 * time.Second

// Longest a hard exit kill of the foreground commands may block
const hardKillBudget = 500 * time.Millisecond

// Seconds a base64 file fetch over the exec channel may take
const fetchTimeoutSeconds = 300

// Collector capacity that never evicts so bounded and unbounded capture share one path
const unboundedCaptureChars = int(^uint(0) >> 1)

// Age after which a terminal output spill file is pruned
const spillMaxAge = 7 * 24 * time.Hour

// Hard ceiling on one spill file
const collectorSpillCap = 5_000_000

// Seconds the sudo -n probe may take inside a backend
const sudoProbeTimeoutSeconds = 3

// Foreground commands in flight in this process across every environment
var liveForeground = struct {
	mu             sync.Mutex
	cond           *sync.Cond
	procs          map[ProcessHandle]*baseEnv
	fenced         bool
	spawnsInFlight int
}{procs: map[ProcessHandle]*baseEnv{}}

func init() {
	liveForeground.cond = sync.NewCond(&liveForeground.mu)
}

// Counts a spawn past the exit fence, false once the fence is up
func enterForegroundSpawn() bool {
	liveForeground.mu.Lock()
	defer liveForeground.mu.Unlock()
	if liveForeground.fenced {
		return false
	}
	liveForeground.spawnsInFlight++
	return true
}

// Publishes a spawned process and reports whether the exit fence went up meanwhile
func leaveForegroundSpawn(b *baseEnv, spawned ProcessHandle) bool {
	liveForeground.mu.Lock()
	defer liveForeground.mu.Unlock()
	liveForeground.spawnsInFlight--
	if spawned != nil {
		liveForeground.procs[spawned] = b
	}
	liveForeground.cond.Broadcast()
	return liveForeground.fenced
}

// Drops a finished foreground process from the live table
func forgetForegroundSpawn(spawned ProcessHandle) {
	liveForeground.mu.Lock()
	delete(liveForeground.procs, spawned)
	liveForeground.mu.Unlock()
}

// KillLiveForegroundProcesses kills every in flight foreground command tree and returns how many were signalled
func KillLiveForegroundProcesses(now bool) int {
	if !now {
		liveForeground.mu.Lock()
		type pair struct {
			b *baseEnv
			p ProcessHandle
		}
		live := make([]pair, 0, len(liveForeground.procs))
		for p, b := range liveForeground.procs {
			live = append(live, pair{b, p})
		}
		liveForeground.mu.Unlock()
		for _, it := range live {
			it.b.self.killProcess(it.p)
		}
		return len(live)
	}
	deadline := time.Now().Add(hardKillBudget)
	liveForeground.mu.Lock()
	liveForeground.fenced = true
	stop := time.AfterFunc(hardKillBudget, func() {
		liveForeground.mu.Lock()
		liveForeground.cond.Broadcast()
		liveForeground.mu.Unlock()
	})
	for liveForeground.spawnsInFlight != 0 && time.Now().Before(deadline) {
		liveForeground.cond.Wait()
	}
	stop.Stop()
	type pair struct {
		b *baseEnv
		p ProcessHandle
	}
	live := make([]pair, 0, len(liveForeground.procs))
	for p, b := range liveForeground.procs {
		live = append(live, pair{b, p})
	}
	liveForeground.mu.Unlock()
	var remote sync.WaitGroup
	for _, it := range live {
		if _, local := it.p.(*ExecProcess); local {
			it.b.self.forceKillProcess(it.p)
			continue
		}
		remote.Add(1)
		go func(it pair) {
			defer remote.Done()
			it.b.self.forceKillProcess(it.p)
		}(it)
	}
	done := make(chan struct{})
	go func() {
		remote.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Until(deadline)):
	}
	return len(live)
}

// FileFetchError reports a file that could not be extracted from the backend filesystem
type FileFetchError struct {
	Msg string
}

// Error returns the fetch failure text
func (e *FileFetchError) Error() string { return e.Msg }

// EnvironmentConnectionError is an infrastructure failure of a terminal backend, never a nonzero command exit
type EnvironmentConnectionError struct {
	Reason    string
	RetryHint string
}

// Default retry guidance of a connection class failure
const defaultConnectionRetryHint = "This is an infrastructure failure, not a command failure. " +
	"Verify the backend is reachable (network, service running, " +
	"credentials), then retry the same command — recovery is " +
	"automatic once the backend is back."

// NewEnvironmentConnectionError builds the error with the default retry hint when none is given
func NewEnvironmentConnectionError(reason, retryHint string) *EnvironmentConnectionError {
	if retryHint == "" {
		retryHint = defaultConnectionRetryHint
	}
	return &EnvironmentConnectionError{Reason: reason, RetryHint: retryHint}
}

// Error returns the failure reason
func (e *EnvironmentConnectionError) Error() string { return e.Reason }

type activityCallbackKey struct{}

// WithActivityCallback binds the liveness callback long waits fire for the gateway inactivity timeout
func WithActivityCallback(ctx context.Context, cb func(string)) context.Context {
	return context.WithValue(ctx, activityCallbackKey{}, cb)
}

// ActivityCallbackOf returns the bound liveness callback or nil
func ActivityCallbackOf(ctx context.Context) func(string) {
	if ctx == nil {
		return nil
	}
	cb, _ := ctx.Value(activityCallbackKey{}).(func(string))
	return cb
}

// Timestamps of one long wait for the activity throttle
type activityState struct {
	start, lastTouch time.Time
	interval         time.Duration
}

// Fresh throttle state firing at most every ten seconds
func newActivityState() *activityState {
	now := time.Now()
	return &activityState{start: now, lastTouch: now, interval: 10 * time.Second}
}

// Fires the activity callback or an activity event at most once per interval
func touchActivityIfDue(ctx context.Context, env *Env, st *activityState, label string) {
	now := time.Now()
	if now.Sub(st.lastTouch) < st.interval {
		return
	}
	st.lastTouch = now
	text := fmt.Sprintf("%s (%ds elapsed)", label, int(now.Sub(st.start).Seconds()))
	if cb := ActivityCallbackOf(ctx); cb != nil {
		func() {
			defer func() {
				if r := recover(); r != nil {
					toolsLogger(env).Debug("activity callback failed during a long-running command", "panic", r)
				}
			}()
			cb(text)
		}()
		return
	}
	env.Emit(Event{Kind: "activity", Text: text})
}

// SandboxDir is the host side root for all sandbox storage, TERMINAL_SANDBOX_DIR over home/sandboxes
func SandboxDir(env *Env) (string, error) {
	custom := ""
	if policy, err := terminalPolicy(env); err == nil {
		custom = policy.Get("TERMINAL_SANDBOX_DIR", "")
	}
	if custom == "" {
		custom = os.Getenv("TERMINAL_SANDBOX_DIR")
	}
	p := filepath.Join(envHome(env), "sandboxes")
	if custom != "" {
		p = expandPathVars(custom)
	}
	if err := os.MkdirAll(p, 0o755); err != nil {
		return "", err
	}
	return p, nil
}

// Snapshot store read as the dict json.loads gives, missing or damaged files count as empty
func envLoadJSONStore(path string) *pyjson.Object {
	data, err := os.ReadFile(path)
	if err != nil {
		return pyjson.NewObject()
	}
	text, err := pystr.DecodeUTF8Sig(data)
	if err != nil {
		return pyjson.NewObject()
	}
	parsed, err := pyjson.Loads(text)
	out, ok := parsed.(*pyjson.Object)
	if err != nil || !ok || out == nil {
		return pyjson.NewObject()
	}
	return out
}

// Writes the store as two space indented json.dumps text through an atomic rename
func envSaveJSONStore(ctx context.Context, path string, data *pyjson.Object) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return config.AtomicJSONWriteWith(ctx, path, data, pyjson.Indent(2))
}

// Seconds to wait for the snapshot store lock
const jsonStoreLockTimeout = 10 * time.Second

// Read modify write of a JSON store under its cross process lock, unlocked when the lock is unusable
func envUpdateJSONStore(ctx context.Context, path string, update func(*pyjson.Object)) error {
	lockPath := path + ".lock"
	os.MkdirAll(filepath.Dir(lockPath), 0o755)
	unlock, err := lockStoreFile(lockPath, jsonStoreLockTimeout)
	if err != nil {
		slog.Warn("Updating snapshot store without its lock", "path", path, "error", err)
	} else {
		defer unlock()
	}
	data := envLoadJSONStore(path)
	before := data.Copy()
	update(data)
	if pyjson.Equal(data, before) {
		return nil
	}
	return envSaveJSONStore(ctx, path, data)
}

// Change key of a host file from mtime, size, inode and ctime, false when unreadable
func fileMtimeKey(hostPath string) (string, bool) {
	st, err := os.Stat(hostPath)
	if err != nil {
		return "", false
	}
	ino, ctime := statIdentity(st)
	return fmt.Sprintf("%d:%d:%d:%d", st.ModTime().UnixNano(), st.Size(), ino, ctime), true
}

// EnvUsesHostFilesystem reports whether the file tools may touch the files of an environment on the host
func EnvUsesHostFilesystem(e Environment) bool {
	if e == nil {
		return false
	}
	return e.UsesHostFilesystem()
}

// Session snapshot scripting

// Names declare -x must never carry from one session into the shared snapshot
const snapshotExcludedEnvRegex = "^declare -x (ICEBATH_SESSION_|ICEBATH_UI_SESSION_ID|ICEBATH_CRON_AUTO_DELIVER_|" +
	"ICEBATH_CRON_SESSION|ICEBATH_DELEGATED_CHILD_CONTEXT|" +
	"ICEBATH_RPC_|ICEBATH_KERNEL_DIR)"

// Valid shell variable name
var shellEnvNameRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// mktemp template suffix of the snapshot temp file
const snapTmpSuffix = ".tmp.XXXXXXXXXX"

// Shell expansion of the allocated snapshot temp path
const snapTmpVar = `"$__icebath_snap_tmp"`

// Marker wrapping the cwd a command ends in
func cwdMarkerFor(sessionID string) string { return "__ICEBATH_CWD_" + sessionID + "__" }

// Marker wrapping the PIPESTATUS of the last pipeline
func pipeMarkerFor(sessionID string) string { return "__ICEBATH_PIPE_" + sessionID + "__" }

// Shell line printing the captured pipeline statuses between the marker
func pipeMarkerPrintf(marker string) string {
	return fmt.Sprintf(`printf '\n%s%%s%s\n' "${__icebath_ps[*]-}"`, marker, marker)
}

// Shell line evaluating the command while keeping its last PIPESTATUS as side data
func evalCapturingPipestatus(escaped string) string {
	return "eval '" + escaped + "\n\n__icebath_rc=$? __icebath_ps=(\"${PIPESTATUS[@]}\"); (exit $__icebath_rc) && :'"
}

// Reports whether a command tolerates the pipestatus capture line appended after it
func canCapturePipestatus(command string) bool {
	return !strings.Contains(command, "<<") && !strings.HasSuffix(command, "\\")
}

// Shell line printing the physical cwd between the marker
func cwdMarkerPrintf(marker string) string {
	return fmt.Sprintf(`printf '\n%s%%s%s\n' "$(pwd -P)"`, marker, marker)
}

// Shell snippet dumping export -p to a path minus the per session vars and the excluded names
func exportDumpExcludingSessionVars(tmpPath string, excludedNames []string) string {
	safe := map[string]bool{}
	for _, n := range excludedNames {
		if n != "" {
			safe[n] = true
		}
	}
	names := make([]string, 0, len(safe))
	for n := range safe {
		names = append(names, n)
	}
	sort.Strings(names)
	var extra strings.Builder
	for _, n := range names {
		extra.WriteString(" " + shQuote(n))
	}
	return "{ ( unset ${!ICEBATH_SESSION_*} ${!ICEBATH_CRON_AUTO_DELIVER_*} " +
		"AI_AGENT ICEBATH_AGENT " +
		"ICEBATH_DELEGATED_CHILD_CONTEXT ICEBATH_CRON_SESSION " +
		"${!ICEBATH_RPC_*} ICEBATH_KERNEL_DIR " +
		"ICEBATH_UI_SESSION_ID" + extra.String() + " 2>/dev/null; " +
		"export -p; ) || true; } " +
		"> " + tmpPath
}

// Quoting inputs shared by the bootstrap and the per command wrapper
type snapshotScriptArgs struct {
	quotedCwd       string
	quotedSnap      string
	snapTmpTemplate string
	cwdMarker       string
}

// Login shell bootstrap capturing env, functions and aliases into the snapshot through mktemp and mv
func snapshotBootstrapScript(a snapshotScriptArgs, excludedNames []string) string {
	return "umask 077\n" +
		"__icebath_snap_tmp=$(mktemp " + a.snapTmpTemplate + ") || exit 1\n" +
		exportDumpExcludingSessionVars(snapTmpVar, excludedNames) + "\n" +
		"__icebath_fns=$(declare -F | awk '{print $3}' | grep -vE '^_[^_]') || true\n" +
		"[ -n \"$__icebath_fns\" ] && declare -f $__icebath_fns >> " + snapTmpVar + " 2>/dev/null || true\n" +
		"alias -p >> " + snapTmpVar + "\n" +
		"echo 'shopt -s expand_aliases' >> " + snapTmpVar + "\n" +
		"echo 'set +e' >> " + snapTmpVar + "\n" +
		"echo 'set +u' >> " + snapTmpVar + "\n" +
		"mv -f " + snapTmpVar + " " + a.quotedSnap + " || rm -f " + snapTmpVar + "\n" +
		"builtin cd -- " + a.quotedCwd + " 2>/dev/null || true\n" +
		cwdMarkerPrintf(a.cwdMarker) + "\n"
}

// Shell lines saving profile scoped passthrough vars before sourcing and restoring them after
func passthroughSaveRestore(names []string) ([]string, []string) {
	var save, restore []string
	for _, name := range names {
		marker := "_ICEBATH_RUNTIME_PASSTHROUGH_" + name
		present, value := marker+"_PRESENT", marker+"_VALUE"
		save = append(save, present+"=${"+name+"+x}", value+"=${"+name+"-}")
		restore = append(restore,
			`if [ "$`+present+`" = x ]; then export `+name+`="$`+value+`"; else unset `+name+`; fi`,
			"unset "+present+" "+value)
	}
	return save, restore
}

// Inputs of the per command wrapper script
type wrapScriptArgs struct {
	snapshotScriptArgs
	passthroughNames []string
	snapshotReady    bool
	pipeMarker       string
	persistEnv       bool
}

// Per command bash script that sources the snapshot, cds, runs, re dumps env and emits the markers
func wrapCommandScript(command string, a wrapScriptArgs) string {
	escaped := strings.ReplaceAll(command, "'", `'\''`)
	capture := a.pipeMarker != "" && canCapturePipestatus(command)
	save, restore := passthroughSaveRestore(a.passthroughNames)
	parts := append([]string{}, save...)
	if a.snapshotReady {
		parts = append(parts, "source "+a.quotedSnap+" >/dev/null 2>&1 || true")
	}
	parts = append(parts, restore...)
	parts = append(parts,
		`export AI_AGENT="${AI_AGENT:-icebath}" ICEBATH_AGENT="${ICEBATH_AGENT:-true}"`,
		`export GIT_PAGER="${GIT_PAGER:-cat}" PAGER="${PAGER:-cat}"`,
		"builtin cd -- "+a.quotedCwd+" || exit 126")
	if capture {
		parts = append(parts, evalCapturingPipestatus(escaped))
	} else {
		parts = append(parts, "eval '"+escaped+"'")
	}
	parts = append(parts, "__icebath_ec=$?", "umask 077")
	if a.snapshotReady && a.persistEnv {
		parts = append(parts,
			"__icebath_snap_tmp=$(mktemp "+a.snapTmpTemplate+") && "+
				"{ "+exportDumpExcludingSessionVars(snapTmpVar, a.passthroughNames)+" "+
				"&& mv -f "+snapTmpVar+" "+a.quotedSnap+"; } "+
				"2>/dev/null || rm -f "+snapTmpVar+" 2>/dev/null || true")
	}
	if capture {
		parts = append(parts, pipeMarkerPrintf(a.pipeMarker))
	}
	parts = append(parts, cwdMarkerPrintf(a.cwdMarker), "exit $__icebath_ec")
	return strings.Join(parts, "\n")
}

// Last complete marker pair in output as its payload and the output without that line
func splitCwdMarker(output, marker string) (payload string, cleaned string, found bool) {
	last := strings.LastIndex(output, marker)
	if last == -1 {
		return "", "", false
	}
	searchStart := last - 4096
	if searchStart < 0 {
		searchStart = 0
	}
	first := strings.LastIndex(output[searchStart:last], marker)
	if first == -1 {
		return "", "", false
	}
	first += searchStart
	payload = strings.TrimSpace(output[first+len(marker) : last])
	lineStart := strings.LastIndex(output[:first], "\n")
	if lineStart == -1 {
		lineStart = first
	}
	lineEnd := strings.Index(output[last+len(marker):], "\n")
	if lineEnd == -1 {
		lineEnd = len(output)
	} else {
		lineEnd = last + len(marker) + lineEnd + 1
	}
	return payload, output[:lineStart] + output[lineEnd:], true
}

// Bounded output capture

// Head and tail window of a streamed text with the full stream teed to a spill file on overflow
type boundedOutputCollector struct {
	maxChars    int
	headLimit   int
	tailLimit   int
	head        []string
	tail        []string
	headChars   int
	tailChars   int
	totalChars  int
	mu          sync.Mutex
	spillPath   string
	spillFile   *os.File
	spillChars  int
	spillCapped bool
}

// Collector retaining a 40/60 head tail window of at most maxChars
func newBoundedOutputCollector(maxChars int, spillPath string) *boundedOutputCollector {
	if maxChars < 1 {
		maxChars = 1
	}
	head := int(float64(maxChars) * 0.4)
	return &boundedOutputCollector{maxChars: maxChars, headLimit: head, tailLimit: maxChars - head, spillPath: spillPath}
}

// Tees text to the spill file, opened lazily on the first overflow
func (c *boundedOutputCollector) maybeSpill(text string) {
	if c.spillPath == "" || c.spillCapped {
		return
	}
	if c.spillFile == nil {
		if err := EnsureSpillDir(filepath.Dir(c.spillPath), true); err != nil {
			c.spillCapped = true
			return
		}
		f, err := OpenExclusive(c.spillPath, true, false)
		if err != nil {
			c.spillCapped = true
			return
		}
		c.spillFile = f
		backlog := strings.Join(c.head, "") + strings.Join(c.tail, "")
		if _, err := f.WriteString(backlog); err != nil {
			c.spillCapped = true
			return
		}
		c.spillChars = len(backlog)
	}
	budget := collectorSpillCap - c.spillChars
	var err error
	if budget <= 0 || len(text) > budget {
		if budget < 0 {
			budget = 0
		}
		_, err = c.spillFile.WriteString(text[:budget])
		if err == nil {
			_, err = c.spillFile.WriteString("\n... [spill capped at 5,000,000 chars] ...\n")
		}
		c.spillCapped = true
	} else {
		_, err = c.spillFile.WriteString(text)
	}
	if err != nil {
		c.spillCapped = true
		return
	}
	c.spillChars += len(text)
}

// Closes the spill file and returns its path when it was used
func (c *boundedOutputCollector) closeSpill() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.spillFile == nil {
		return ""
	}
	c.spillFile.Close()
	c.spillFile = nil
	return c.spillPath
}

// Characters currently retained in the window
func (c *boundedOutputCollector) bufferedChars() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.headChars + c.tailChars
}

// Characters appended over the collector lifetime
func (c *boundedOutputCollector) total() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.totalChars
}

// Appends text to the window evicting from the front of the tail
func (c *boundedOutputCollector) append(text string) {
	if text == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	textLen := len(text)
	if c.spillPath != "" && (c.spillFile != nil || c.totalChars+textLen > c.maxChars) {
		c.maybeSpill(text)
	}
	c.totalChars += textLen
	start := 0
	if c.headChars < c.headLimit {
		take := c.headLimit - c.headChars
		if take > textLen {
			take = textLen
		}
		if take > 0 {
			c.head = append(c.head, text[:take])
			c.headChars += take
			start = take
		}
	}
	remaining := textLen - start
	if remaining <= 0 || c.tailLimit <= 0 {
		return
	}
	if remaining >= c.tailLimit {
		c.tail = []string{text[textLen-c.tailLimit:]}
		c.tailChars = c.tailLimit
		return
	}
	chunk := text[start:]
	c.tail = append(c.tail, chunk)
	c.tailChars += len(chunk)
	for c.tailChars-c.tailLimit > 0 {
		excess := c.tailChars - c.tailLimit
		first := c.tail[0]
		if len(first) <= excess {
			c.tail = c.tail[1:]
			c.tailChars -= len(first)
		} else {
			c.tail[0] = first[excess:]
			c.tailChars -= excess
		}
	}
}

// Renders within maxChars while keeping a required status suffix intact
func (c *boundedOutputCollector) render(suffix string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(suffix) >= c.maxChars {
		return suffix[len(suffix)-c.maxChars:]
	}
	head := strings.Join(c.head, "")
	tail := strings.Join(c.tail, "")
	available := c.maxChars - len(suffix)
	if c.totalChars <= available {
		return head + tail + suffix
	}
	notice := ""
	for range 4 {
		budget := available - len(notice)
		if budget < 0 {
			budget = 0
		}
		omitted := c.totalChars - budget
		if omitted < 0 {
			omitted = 0
		}
		updated := TruncationNotice(omitted, c.totalChars, "", "")
		if updated == notice {
			break
		}
		notice = updated
	}
	contentBudget := available - len(notice)
	if contentBudget < 0 {
		contentBudget = 0
	}
	headChars, tailChars := HeadTailSplit(contentBudget)
	if headChars > len(head) {
		headChars = len(head)
	}
	renderedTail := ""
	if tailChars > 0 {
		if tailChars > len(tail) {
			tailChars = len(tail)
		}
		renderedTail = tail[len(tail)-tailChars:]
	}
	if len(notice) > available {
		notice = notice[:available]
	}
	return head[:headChars] + notice + renderedTail + suffix
}

// Prunes spill files older than a week at most once per process start
var spillPruneOnce sync.Once

// Collector for one wait, bounded at tool_output.max_bytes with a spill file under cache/terminal-output
func newOutputCollector(env *Env, proc ProcessHandle, boundedCapture bool) *boundedOutputCollector {
	if !boundedCapture {
		return newBoundedOutputCollector(unboundedCaptureChars, "")
	}
	captureLimit := ToolOutputLimits(env).MaxBytes
	if captureLimit <= 0 {
		captureLimit = 50_000
	}
	spillDir := filepath.Join(envHome(env), "cache", "terminal-output")
	ident := 0
	if proc != nil {
		ident = proc.Pid() & 0xffff
	}
	spillPath := filepath.Join(spillDir, fmt.Sprintf("out-%d-%d-%x.log", time.Now().Unix(), os.Getpid(), ident))
	spillPruneOnce.Do(func() {
		cutoff := time.Now().Add(-spillMaxAge)
		olds, _ := filepath.Glob(filepath.Join(spillDir, "out-*.log"))
		for _, old := range olds {
			if st, err := os.Stat(old); err == nil && st.ModTime().Before(cutoff) {
				os.Remove(old)
			}
		}
	})
	return newBoundedOutputCollector(captureLimit, spillPath)
}

// Wait result with spill metadata attached when overflow occurred
func finalizeWaitResult(c *boundedOutputCollector, rendered string, returncode int) *ExecResult {
	res := &ExecResult{Output: rendered, ReturnCode: returncode}
	if spill := c.closeSpill(); spill != "" {
		res.OutputTotalChars = c.total()
		res.FullOutputPath = spill
	}
	return res
}

// Process handles

// Handle that reports the first stdin write failure once the writer finished
type stdinJoiner interface {
	JoinStdin(timeout time.Duration) string
}

// ExecProcess is the subprocess handle every backend spawning a host command returns
type ExecProcess struct {
	Cmd       *exec.Cmd
	stdout    *os.File
	waitDone  chan struct{}
	code      int
	stdinDone chan struct{}
	stdinErr  string
	// Owner death supervisor entry of the process group, zero once released
	ownedPgid int
	// Process group id recorded at spawn
	pgid int
	// Start time fingerprint of the group leader, empty without a baseline
	pgidStart string
}

// Writes data encoded with surrogateescape to the child stdin on a goroutine, recording an encode failure, then closes it
func pipeStdin(p *ExecProcess, w *os.File, data string) {
	p.stdinDone = make(chan struct{})
	go func() {
		defer close(p.stdinDone)
		if w == nil {
			p.stdinErr = "process stdin unavailable"
			return
		}
		defer w.Close()
		raw, err := pystr.SurrogateEscapeEncode(data)
		if err != nil {
			p.stdinErr = err.Error()
			return
		}
		w.Write(raw)
	}()
}

// StartExecProcess spawns a configured command with merged stdout and stderr on a pipe and optional stdin data
func StartExecProcess(cmd *exec.Cmd, stdinData *string) (*ExecProcess, error) {
	r, w, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	cmd.Stdout = w
	cmd.Stderr = w
	var stdinR, stdinW *os.File
	if stdinData != nil {
		stdinR, stdinW, err = os.Pipe()
		if err != nil {
			r.Close()
			w.Close()
			return nil, err
		}
		cmd.Stdin = stdinR
	}
	hideConsoleWindow(cmd)
	if err := cmd.Start(); err != nil {
		r.Close()
		w.Close()
		if stdinR != nil {
			stdinR.Close()
			stdinW.Close()
		}
		return nil, err
	}
	w.Close()
	p := &ExecProcess{Cmd: cmd, stdout: r, waitDone: make(chan struct{})}
	if stdinR != nil {
		stdinR.Close()
		pipeStdin(p, stdinW, *stdinData)
	}
	go func() {
		defer close(p.waitDone)
		err := cmd.Wait()
		p.code = exitCodeOf(cmd, err)
	}()
	return p, nil
}

// Exit code of a finished command, negative signal number when it was killed
func exitCodeOf(cmd *exec.Cmd, err error) int {
	if cmd.ProcessState != nil {
		if code := cmd.ProcessState.ExitCode(); code >= 0 {
			return code
		}
		if sig := signalOf(cmd.ProcessState); sig > 0 {
			return -sig
		}
	}
	if err != nil {
		return 1
	}
	return 0
}

// popenBash spawns argv with the shared stdout, stderr and stdin wiring
func popenBash(argv []string, stdinData *string, configure func(*exec.Cmd)) (*ExecProcess, error) {
	cmd := exec.Command(argv[0], argv[1:]...)
	if configure != nil {
		configure(cmd)
	}
	return StartExecProcess(cmd, stdinData)
}

// Poll reports the exit code once the command finished
func (p *ExecProcess) Poll() (int, bool) {
	select {
	case <-p.waitDone:
		return p.code, true
	default:
		return 0, false
	}
}

// Kill sends SIGKILL to the process itself
func (p *ExecProcess) Kill() error {
	if p.Cmd.Process == nil {
		return nil
	}
	return p.Cmd.Process.Kill()
}

// Wait blocks until the command finishes or the timeout passes
func (p *ExecProcess) Wait(timeoutSeconds float64) (int, error) {
	if timeoutSeconds <= 0 {
		<-p.waitDone
		return p.code, nil
	}
	select {
	case <-p.waitDone:
		return p.code, nil
	case <-time.After(time.Duration(timeoutSeconds * float64(time.Second))):
		return 0, context.DeadlineExceeded
	}
}

// Stdout returns the read end of the merged output pipe
func (p *ExecProcess) Stdout() *os.File { return p.stdout }

// Pid returns the child process id
func (p *ExecProcess) Pid() int {
	if p.Cmd.Process == nil {
		return 0
	}
	return p.Cmd.Process.Pid
}

// JoinStdin waits for the stdin writer and returns its recorded error
func (p *ExecProcess) JoinStdin(timeout time.Duration) string {
	if p.stdinDone == nil {
		return ""
	}
	select {
	case <-p.stdinDone:
	case <-time.After(timeout):
	}
	return p.stdinErr
}

// ThreadedProcessHandle adapts an SDK call with no real subprocess to the ProcessHandle contract
type ThreadedProcessHandle struct {
	cancelFn   func()
	done       chan struct{}
	returncode int
	stdout     *os.File
	pid        int
}

// Synthetic pids handed to SDK backed handles
var threadedHandleSeq atomic.Int64

// NewThreadedProcessHandle runs execFn on a goroutine and serves its output through a pipe
func NewThreadedProcessHandle(execFn func() (string, int), cancelFn func()) (*ThreadedProcessHandle, error) {
	r, w, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	h := &ThreadedProcessHandle{cancelFn: cancelFn, done: make(chan struct{}), stdout: r, pid: int(threadedHandleSeq.Add(1))}
	go func() {
		defer close(h.done)
		defer w.Close()
		output, code, failed := runThreadedExec(execFn)
		if failed {
			h.returncode = 1
			return
		}
		h.returncode = code
		w.WriteString(strings.TrimPrefix(output, "\xEF\xBB\xBF"))
	}()
	return h, nil
}

// Runs the exec function reporting a panic as a failure
func runThreadedExec(execFn func() (string, int)) (output string, code int, failed bool) {
	defer func() {
		if r := recover(); r != nil {
			failed = true
		}
	}()
	output, code = execFn()
	return output, code, false
}

// Stdout returns the read end of the output pipe
func (h *ThreadedProcessHandle) Stdout() *os.File { return h.stdout }

// Pid returns a synthetic identifier unique per handle
func (h *ThreadedProcessHandle) Pid() int { return h.pid }

// Poll reports the exit code once the exec function returned
func (h *ThreadedProcessHandle) Poll() (int, bool) {
	select {
	case <-h.done:
		return h.returncode, true
	default:
		return 0, false
	}
}

// Kill invokes the backend cancellation
func (h *ThreadedProcessHandle) Kill() error {
	if h.cancelFn != nil {
		func() {
			defer func() { recover() }()
			h.cancelFn()
		}()
	}
	return nil
}

// Wait blocks until the exec function returned or the timeout passed
func (h *ThreadedProcessHandle) Wait(timeoutSeconds float64) (int, error) {
	if timeoutSeconds <= 0 {
		<-h.done
		return h.returncode, nil
	}
	select {
	case <-h.done:
		return h.returncode, nil
	case <-time.After(time.Duration(timeoutSeconds * float64(time.Second))):
		return 0, context.DeadlineExceeded
	}
}

// Incremental UTF-8 decoder replacing invalid bytes with U+FFFD across chunk boundaries
type utf8Decoder struct {
	pending []byte
}

// Decodes a chunk keeping an incomplete trailing sequence for the next call
func (d *utf8Decoder) decode(chunk []byte) string {
	data := chunk
	if len(d.pending) > 0 {
		data = append(d.pending, chunk...)
		d.pending = nil
	}
	var b strings.Builder
	b.Grow(len(data))
	i := 0
	for i < len(data) {
		r, size := utf8.DecodeRune(data[i:])
		if r == utf8.RuneError && size == 1 {
			if !utf8.FullRune(data[i:]) {
				d.pending = append([]byte{}, data[i:]...)
				return b.String()
			}
			b.WriteRune(utf8.RuneError)
			i++
			continue
		}
		b.Write(data[i : i+size])
		i += size
	}
	return b.String()
}

// Flushes a dangling incomplete sequence as replacement characters
func (d *utf8Decoder) finish() string {
	if len(d.pending) == 0 {
		return ""
	}
	n := len(d.pending)
	d.pending = nil
	return strings.Repeat(string(utf8.RuneError), n)
}

// Goroutine draining a process stdout into a collector
type drainThread struct {
	done chan struct{}
}

// Waits for the drain to finish within the timeout
func (d *drainThread) join(timeout time.Duration) {
	select {
	case <-d.done:
	case <-time.After(timeout):
	}
}

// Starts the drain goroutine, stop ends it early for a handoff
func startDrainThread(proc ProcessHandle, output *boundedOutputCollector, stop *atomic.Bool) *drainThread {
	d := &drainThread{done: make(chan struct{})}
	go func() {
		defer close(d.done)
		drainStdout(proc, output, stop)
	}()
	return d
}

// Drains stdout until EOF, a stop request, or about 300ms after the process exited with the pipe idle
func drainStdout(proc ProcessHandle, output *boundedOutputCollector, stop *atomic.Bool) {
	stream := proc.Stdout()
	if stream == nil {
		return
	}
	dec := &utf8Decoder{}
	defer func() {
		if tail := dec.finish(); tail != "" {
			output.append(tail)
		}
	}()
	buf := make([]byte, 4096)
	idleAfterExit := 0
	for {
		if stop != nil && stop.Load() {
			return
		}
		n, ready, err := readChunkTimeout(stream, buf, 100*time.Millisecond)
		if err != nil {
			return
		}
		if ready {
			if n == 0 {
				return
			}
			output.append(dec.decode(buf[:n]))
			idleAfterExit = 0
			continue
		}
		if _, exited := proc.Poll(); exited {
			idleAfterExit++
			if idleAfterExit >= 3 {
				return
			}
		}
	}
}

// Opt in interrupt and poll tracing of one wait
type waitTrace struct {
	enabled       bool
	iterations    int
	timeout       int
	pid           int
	start         time.Time
	lastHeartbeat time.Time
	cbWasNone     bool
	ctx           context.Context
	log           *slog.Logger
}

// Tracer that stays silent unless ICEBATH_DEBUG_INTERRUPT is set
func newWaitTrace(ctx context.Context, env *Env, proc ProcessHandle, timeout int) *waitTrace {
	t := &waitTrace{enabled: debugInterrupt, timeout: timeout, ctx: ctx, log: toolsLogger(env)}
	if !t.enabled {
		return t
	}
	t.pid = proc.Pid()
	t.start = time.Now()
	t.lastHeartbeat = t.start
	t.cbWasNone = ActivityCallbackOf(ctx) == nil
	return t
}

// Logs one trace event with the common prefix
func (t *waitTrace) emit(event, detail string) {
	t.log.Info(fmt.Sprintf("[interrupt-debug] _wait_for_process %s worker=%d pid=%d %s", event, WorkerOf(t.ctx), t.pid, detail))
}

// Seconds since the wait started
func (t *waitTrace) elapsed() float64 { return time.Since(t.start).Seconds() }

// Callback presence spelled the way the trace reports it
func cbLabel(none bool) string {
	if none {
		return "MISSING"
	}
	return "set"
}

func (t *waitTrace) enter() {
	if t.enabled {
		t.emit("ENTER", fmt.Sprintf("timeout=%ds activity_cb=%s initial_interrupt=%v", t.timeout, cbLabel(t.cbWasNone), IsInterrupted(t.ctx)))
	}
}

func (t *waitTrace) interrupted() {
	if t.enabled {
		t.emit("INTERRUPT DETECTED", fmt.Sprintf("iter=%d elapsed=%.1fs — killing process group", t.iterations, t.elapsed()))
	}
}

func (t *waitTrace) timedOut() {
	if t.enabled {
		t.emit("TIMEOUT", fmt.Sprintf("iter=%d timeout=%ds", t.iterations, t.timeout))
	}
}

// Proves the loop is alive every 30s and reports a lost activity callback
func (t *waitTrace) heartbeat() {
	if !t.enabled || time.Since(t.lastHeartbeat) < 30*time.Second {
		return
	}
	cbNowNone := ActivityCallbackOf(t.ctx) == nil
	lost := ""
	if cbNowNone && !t.cbWasNone {
		lost = " (LOST during run)"
	}
	t.emit("HEARTBEAT", fmt.Sprintf("iter=%d elapsed=%.0fs interrupt=%v activity_cb=%s%s", t.iterations, t.elapsed(), IsInterrupted(t.ctx), cbLabel(cbNowNone), lost))
	t.lastHeartbeat = time.Now()
	t.cbWasNone = cbNowNone
}

func (t *waitTrace) naturalExit(returncode int) {
	if t.enabled {
		t.emit("EXIT (natural)", fmt.Sprintf("iter=%d elapsed=%.1fs returncode=%d", t.iterations, t.elapsed(), returncode))
	}
}

// Base environment

// Overridable behaviour of a concrete backend built on baseEnv
type envSelf interface {
	Environment
	runBash(ctx context.Context, cmd string, login bool, timeout int, stdinData *string) (ProcessHandle, error)
	quoteCwdForCd(cwd string) string
	quoteShellPath(path string) string
	killProcess(p ProcessHandle)
	forceKillProcess(p ProcessHandle)
	updateCwd(result *ExecResult)
	extractCwdFromOutput(result *ExecResult)
	beforeExecute(ctx context.Context) error
	additionalProfileScopedPassthroughNames() []string
	waitForProcess(ctx context.Context, p ProcessHandle, o waitOptions) *ExecResult
}

// Inputs of one wait on a spawned process
type waitOptions struct {
	timeout        int
	boundedCapture bool
	output         *boundedOutputCollector
	yieldHandler   func(ProcessHandle, string) *ExecResult
	watchWorker    WorkerID
}

// Shared execution flow of every backend with snapshot sourcing, cwd tracking, interrupts and timeouts
type baseEnv struct {
	self                           envSelf
	env                            *Env
	mu                             sync.Mutex
	cwd                            string
	timeout                        int
	childEnv                       map[string]string
	persistEnv                     bool
	sessionID                      string
	snapshotPath                   string
	cwdFile                        string
	cwdMarker                      string
	pipeMarker                     string
	snapshotReady                  bool
	preferNonlogin                 bool
	snapshotPassthroughNames       map[string]bool
	stdinMode                      string
	isLocal                        bool
	usesHostFilesystem             bool
	snapshotTimeout                int
	passwordlessSudoProbeSupported bool
	profileScopedPassthrough       bool
	recreatedNoticePending         bool
	persistent                     bool
	sessionScoped                  bool
	hostCwd                        string
	hostCwdMount                   string
	sandboxHome                    string
}

// Fills the shared fields and allocates the per session snapshot paths
func initBaseEnv(b *baseEnv, self envSelf, env *Env, cwd string, timeout int, childEnv map[string]string, persistEnv bool) {
	b.self = self
	b.env = env
	b.cwd = cwd
	b.timeout = timeout
	if childEnv == nil {
		childEnv = map[string]string{}
	}
	b.childEnv = childEnv
	b.persistEnv = persistEnv
	b.sessionID = strings.ReplaceAll(uuid.NewString(), "-", "")[:12]
	tempDir := strings.TrimRight(self.TempDir(), "/")
	if tempDir == "" {
		tempDir = "/"
	}
	b.snapshotPath = tempDir + "/icebath-snap-" + b.sessionID + ".sh"
	b.cwdFile = tempDir + "/icebath-cwd-" + b.sessionID + ".txt"
	b.cwdMarker = cwdMarkerFor(b.sessionID)
	b.pipeMarker = pipeMarkerFor(b.sessionID)
	b.snapshotPassthroughNames = map[string]bool{}
	if b.stdinMode == "" {
		b.stdinMode = "pipe"
	}
	if b.snapshotTimeout == 0 {
		b.snapshotTimeout = 30
	}
	b.persistent = true
}

// Env returns the tool environment the backend serves
func (b *baseEnv) Env() *Env { return b.env }

// SessionID returns the twelve character session identity of the snapshot
func (b *baseEnv) SessionID() string { return b.sessionID }

// SnapshotPath returns the sandbox side path of the session snapshot
func (b *baseEnv) SnapshotPath() string { return b.snapshotPath }

// CwdMarker returns the marker wrapping the cwd a command ends in
func (b *baseEnv) CwdMarker() string { return b.cwdMarker }

// PipeMarker returns the marker wrapping the pipeline statuses
func (b *baseEnv) PipeMarker() string { return b.pipeMarker }

// SnapshotReady reports whether commands source the snapshot instead of running under bash -l
func (b *baseEnv) SnapshotReady() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.snapshotReady
}

// PreferNonlogin reports whether login bash proved unusable
func (b *baseEnv) PreferNonlogin() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.preferNonlogin
}

// IsLocal reports whether commands run on the same host as this process
func (b *baseEnv) IsLocal() bool { return b.isLocal }

// TempDir is the sandbox side temp directory used for session artifacts
func (b *baseEnv) TempDir() string { return "/tmp" }

// Cwd returns the directory the next command starts in
func (b *baseEnv) Cwd() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.cwd
}

// SetCwd replaces the directory the next command starts in
func (b *baseEnv) SetCwd(cwd string) {
	b.mu.Lock()
	b.cwd = cwd
	b.mu.Unlock()
}

// Timeout returns the default per command timeout in seconds
func (b *baseEnv) Timeout() int { return b.timeout }

// ChildEnv returns the extra variables the backend injects into every command
func (b *baseEnv) ChildEnv() map[string]string { return b.childEnv }

// HostCwd returns the host directory the sandbox workspace mirrors
func (b *baseEnv) HostCwd() string { return b.hostCwd }

// HostCwdMount returns the sandbox path the host cwd is mounted at
func (b *baseEnv) HostCwdMount() string { return b.hostCwdMount }

// SandboxHome returns the home directory inside the sandbox when declared
func (b *baseEnv) SandboxHome() string { return b.sandboxHome }

// IsPersistent reports whether the backend keeps its state across sessions
func (b *baseEnv) IsPersistent() bool { return b.persistent }

// IsSessionScoped reports whether the backend identity is per session
func (b *baseEnv) IsSessionScoped() bool { return b.sessionScoped }

// UsesHostFilesystem reports whether the file tools may read and write its files on the host
func (b *baseEnv) UsesHostFilesystem() bool { return b.usesHostFilesystem }

// MarkRecreated flags that the live sandbox was replaced while serving the current command
func (b *baseEnv) MarkRecreated() {
	b.mu.Lock()
	b.recreatedNoticePending = true
	b.mu.Unlock()
}

// Default run hook that every backend must override
func (b *baseEnv) runBash(ctx context.Context, cmd string, login bool, timeout int, stdinData *string) (ProcessHandle, error) {
	return nil, fmt.Errorf("%s must implement runBash", b.self.EnvType())
}

// Default hook before each command, remote backends trigger their file sync here
func (b *baseEnv) beforeExecute(ctx context.Context) error { return nil }

// Default backend specific names that must not persist in snapshots
func (b *baseEnv) additionalProfileScopedPassthroughNames() []string { return nil }

// Quotes a cd target while preserving tilde expansion through HOME
func quoteCwdForCd(cwd string) string {
	switch {
	case cwd == "~":
		return cwd
	case cwd == "~/":
		return "$HOME"
	case strings.HasPrefix(cwd, "~/"):
		return "$HOME/" + shQuote(cwd[2:])
	}
	return shQuote(cwd)
}

// Default cd quoting of a POSIX backend
func (b *baseEnv) quoteCwdForCd(cwd string) string { return quoteCwdForCd(cwd) }

// Default shell path quoting of a POSIX backend
func (b *baseEnv) quoteShellPath(path string) string { return shQuote(path) }

// Terminates a process, backends override for a process group kill
func (b *baseEnv) killProcess(p ProcessHandle) {
	p.Kill()
}

// Kills without waiting for a host that hard exits next
func (b *baseEnv) forceKillProcess(p ProcessHandle) {
	b.self.killProcess(p)
}

// Extracts the cwd from the command output, backends override for a file based read
func (b *baseEnv) updateCwd(result *ExecResult) {
	b.self.extractCwdFromOutput(result)
}

// Parses the pipeline and cwd markers out of the output and updates the cwd when this command emitted one
func (b *baseEnv) extractCwdFromOutput(result *ExecResult) {
	if b.pipeMarker != "" {
		if statuses, cleaned, ok := splitCwdMarker(result.Output, b.pipeMarker); ok {
			result.Output = cleaned
			var codes []int
			valid := true
			for _, s := range strings.Fields(statuses) {
				n, err := strconv.Atoi(s)
				if err != nil {
					valid = false
					break
				}
				codes = append(codes, n)
			}
			if valid {
				result.PipelineStatus = codes
			}
		}
	}
	cwdPath, cleaned, ok := splitCwdMarker(result.Output, b.cwdMarker)
	if !ok {
		return
	}
	if cwdPath != "" {
		b.SetCwd(cwdPath)
		result.CwdObserved = true
		result.Cwd = cwdPath
	}
	result.Output = cleaned
}

// Profile scoped names that must not persist in the snapshot, monotonic for the environment lifetime
func (b *baseEnv) snapshotExcludedPassthroughNames() []string {
	if !b.profileScopedPassthrough {
		return nil
	}
	b.mu.Lock()
	if isMultiplexActive(b.env) {
		names := []string{}
		for n := range AllPassthrough(b.env) {
			names = append(names, n)
		}
		names = append(names, b.self.additionalProfileScopedPassthroughNames()...)
		for _, n := range names {
			if shellEnvNameRe.MatchString(n) {
				b.snapshotPassthroughNames[n] = true
			}
		}
	}
	out := make([]string, 0, len(b.snapshotPassthroughNames))
	for n := range b.snapshotPassthroughNames {
		out = append(out, n)
	}
	b.mu.Unlock()
	sort.Strings(out)
	return out
}

// Quoting inputs shared by the bootstrap and per command wrapper
func (b *baseEnv) snapshotScriptArgs(cwd string) snapshotScriptArgs {
	return snapshotScriptArgs{
		quotedCwd:       b.self.quoteCwdForCd(cwd),
		quotedSnap:      b.self.quoteShellPath(b.snapshotPath),
		snapTmpTemplate: b.self.quoteShellPath(b.snapshotPath + snapTmpSuffix),
		cwdMarker:       b.cwdMarker,
	}
}

// Captures the login shell environment into the snapshot, falling back to bash -l or bash -c per command
func (b *baseEnv) initSession(ctx context.Context) {
	bootstrap := snapshotBootstrapScript(b.snapshotScriptArgs(b.Cwd()), b.snapshotExcludedPassthroughNames())
	log := toolsLogger(b.env)
	err := func() error {
		p, err := b.self.runBash(ctx, bootstrap, true, b.snapshotTimeout, nil)
		if err != nil {
			return err
		}
		result := b.self.waitForProcess(ctx, p, waitOptions{timeout: b.snapshotTimeout})
		if result.ReturnCode != 0 {
			return fmt.Errorf("snapshot bootstrap failed with exit code %d", result.ReturnCode)
		}
		b.mu.Lock()
		b.snapshotReady = true
		b.mu.Unlock()
		b.self.updateCwd(result)
		return nil
	}()
	if err == nil {
		log.Info("Session snapshot created", "session", b.sessionID, "cwd", b.Cwd())
		return
	}
	prefer, detail := b.probeNonloginFallback(ctx, err.Error())
	b.mu.Lock()
	b.snapshotReady = false
	b.preferNonlogin = prefer
	b.mu.Unlock()
	if prefer {
		log.Warn(fmt.Sprintf("init_session failed (session=%s): %s — login bash unusable; falling back to non-login bash -c", b.sessionID, err))
	} else {
		log.Warn(fmt.Sprintf("init_session failed (session=%s): %s — falling back to bash -l per command", b.sessionID, detail))
	}
}

// Runs true under non login bash and reports whether that is the only usable path
func (b *baseEnv) probeNonloginFallback(ctx context.Context, detail string) (bool, string) {
	probeTimeout := b.snapshotTimeout
	if probeTimeout > 15 {
		probeTimeout = 15
	}
	p, err := b.self.runBash(ctx, "true", false, probeTimeout, nil)
	if err != nil {
		return false, detail + "; non-login probe: " + err.Error()
	}
	result := b.self.waitForProcess(ctx, p, waitOptions{timeout: probeTimeout})
	prefer := result.ReturnCode == 0
	if !prefer {
		if text := strings.TrimSpace(result.Output); text != "" {
			detail = text
		}
	}
	return prefer, detail
}

// Full bash script that sources the snapshot, cds, runs, re dumps env and emits the cwd markers
func (b *baseEnv) wrapCommand(command, cwd string) string {
	b.mu.Lock()
	ready, persist := b.snapshotReady, b.persistEnv
	b.mu.Unlock()
	return wrapCommandScript(command, wrapScriptArgs{
		snapshotScriptArgs: b.snapshotScriptArgs(cwd),
		passthroughNames:   b.snapshotExcludedPassthroughNames(),
		snapshotReady:      ready,
		pipeMarker:         b.pipeMarker,
		persistEnv:         persist,
	})
}

// Redirects stdin data to the whole command with a heredoc minus the newline the heredoc adds
func embedStdinHeredoc(command, stdinData string) string {
	delimiter := "ICEBATH_STDIN_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:12]
	return "{\n" + command + "\n} << '" + delimiter + "' < <(IFS= read -r -d '' s || :; printf '%s' \"${s%?}\")\n" +
		stdinData + "\n" + delimiter
}

// Unique sandbox path for staging a payload mode stdin file
func (b *baseEnv) stagedStdinPath() string {
	tempDir := strings.TrimRight(b.self.TempDir(), "/")
	if tempDir == "" {
		tempDir = "/"
	}
	return tempDir + "/.icebath-stdin-" + strings.ReplaceAll(uuid.NewString(), "-", "")
}

// Prefixes the command so the shell reads stdin from the staged file and unlinks it first
func redirectStdinFromFile(command, path string) string {
	quoted := shQuote(path)
	return "exec 0< " + quoted + " || exit $?\nrm -f -- " + quoted + " || exit $?\n" + command
}

// Polls the process with interrupt, yield, timeout and activity handling while draining stdout
func (b *baseEnv) waitForProcess(ctx context.Context, p ProcessHandle, o waitOptions) *ExecResult {
	if ctx == nil {
		ctx = context.Background()
	}
	timeout := o.timeout
	if timeout <= 0 {
		timeout = 120
	}
	output := o.output
	if output == nil {
		output = newOutputCollector(b.env, p, o.boundedCapture)
	}
	var stop *atomic.Bool
	if o.yieldHandler != nil {
		stop = &atomic.Bool{}
	}
	drain := startDrainThread(p, output, stop)
	start := time.Now()
	deadline := start.Add(time.Duration(timeout) * time.Second)
	activity := newActivityState()
	trace := newWaitTrace(ctx, b.env, p, timeout)
	trace.enter()
	watch := o.watchWorker
	if watch == 0 {
		watch = WorkerOf(ctx)
	}
	killAndJoin := func() {
		b.self.killProcess(p)
		drain.join(2 * time.Second)
	}
	pollSleep := 5 * time.Millisecond
	for {
		code, exited := p.Poll()
		if exited {
			drain.join(2 * time.Second)
			if f := p.Stdout(); f != nil {
				f.Close()
			}
			trace.naturalExit(code)
			stdinErr := ""
			if j, ok := p.(stdinJoiner); ok {
				stdinErr = j.JoinStdin(5 * time.Second)
			}
			rendered := output.render("")
			res := finalizeWaitResult(output, rendered, code)
			if stdinErr != "" {
				res.StdinError = stdinErr
				res.Output = rendered + "\n[stdin write failed: " + stdinErr + "]"
			}
			return res
		}
		trace.iterations++
		if ctx.Err() != nil || IsInterrupted(ctx) || IsWorkerInterrupted(watch) {
			trace.interrupted()
			killAndJoin()
			res := finalizeWaitResult(output, output.render("\n[Command interrupted]"), 130)
			res.Interrupted = true
			return res
		}
		if o.yieldHandler != nil && ConsumeYield(watch) {
			stop.Store(true)
			drain.join(time.Second)
			handed := b.callYieldHandler(o.yieldHandler, p, output.render(""))
			if handed != nil {
				output.closeSpill()
				return handed
			}
			stop.Store(false)
			drain = startDrainThread(p, output, stop)
		}
		if time.Now().After(deadline) {
			trace.timedOut()
			killAndJoin()
			rendered := output.render(fmt.Sprintf("\n[Command timed out after %ds]", timeout))
			if output.total() == 0 {
				rendered = strings.TrimLeft(rendered, " \t\n\r")
			}
			res := finalizeWaitResult(output, rendered, 124)
			res.TimedOut = true
			return res
		}
		touchActivityIfDue(ctx, b.env, activity, "terminal command running")
		trace.heartbeat()
		time.Sleep(pollSleep)
		if pollSleep < 200*time.Millisecond {
			pollSleep = time.Duration(float64(pollSleep) * 1.5)
			if pollSleep > 200*time.Millisecond {
				pollSleep = 200 * time.Millisecond
			}
		}
	}
}

// Runs the yield handler, a panic logs and keeps waiting
func (b *baseEnv) callYieldHandler(h func(ProcessHandle, string) *ExecResult, p ProcessHandle, soFar string) (handed *ExecResult) {
	defer func() {
		if r := recover(); r != nil {
			toolsLogger(b.env).Warn("yield-to-background handoff failed; continuing to wait", "panic", r)
			handed = nil
		}
	}()
	return h(p, soFar)
}

// Execute runs a command through the snapshot wrapper with the wall clock backstop and cwd tracking
func (b *baseEnv) Execute(ctx context.Context, command string, opts ExecOptions) (*ExecResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := b.self.beforeExecute(ctx); err != nil {
		return nil, err
	}
	execCommand, sudoStdin := b.prepareCommand(ctx, command)
	if !opts.NoCompoundRewrite {
		execCommand = RewriteCompoundBackground(execCommand)
	}
	effectiveTimeout := opts.Timeout
	if effectiveTimeout <= 0 {
		effectiveTimeout = b.timeout
	}
	effectiveCwd := opts.Cwd
	if effectiveCwd == "" {
		effectiveCwd = b.Cwd()
	}
	var effectiveStdin *string
	if sudoStdin != nil {
		merged := *sudoStdin
		if opts.Stdin != nil {
			merged += *opts.Stdin
		}
		effectiveStdin = &merged
	} else {
		effectiveStdin = opts.Stdin
	}
	if effectiveStdin != nil && *effectiveStdin != "" && b.stdinMode == "heredoc" {
		execCommand = embedStdinHeredoc(execCommand, *effectiveStdin)
		effectiveStdin = nil
	}
	wrapped := b.wrapCommand(execCommand, effectiveCwd)
	b.mu.Lock()
	login := !b.snapshotReady && !b.preferNonlogin
	b.mu.Unlock()
	parentWorker := WorkerOf(ctx)

	var procMu sync.Mutex
	var spawned ProcessHandle
	var collector *boundedOutputCollector
	spawnAndWait := func() (*ExecResult, error) {
		if !enterForegroundSpawn() {
			return &ExecResult{Output: "[host is exiting: command not started]", ReturnCode: 130}, nil
		}
		p, err := b.self.runBash(ctx, wrapped, login, effectiveTimeout, effectiveStdin)
		fenced := leaveForegroundSpawn(b, p)
		if err != nil {
			return nil, err
		}
		procMu.Lock()
		spawned = p
		procMu.Unlock()
		if fenced {
			b.self.forceKillProcess(p)
		}
		output := newOutputCollector(b.env, p, opts.BoundedCapture)
		procMu.Lock()
		collector = output
		procMu.Unlock()
		defer forgetForegroundSpawn(p)
		return b.self.waitForProcess(ctx, p, waitOptions{
			timeout:        effectiveTimeout,
			boundedCapture: opts.BoundedCapture,
			output:         output,
			yieldHandler:   opts.YieldHandler,
			watchWorker:    parentWorker,
		}), nil
	}
	onTimeout := func() {
		procMu.Lock()
		p := spawned
		procMu.Unlock()
		if p != nil {
			b.killSpawnedTree(p)
		}
	}
	type outcome struct {
		res *ExecResult
		err error
	}
	done := make(chan outcome, 1)
	go func() {
		res, err := spawnAndWait()
		done <- outcome{res, err}
	}()
	bound := time.Duration(effectiveTimeout)*time.Second + executeWaitBoundGrace
	var result *ExecResult
	select {
	case out := <-done:
		if out.err != nil {
			return nil, out.err
		}
		result = out.res
	case <-time.After(bound):
		onTimeout()
		suffix := fmt.Sprintf("\n[Command timed out after %ds]", effectiveTimeout)
		procMu.Lock()
		c := collector
		procMu.Unlock()
		if c != nil {
			result = finalizeWaitResult(c, strings.TrimLeft(c.render(suffix), "\n"), 124)
		} else {
			result = &ExecResult{Output: strings.TrimLeft(suffix, " \t\n\r"), ReturnCode: 124}
		}
		result.TimedOut = true
	}
	b.self.updateCwd(result)
	b.mu.Lock()
	if b.recreatedNoticePending {
		b.recreatedNoticePending = false
		result.EnvironmentRecreated = true
	}
	b.mu.Unlock()
	if result.Pid == 0 {
		procMu.Lock()
		if spawned != nil {
			result.Pid = spawned.Pid()
		}
		procMu.Unlock()
	}
	return result, nil
}

// Kills a wedged spawned process and its tree on the backstop path
func (b *baseEnv) killSpawnedTree(p ProcessHandle) {
	func() {
		defer func() {
			if r := recover(); r != nil {
				toolsLogger(b.env).Debug("terminal wait-bound kill_process failed", "panic", r)
			}
		}()
		b.self.killProcess(p)
	}()
	if pid := p.Pid(); pid > 0 {
		if _, real := p.(*ExecProcess); real {
			KillProcessTree(pid)
		}
	}
}

// KillProcessTree kills pid and every descendant through the shared deadline tree kill
func KillProcessTree(pid int) { deadlineKillTree(pid, 0) }

// Rewrites sudo for a piped password and applies the macOS open raise ladder
func (b *baseEnv) prepareCommand(ctx context.Context, command string) (string, *string) {
	sessionKey := sessionValue(b.env, "key", "ICEBATH_SESSION_KEY")
	return transformSudoCommand(ctx, b.env, sessionKey, TransformMacosOpenCommand(command, hostSystemName()), func() bool {
		return b.sudoNopasswdWorks(ctx)
	})
}

// Probes sudo -n true inside this backend, failing closed on any error or timeout
func (b *baseEnv) sudoNopasswdWorks(ctx context.Context) bool {
	if !b.passwordlessSudoProbeSupported {
		return false
	}
	p, err := b.self.runBash(ctx, "sudo -n true", false, sudoProbeTimeoutSeconds, nil)
	if err != nil {
		return false
	}
	return b.self.waitForProcess(ctx, p, waitOptions{timeout: sudoProbeTimeoutSeconds}).ReturnCode == 0
}

// FetchFile copies a regular file out of the backend over base64 bounded inside the sandbox
func (b *baseEnv) FetchFile(ctx context.Context, remotePath, localDest string, maxBytes int) error {
	marker := "__ICEBATH_FETCH_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:12] + "__"
	quoted := shQuote(remotePath)
	cmd := fmt.Sprintf("[ -f %s ] && echo %s && head -c %d < %s | base64 && echo %s", quoted, marker, maxBytes+1, quoted, marker)
	result, err := b.self.Execute(ctx, cmd, ExecOptions{Timeout: fetchTimeoutSeconds, NoCompoundRewrite: true})
	if err != nil {
		return err
	}
	output := result.Output
	first, last := strings.Index(output, marker), strings.LastIndex(output, marker)
	if result.ReturnCode != 0 || first == -1 || last <= first {
		return &FileFetchError{Msg: fmt.Sprintf("could not read %q in the sandbox (missing, not a regular file, or unreadable)", remotePath)}
	}
	payload := strings.Join(strings.Fields(output[first+len(marker):last]), "")
	data, err := base64.StdEncoding.DecodeString(payload)
	if err != nil {
		return &FileFetchError{Msg: fmt.Sprintf("transfer of %q was corrupted in transit: %v", remotePath, err)}
	}
	if len(data) > maxBytes {
		return &FileFetchError{Msg: fmt.Sprintf("%q exceeds the %d MB delivery limit", remotePath, maxBytes/(1024*1024))}
	}
	return os.WriteFile(localDest, data, 0o600)
}

// FetchRealpath resolves readlink -f inside the backend, empty when it cannot be resolved
func (b *baseEnv) FetchRealpath(ctx context.Context, remotePath string) string {
	result, err := b.self.Execute(ctx, "readlink -f "+shQuote(remotePath)+" 2>/dev/null", ExecOptions{NoCompoundRewrite: true})
	if err != nil || result.ReturnCode != 0 {
		return ""
	}
	lines := strings.Split(result.Output, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if ln := strings.TrimSpace(lines[i]); strings.HasPrefix(ln, "/") {
			return ln
		}
	}
	return ""
}

// Path helpers

// Characters a sandbox directory name may not hold
var sandboxDirUnsafeRe = regexp.MustCompile(`[^A-Za-z0-9._-]`)

const sandboxDirMaxLen = 128

const sandboxDirHashLen = 12

// SanitizeTaskIDForPath is the bind mountable directory name of a task sandbox, digest suffixed when rewritten
func SanitizeTaskIDForPath(taskID string) string {
	if taskID == "" {
		return "default"
	}
	cleaned := sandboxDirUnsafeRe.ReplaceAllString(taskID, "_")
	if cleaned == taskID && len(taskID) <= sandboxDirMaxLen && taskID != "." && taskID != ".." &&
		!strings.HasSuffix(taskID, ".") && !strings.HasSuffix(taskID, " ") {
		return taskID
	}
	sum := sha256.Sum256([]byte(taskID))
	digest := hex.EncodeToString(sum[:])[:sandboxDirHashLen]
	stem := cleaned
	if limit := sandboxDirMaxLen - sandboxDirHashLen - 1; len(stem) > limit {
		stem = stem[:limit]
	}
	stem = strings.Trim(stem, "._")
	if stem == "" {
		stem = "task"
	}
	return stem + "-" + digest
}

// Streams

// StreamExecPrefixer is implemented by backends whose commands run through a local argv prefix
type StreamExecPrefixer interface {
	ExecPrefix(user string, interactive bool) ([]string, error)
}

// Backend whose client joins the argv words with spaces for a remote login shell
type shellJoinedExecer interface {
	ShellJoinedExec() bool
}

// ExecPrefix is the local argv that runs its remainder inside the environment, nil without one
func ExecPrefix(e Environment, user string, interactive bool) []string {
	p, ok := e.(StreamExecPrefixer)
	if !ok {
		return nil
	}
	prefix, err := p.ExecPrefix(user, interactive)
	if err != nil {
		slog.Warn(fmt.Sprintf("%s.exec_prefix() failed; treating the backend as unable to stream", e.EnvType()), "error", err)
		return nil
	}
	if len(prefix) == 0 {
		return nil
	}
	return prefix
}

// SandboxTempDir is the sandbox side temp directory of an environment
func SandboxTempDir(e Environment) string {
	if e == nil {
		return "/tmp"
	}
	return e.TempDir()
}

// SupportsStreams reports whether the environment can host a long lived stdio stream
func SupportsStreams(e Environment) bool {
	return ExecPrefix(e, "", true) != nil
}

// RemoteArgv is prefix plus a bash -c that exports childEnv and execs argv
func RemoteArgv(prefix, argv []string, childEnv map[string]string, shellJoined bool) []string {
	keys := make([]string, 0, len(childEnv))
	for k := range childEnv {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	exports := make([]string, 0, len(keys))
	for _, k := range keys {
		exports = append(exports, "export "+k+"="+shQuote(childEnv[k])+";")
	}
	script := strings.Join(exports, " ") + " exec " + shJoin(argv)
	out := append([]string{}, prefix...)
	if shellJoined {
		return append(out, shJoin([]string{"bash", "-c", script}))
	}
	return append(out, "bash", "-c", script)
}

// Reports whether the backend client joins argv words for a remote login shell
func shellJoined(e Environment) bool {
	if s, ok := e.(shellJoinedExecer); ok {
		return s.ShellJoinedExec()
	}
	return false
}

// RemoteCommand is the full local argv running argv inside the environment, nil without an exec prefix
func RemoteCommand(e Environment, argv []string, childEnv map[string]string, user string, interactive bool) []string {
	prefix := ExecPrefix(e, user, interactive)
	if prefix == nil {
		return nil
	}
	return RemoteArgv(prefix, argv, childEnv, shellJoined(e))
}

// StreamProcess is a long lived process inside a backend with stdin and stdout as pipes
type StreamProcess struct {
	Cmd    *exec.Cmd
	Stdin  *os.File
	Stdout *os.File
	done   chan struct{}
	code   int
}

// OpenStream spawns argv inside the environment with byte pipes, stderr nil discards it
func OpenStream(e Environment, argv []string, childEnv map[string]string, user string, stderr *os.File) (*StreamProcess, error) {
	command := RemoteCommand(e, argv, childEnv, user, true)
	if command == nil {
		return nil, fmt.Errorf("%s cannot host a long-lived stdio stream", e.EnvType())
	}
	cmd := exec.Command(command[0], command[1:]...)
	stdinR, stdinW, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	stdoutR, stdoutW, err := os.Pipe()
	if err != nil {
		stdinR.Close()
		stdinW.Close()
		return nil, err
	}
	cmd.Stdin = stdinR
	cmd.Stdout = stdoutW
	if stderr != nil {
		cmd.Stderr = stderr
	}
	hideConsoleWindow(cmd)
	if err := cmd.Start(); err != nil {
		stdinR.Close()
		stdinW.Close()
		stdoutR.Close()
		stdoutW.Close()
		return nil, err
	}
	stdinR.Close()
	stdoutW.Close()
	s := &StreamProcess{Cmd: cmd, Stdin: stdinW, Stdout: stdoutR, done: make(chan struct{})}
	go func() {
		defer close(s.done)
		err := cmd.Wait()
		s.code = exitCodeOf(cmd, err)
	}()
	return s, nil
}

// Kill terminates the stream process
func (s *StreamProcess) Kill() error {
	if s.Cmd.Process == nil {
		return nil
	}
	return s.Cmd.Process.Kill()
}

// Wait blocks until the stream exits or the timeout passes
func (s *StreamProcess) Wait(timeout time.Duration) (int, error) {
	if timeout <= 0 {
		<-s.done
		return s.code, nil
	}
	select {
	case <-s.done:
		return s.code, nil
	case <-time.After(timeout):
		return 0, context.DeadlineExceeded
	}
}

// RunInResult is the captured outcome of one short command inside a backend
type RunInResult struct {
	Stdout     []byte
	Stderr     []byte
	ReturnCode int
}

// RunIn runs one short argv inside the environment capturing bytes output
func RunIn(ctx context.Context, e Environment, argv []string, childEnv map[string]string, user string, timeout time.Duration, stdin []byte) (*RunInResult, error) {
	command := RemoteCommand(e, argv, childEnv, user, stdin != nil)
	if command == nil {
		return nil, fmt.Errorf("%s cannot exec argv", e.EnvType())
	}
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(runCtx, command[0], command[1:]...)
	if stdin != nil {
		cmd.Stdin = strings.NewReader(string(stdin))
	}
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	hideConsoleWindow(cmd)
	err := cmd.Run()
	if runCtx.Err() != nil {
		return nil, fmt.Errorf("command timed out after %s", timeout)
	}
	var exitErr *exec.ExitError
	if err != nil && !errors.As(err, &exitErr) {
		return nil, err
	}
	return &RunInResult{Stdout: []byte(stdout.String()), Stderr: []byte(stderr.String()), ReturnCode: exitCodeOf(cmd, err)}, nil
}

// Python relay run inside the sandbox bridging stdio to a loopback port
const tcpRelayScript = "import os,socket,sys,threading\n" +
	"s=socket.create_connection((sys.argv[1],int(sys.argv[2])))\n" +
	"def up():\n" +
	"  while True:\n" +
	"    d=os.read(0,65536)\n" +
	"    if not d: break\n" +
	"    s.sendall(d)\n" +
	"  s.shutdown(socket.SHUT_WR)\n" +
	"threading.Thread(target=up,daemon=True).start()\n" +
	"while True:\n" +
	"  d=s.recv(65536)\n" +
	"  if not d: break\n" +
	"  os.write(1,d)\n"

// Open forwards keyed by environment, host and port
var portForwards = struct {
	sync.Mutex
	m map[portForwardKey]*PortForward
}{m: map[portForwardKey]*PortForward{}}

type portForwardKey struct {
	env  Environment
	host string
	port int
}

// PortForward is a local loopback listener whose connections land on a port inside the environment
type PortForward struct {
	Env        Environment
	RemoteHost string
	RemotePort int
	User       string
	LocalPort  int
	listener   net.Listener
	closed     atomic.Bool
	failures   atomic.Int64
}

// Failures counts connections that never reached the target
func (f *PortForward) Failures() int { return int(f.failures.Load()) }

// NewPortForward starts the listener and its accept loop
func NewPortForward(e Environment, remoteHost string, remotePort int, user string) (*PortForward, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	port := ln.Addr().(*net.TCPAddr).Port
	f := &PortForward{Env: e, RemoteHost: remoteHost, RemotePort: remotePort, User: user, LocalPort: port, listener: ln}
	go f.acceptLoop()
	return f, nil
}

// Accepts connections until closed, serving each on its own goroutine
func (f *PortForward) acceptLoop() {
	for !f.closed.Load() {
		conn, err := f.listener.Accept()
		if err != nil {
			return
		}
		go f.serve(conn)
	}
}

// Stream opener the port forward relays through, swapped in tests for a host side relay
var openStreamFn = OpenStream

// Relays one connection through an exec stream running the sandbox side relay
func (f *PortForward) serve(conn net.Conn) {
	p, err := openStreamFn(f.Env, []string{"python3", "-c", tcpRelayScript, f.RemoteHost, strconv.Itoa(f.RemotePort)}, nil, f.User, nil)
	if err != nil {
		conn.Close()
		f.failures.Add(1)
		return
	}
	var requestMu sync.Mutex
	var request []byte
	go func() {
		defer p.Stdin.Close()
		buf := make([]byte, 65536)
		for {
			n, err := conn.Read(buf)
			if n > 0 {
				requestMu.Lock()
				if request == nil {
					request = append([]byte{}, buf[:n]...)
				}
				requestMu.Unlock()
				if _, werr := p.Stdin.Write(buf[:n]); werr != nil {
					return
				}
			}
			if err != nil {
				return
			}
		}
	}()
	var reply []byte
	buf := make([]byte, 65536)
	for {
		n, err := p.Stdout.Read(buf)
		if n > 0 {
			if reply == nil {
				reply = append([]byte{}, buf[:n]...)
			}
			if _, werr := conn.Write(buf[:n]); werr != nil {
				break
			}
		}
		if err != nil {
			break
		}
	}
	conn.Close()
	requestMu.Lock()
	req := request
	requestMu.Unlock()
	if (reply == nil && streamExitedNonzero(p)) || staleBrowserPath(req, reply) {
		f.failures.Add(1)
	}
	p.Kill()
}

// Reports a relaunched Chromium answering a cached devtools browser path with 404
func staleBrowserPath(request, reply []byte) bool {
	return len(request) > 0 && strings.HasPrefix(string(request), "GET /devtools/browser/") &&
		(strings.HasPrefix(string(reply), "HTTP/1.1 404 ") || strings.HasPrefix(string(reply), "HTTP/1.0 404 "))
}

// Reports whether the relay exited nonzero within two seconds
func streamExitedNonzero(p *StreamProcess) bool {
	code, err := p.Wait(2 * time.Second)
	if err != nil {
		return false
	}
	return code != 0
}

// Close stops the listener
func (f *PortForward) Close() {
	f.closed.Store(true)
	f.listener.Close()
}

// ForwardFailures counts connections through the forward of a port that never reached the target
func ForwardFailures(e Environment, remotePort int, remoteHost string) int {
	if remoteHost == "" {
		remoteHost = "127.0.0.1"
	}
	portForwards.Lock()
	f := portForwards.m[portForwardKey{e, remoteHost, remotePort}]
	portForwards.Unlock()
	if f == nil {
		return 0
	}
	return f.Failures()
}

// ForwardPort returns a local loopback port reaching the remote port inside the environment, one listener per key
func ForwardPort(e Environment, remotePort int, remoteHost, user string) (int, error) {
	if remoteHost == "" {
		remoteHost = "127.0.0.1"
	}
	key := portForwardKey{e, remoteHost, remotePort}
	portForwards.Lock()
	defer portForwards.Unlock()
	f := portForwards.m[key]
	if f == nil || f.closed.Load() {
		var err error
		f, err = NewPortForward(e, remoteHost, remotePort, user)
		if err != nil {
			return 0, err
		}
		portForwards.m[key] = f
	}
	return f.LocalPort, nil
}
