package updater

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nickheyer/icebath/pkg/desktop/backend"
	"github.com/nickheyer/icebath/pkg/desktop/desktoplog"
	"github.com/nickheyer/icebath/pkg/install"
)

// MessageBox is a native message box the shell shows
type MessageBox struct {
	Type      string
	Title     string
	Message   string
	Detail    string
	Buttons   []string
	DefaultID int
	CancelID  int
	NoLink    bool
}

// Host is the shell surface the updater drives, windows, dialogs and the app lifecycle
type Host interface {
	// Window whose boot surface owns the update hold and bootstrap, empty when none
	MainWindowID() string
	// Sends one event to one window
	Send(windowID, event string, payload any)
	// Sends one event to every window
	Broadcast(event string, payload any)
	// Shows a message box and answers the index of the button clicked
	MessageBox(box MessageBox) int
	// Reveals a path in the platform file manager
	ShowItemInFolder(path string)
	// Asks the renderer to open the update settings, queued until it is ready
	OpenUpdates()
	// Quits the app through its normal quit path
	Quit()
	// Relaunches this app with args after it exits
	Relaunch(args []string) error
	// Stops the backends and exits the process with code
	ExitAfterBackendShutdown(code int)
	// Version of the running desktop app
	AppVersion() string
	// Native handle of the focused or main window, zero when none is open
	WindowHandle() uintptr
	// Opens a file with its default handler, the error text when it cannot
	OpenPath(path string) string
}

// Backend is the backend service surface the update gate and bootstrap report through
type Backend interface {
	UpdateBootProgress(update map[string]any, allowDecrease bool)
	AdvanceBootProgress(phase, message string, progress int)
	TeardownPrimary(soft bool) error
	StopAllPoolBackends()
	QuitContext() context.Context
	AssertCanStart() error
	SetStartFailure(err error)
	ClearStartFailures()
	PrimaryProcessAlive() bool
	EnsureBackend(profile string, opts backend.EnsureOptions) (backend.Connection, error)
}

// ManagedUpdates is the managed SSH update join of the connections group a local update apply waits on
type ManagedUpdates interface {
	// Waits up to two minutes for managed SSH updates and recoveries, false with the refusal code and message while one runs
	JoinManagedUpdatesForApply(log func(string)) (joined bool, code, message string)
}

// Options name where the install lives, how the backend resolves and where the desktop logs
type Options struct {
	IcebathHome           string
	UserData              string
	IsPackaged            bool
	ResourcesPath         string
	SourceRepoRoot        string
	Resolve               func(args []string) (backend.ResolvedBackend, error)
	InstalledRuntime      *backend.InstalledRuntimeGate[backend.ResolvedBackend]
	DesktopLog            *desktoplog.Log
	Home                  backend.HomeInputs
	MsixAppIDWithOrg      string
	StoreMsixIdentityName string
	Getenv                func(string) string
	Args                  []string
}

// Service is the updater group behind the update, bootstrap, uninstall and metrics preload functions
type Service struct {
	host    Host
	backend Backend
	opts    Options
	stamp   *InstallStamp

	holds     *holdRegistry
	screen    *holdScreen
	metrics   *updateRunRecorder
	operation *updateOperation
	conns     Connections
	firstRun  FirstRunGate
	consent   releaseConsent
	started   time.Time

	updateInFlight     atomic.Bool
	quittingForHandoff atomic.Bool

	mu                       sync.Mutex
	bootstrapFailure         error
	bootstrapRepairAttempt   int
	bootstrapRepairRequested bool
	bootstrapAbort           context.CancelFunc
	bootstrap                bootstrapState
}

// Updater service over the shell, the install home and the artifact's baked stamp
func NewService(host Host, opts Options) *Service {
	if opts.Getenv == nil {
		opts.Getenv = os.Getenv
	}
	if opts.Args == nil {
		opts.Args = os.Args
	}
	s := &Service{host: host, opts: opts, stamp: readInstallStamp(opts.ResourcesPath), holds: newHoldRegistry()}
	s.screen = &holdScreen{home: opts.IcebathHome, log: s.log, progress: func(update map[string]any) { s.backend.UpdateBootProgress(update, false) }}
	s.bootstrap = emptyBootstrapState()
	s.metrics = &updateRunRecorder{dir: func() string { return opts.UserData }, appVersion: host.AppVersion, now: time.Now, onRecorded: func() { host.Broadcast("updates:metric:pending", nil) }}
	s.operation = &updateOperation{initialize: s.createPackagedUpdateStrategy}
	s.started = time.Now()
	return s
}

// Joins the backend service the gate and bootstrap report boot progress through
func (s *Service) AttachBackend(b Backend) {
	s.backend = b
}

func (s *Service) log(line string) {
	s.opts.DesktopLog.Remember(line)
}

func (s *Service) isWindows() bool { return runtime.GOOS == "windows" }

func isMacOS() bool { return runtime.GOOS == "darwin" }

// Platform name the backend env builder reads, Node's spelling
func backendPlatform() string {
	if runtime.GOOS == "windows" {
		return "win32"
	}
	return runtime.GOOS
}

// Install root the desktop bootstraps and updates, ICEBATH_HOME/icebath-agent
func (s *Service) activeRoot() string {
	return install.InstallRootOf(s.opts.IcebathHome)
}

// Icebath Go source tree, the go.mod of the module and its cmd/icebath entry
func isIcebathSourceRoot(root string) bool {
	if info, err := os.Stat(root); err != nil || !info.IsDir() {
		return false
	}
	info, err := os.Stat(filepath.Join(root, "cmd", "icebath", "main.go"))
	return err == nil && info.Mode().IsRegular()
}

func isGitCheckout(root string) bool {
	_, err := os.Stat(filepath.Join(root, ".git"))
	return err == nil
}

// Root an update acts on, the pinned worktree first, then a dev source tree, then the active install
func (s *Service) resolveUpdateRoot() string {
	var candidates []string
	if pinned := s.opts.Getenv("ICEBATH_DESKTOP_ICEBATH_ROOT"); pinned != "" {
		if abs, err := filepath.Abs(pinned); err == nil {
			candidates = append(candidates, abs)
		}
	}
	if !s.opts.IsPackaged && s.opts.SourceRepoRoot != "" && isIcebathSourceRoot(s.opts.SourceRepoRoot) {
		candidates = append(candidates, s.opts.SourceRepoRoot)
	}
	if isIcebathSourceRoot(s.activeRoot()) {
		candidates = append(candidates, s.activeRoot())
	}
	for _, candidate := range candidates {
		if isGitCheckout(candidate) {
			return candidate
		}
	}
	if len(candidates) > 0 {
		return candidates[0]
	}
	return s.activeRoot()
}

func (s *Service) isPrimaryBootSender(windowID string) bool {
	main := s.host.MainWindowID()
	return main != "" && windowID == main
}

// Update hold the boot screen shows, nil when none
func (s *Service) UpdateHold() any {
	return s.screen.carried()
}

// Latched bootstrap failure, nil when none
func (s *Service) BootstrapFailure() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.bootstrapFailure
}

// Latches a bootstrap failure every start returns until a reset or repair
func (s *Service) SetBootstrapFailure(err error) {
	s.mu.Lock()
	s.bootstrapFailure = err
	s.mu.Unlock()
}

// Zeroes the repair attempts once a local gateway is ready
func (s *Service) ResetBootstrapRepair() {
	s.mu.Lock()
	s.bootstrapRepairAttempt = 0
	s.mu.Unlock()
}

var (
	_ backend.Runtime = (*Service)(nil)
	_ Backend         = (*backend.Service)(nil)
)

// Whether the app is quitting for an update hand-off
func (s *Service) QuittingForHandoff() bool {
	return s.quittingForHandoff.Load()
}
