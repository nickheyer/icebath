package tuiserver

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/observability"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/tools"
	"github.com/nickheyer/icebath/pkg/tuigateway"
)

func entryLogger() *slog.Logger { return slog.Default().With("logger", "tui_gateway.entry") }

// Seconds of orderly shutdown a termination signal gets before the hard exit
const defaultShutdownGraceS = 1.0

// Grace before the hard exit, ICEBATH_TUI_GATEWAY_SHUTDOWN_GRACE_S winning when positive
func shutdownGraceSeconds() float64 {
	if value := envFloat("ICEBATH_TUI_GATEWAY_SHUTDOWN_GRACE_S", defaultShutdownGraceS); value > 0 {
		return value
	}
	return defaultShutdownGraceS
}

// Collaborators of the stdio entry, the real functions swapped only where a hermes case installs a fake
var (
	entryStdin                   io.Reader = os.Stdin
	entryInstallSidecarPublisher           = installSidecarPublisher
	entryEnsureMCPDiscovery                = ensureMCPDiscoveryStarted
	entryResolveSkin                       = func(ctx context.Context) any { return resolveSkin(ctx) }
	entryStartHeartbeat                    = startBackendHeartbeatRefresher
	entryScheduleOrphanSweep               = scheduleStartupOrphanSweep
	entryEnsureSkinWatcher                 = ensureSkinWatcher
	entryPrewarmPickerCache                = func(ctx context.Context) { provider.PrewarmPickerCacheAsync(ctx) }
	entryHandleSpuriousEOF                 = tuigateway.HandleSpuriousEOF
	entryLogExit                           = logExit
	entryWriteJSON                         = tuigateway.WriteJSON
	entryDispatch                          = func(ctx context.Context, req any) map[string]any { return Dispatch(ctx, req, nil) }
	entryAppendCrashLog                    = appendCrashLog
	entryOSExit                            = os.Exit
)

// Mirrors every stdio frame to the dashboard sidecar socket when ICEBATH_TUI_SIDECAR_URL names one
func installSidecarPublisher() {
	url := os.Getenv("ICEBATH_TUI_SIDECAR_URL")
	if url == "" {
		return
	}
	tuigateway.SetModuleStdioTransport(tuigateway.NewTeeTransport(tuigateway.ModuleStdioTransport(),
		tuigateway.NewWsPublisherTransport(url, 2*time.Second)))
}

// Appends one headed entry to the crash log with the detail dump writes, never raising
func appendCrashLog(header string, dump func(io.Writer)) {
	suppress(func() {
		path := crashLogPath()
		if err := os.MkdirAll(filepath.Dir(path), 0o777); err != nil {
			return
		}
		f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o666)
		if err != nil {
			return
		}
		defer f.Close()
		fmt.Fprintf(f, "\n=== %s ===\n", header)
		if dump != nil {
			dump(f)
		}
	})
}

// Stamps a clean exit, kills every live foreground command tree at once and ends the process with status 0
func hardExit() {
	suppress(func() { observability.StampExit("clean", "none") })
	suppress(func() { tools.KillLiveForegroundProcesses(true) })
	entryOSExit(0)
}

// Records why the gateway exits in the crash log and on stderr
func logExit(reason string) {
	entryAppendCrashLog(fmt.Sprintf("gateway exit · %s · reason=%s", time.Now().Format("2006-01-02 15:04:05"), reason), nil)
	fmt.Fprintf(os.Stderr, "[gateway-exit] %s\n", reason)
}

// Name a termination signal is logged under, the number for one with no name
func entrySignalName(sig os.Signal) string {
	if name, known := entrySignalNames[sig]; known {
		return name
	}
	number, _ := sig.(syscall.Signal)
	return fmt.Sprintf("signal %d", int(number))
}

// Writes the main goroutine stack and then every goroutine stack, what a termination signal leaves in the crash log
func dumpSignalStacks(w io.Writer) {
	buf := make([]byte, 1<<16)
	for {
		n := runtime.Stack(buf, true)
		if n < len(buf) {
			buf = buf[:n]
			break
		}
		buf = make([]byte, 2*len(buf))
	}
	blocks := strings.Split(strings.TrimSpace(string(buf)), "\n\n")
	for _, block := range blocks {
		if strings.HasPrefix(block, "goroutine 1 ") {
			fmt.Fprintf(w, "main-thread stack at signal delivery:\n%s\n", block)
		}
	}
	for _, block := range blocks {
		head, _, _ := strings.Cut(block, "\n")
		id, _, _ := strings.Cut(strings.TrimPrefix(head, "goroutine "), " ")
		fmt.Fprintf(w, "\n--- thread goroutine %s (id=%s) ---\n%s\n", id, id, block)
	}
}

// One run of the stdio entry, its reserved home released once by main or by a signal
type stdioEntry struct {
	ctx      context.Context
	exit     func(int)
	mu       sync.Mutex
	home     string
	ended    sync.Once
	recovery []float64
}

// Runs the stdio gateway when the binary was re-executed as it, ending the process through exit
func DispatchTUIGatewayEntry(ctx context.Context, exit func(int)) {
	if len(os.Args) < 2 || os.Args[1] != tuigateway.TUIGatewayEntry {
		return
	}
	runStdioEntry(ctx, exit)
}

// Loads the server module, keeps stray prints off the RPC stdout, installs the signals and runs main
func runStdioEntry(ctx context.Context, exit func(int)) {
	loadServerModule()
	os.Stdout = os.Stderr
	e := &stdioEntry{ctx: ctx, exit: exit}
	installEntrySignals(e)
	e.finish(e.main())
}

// Releases the reserved launch home, a second call finding nothing left to release
func (e *stdioEntry) releaseHome() {
	e.mu.Lock()
	h := e.home
	e.home = ""
	e.mu.Unlock()
	if h != "" {
		gateway.ProcessOwnership.Release(h)
	}
}

// Releases the reserved home and exits with code, once whichever of the read loop and a signal gets here first
func (e *stdioEntry) finish(code int) {
	e.ended.Do(func() {
		e.releaseHome()
		e.exit(code)
	})
}

// Logs every goroutine stack, arms the hard exit, finalizes sessions and exits 0 through the orderly path
func (e *stdioEntry) logSignal(sig os.Signal) error {
	name := entrySignalName(sig)
	entryAppendCrashLog(fmt.Sprintf("%s received · %s", name, time.Now().Format("2006-01-02 15:04:05")), dumpSignalStacks)
	fmt.Fprintf(os.Stderr, "[gateway-signal] %s\n", name)
	time.AfterFunc(time.Duration(shutdownGraceSeconds()*float64(time.Second)), hardExit)
	suppress(shutdownSessions)
	e.finish(0)
	return nil
}

// Reserves the launch home as the gateway does, empty when a running gateway or another entry owns it
func claimLaunchHome(ctx context.Context) (string, error) {
	h := launchHome()
	err := gateway.ProcessOwnership.Reserve(ctx, []string{h}, "")
	var conflict *gateway.OwnershipConflict
	if errors.As(err, &conflict) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return h, nil
}

// Serves the stdio RPC channel while it holds the launch home, 1 when another owner holds it
func (e *stdioEntry) main() int {
	defer panicHook()
	h, err := claimLaunchHome(e.ctx)
	if err != nil {
		recordCrash("unhandled exception", err, nil)
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if h == "" {
		entryLogExit(fmt.Sprintf("refused: %s is owned by a running gateway; attach to it instead", launchHome()))
		return 1
	}
	e.mu.Lock()
	e.home = h
	e.mu.Unlock()
	defer e.releaseHome()
	if err := e.serve(); err != nil {
		recordCrash("unhandled exception", err, nil)
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return 0
}

// Writes one frame, logging the reason and returning false when stdout is gone
func (e *stdioEntry) writeOrExit(payload *pyjson.Object, reason string) bool {
	if entryWriteJSON(e.ctx, payload) {
		return true
	}
	entryLogExit(reason)
	return false
}

// Announces readiness on stdout and answers one JSON RPC request per stdin line until the peer closes it
func (e *stdioEntry) serve() error {
	ctx := e.ctx
	tuigateway.SetStdioIsRPCChannel(true)
	func() {
		defer func() {
			if failure := recover(); failure != nil {
				entryLogger().Warn("TUI message injector did not install", "exc_info", failureText(failure))
			}
		}()
		InstallTUIMessageInjector(ctx, nil)
	}()
	closeRPCStdinOnExec()
	entryInstallSidecarPublisher()
	observability.BeginProcess(ctx, "tui")
	observability.SetProcessSurface("tui_gateway")
	func() {
		defer func() {
			if failure := recover(); failure != nil {
				entryLogger().Debug("truststore install skipped", "exc_info", failureText(failure))
			}
		}()
		provider.InstallTrustStore()
	}()
	for _, step := range []struct {
		start func(context.Context)
		what  string
	}{{entryStartHeartbeat, "backend heartbeat refresher start"}, {entryScheduleOrphanSweep, "startup orphan sweep scheduling"}} {
		func() {
			defer func() {
				if failure := recover(); failure != nil {
					entryLogger().Warn(fmt.Sprintf("%s failed", step.what), "exc_info", failureText(failure))
				}
			}()
			step.start(ctx)
		}()
	}
	entryEnsureMCPDiscovery(ctx)
	ready := pyjson.ObjectOf("jsonrpc", "2.0", "method", "event", "params", pyjson.ObjectOf("type", "gateway.ready",
		"payload", pyjson.ObjectOf("skin", pyjsonValue(entryResolveSkin(ctx)), "change_events", true,
			"replay_epoch", tuigateway.ReplayEpoch())))
	if !e.writeOrExit(ready, "startup write failed (broken stdout pipe before first event)") {
		return nil
	}
	entryEnsureSkinWatcher(ctx)
	func() {
		defer func() {
			if failure := recover(); failure != nil {
				entryLogger().Debug("picker cache prewarm (tui) failed to start", "exc_info", failureText(failure))
			}
		}()
		entryPrewarmPickerCache(ctx)
	}()
	reader := bufio.NewReader(entryStdin)
	for {
		raw, readErr := reader.ReadString('\n')
		if raw == "" && readErr != nil {
			if !errors.Is(readErr, io.EOF) && !errors.Is(readErr, syscall.EAGAIN) {
				return readErr
			}
			if !entryHandleSpuriousEOF(&e.recovery, entryLogExit) {
				return nil
			}
			continue
		}
		line := pystr.Strip(raw)
		if line == "" {
			continue
		}
		if !e.answer(line) {
			return nil
		}
	}
}

// Parses and dispatches one request line and writes its reply, false once stdout is gone
func (e *stdioEntry) answer(line string) bool {
	req, err := pyjson.Loads(line)
	if err != nil {
		return e.writeOrExit(pyjson.ObjectOf("jsonrpc", "2.0", "error", pyjson.ObjectOf("code", -32700, "message", "parse error"),
			"id", nil), "parse-error-response write failed (broken stdout pipe)")
	}
	var method, rid any
	if obj, isDict := req.(*pyjson.Object); isDict {
		method = obj.GetOr("method", nil)
		rid = obj.GetOr("id", nil)
	}
	var resp map[string]any
	func() {
		defer func() {
			failure := recover()
			if failure == nil {
				return
			}
			trace := fmt.Sprintf("%s\n%s", failureText(failure), debugStack())
			entryLogger().Error(fmt.Sprintf("inline RPC handler failed for method=%s id=%s", pyrepr.Repr(method), pyrepr.Repr(rid)),
				"exc_info", trace)
			entryAppendCrashLog(fmt.Sprintf("inline dispatch crash · %s · method=%s", time.Now().Format("2006-01-02 15:04:05"),
				pyrepr.Repr(method)), func(w io.Writer) { io.WriteString(w, trace) })
			resp = rpcErr(rid, -32000, "handler error: "+failureText(failure), nil)
		}()
		resp = entryDispatch(e.ctx, req)
	}()
	if resp == nil {
		return true
	}
	return e.writeOrExit(frameObject(resp), fmt.Sprintf("response write failed for method=%s (broken stdout pipe)", pyrepr.Repr(method)))
}

// Stack of the calling goroutine, the traceback a recovered handler failure records
func debugStack() string {
	buf := make([]byte, 1<<16)
	return string(buf[:runtime.Stack(buf, false)])
}
