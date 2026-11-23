package tuiserver

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/tuihost"
)

// Seconds of orderly shutdown a termination signal gets before the hard exit
const defaultShutdownGraceS = 1.0

// Collaborators of the stdio entry, the real functions swapped only where a test installs a fake
var (
	entryStdin               io.Reader = os.Stdin
	entryEnsureMCPDiscovery            = ensureMCPDiscoveryStarted
	entryResolveSkin                   = func(ctx context.Context) any { return resolveSkin(ctx) }
	entryStartHeartbeat                = startBackendHeartbeatRefresher
	entryScheduleOrphanSweep           = scheduleStartupOrphanSweep
	entryEnsureSkinWatcher             = ensureSkinWatcher
	entryPrewarmPickerCache            = func(ctx context.Context) { provider.PrewarmPickerCacheAsync(ctx) }
	entryHandleSpuriousEOF             = tuihost.HandleSpuriousEOF
	entryLogExit                       = logExit
	entryWriteJSON                     = tuihost.WriteJSON
	entryDispatch                      = func(ctx context.Context, req any) map[string]any { return Dispatch(ctx, req, nil) }
	entryAppendCrashLog                = appendCrashLog
	entryOSExit                        = os.Exit
)

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

// Records why the gateway exits in the crash log and on stderr
func logExit(reason string) {
	entryAppendCrashLog(fmt.Sprintf("gateway exit · %s · reason=%s", time.Now().Format("2006-01-02 15:04:05"), reason), nil)
	fmt.Fprintf(os.Stderr, "[gateway-exit] %s\n", reason)
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
