// Shadow git checkpoints of working directories with listing, diff, rollback, pruning and store maintenance
package checkpoint

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/home"
	"github.com/nickheyer/icebath/internal/toolchain"
)

const (
	refsPrefix       = "refs/icebath"
	legacyPrefix     = "legacy-"
	storeDirName     = "store"
	pruneMarkerName  = ".last_prune"
	workdirMarker    = "ICEBATH_WORKDIR"
	ledgerMaxEntries = 2000
	maxFiles         = 50000
	mib              = 1024 * 1024
)

var defaultExcludes = []string{
	"node_modules/",
	"dist/",
	"build/",
	"target/",
	"out/",
	".next/",
	".nuxt/",
	"__pycache__/",
	"*.pyc",
	"*.pyo",
	".cache/",
	".pytest_cache/",
	".mypy_cache/",
	".ruff_cache/",
	"coverage/",
	".coverage",
	".venv/",
	"venv/",
	"env/",
	".git/",
	".hg/",
	".svn/",
	".worktrees/",
	"*.so",
	"*.dylib",
	"*.dll",
	"*.o",
	"*.a",
	"*.jar",
	"*.class",
	"*.exe",
	"*.obj",
	"*.mp4",
	"*.mov",
	"*.mkv",
	"*.webm",
	"*.zip",
	"*.tar",
	"*.tar.gz",
	"*.tgz",
	"*.7z",
	"*.rar",
	"*.iso",
	".env",
	".env.*",
	".env.local",
	".env.*.local",
	".DS_Store",
	"Thumbs.db",
	"*.log",
}

type Backends interface {
	ContainerBackend(ctx context.Context, taskID string) (string, error)
}

type Options struct {
	Homes          *home.Resolver
	Tools          *toolchain.Store
	Config         config.CheckpointsConfig
	Backends       Backends
	DelegatedChild bool
}

type Manager struct {
	homes          *home.Resolver
	tools          *toolchain.Store
	backends       Backends
	delegated      bool
	enabled        bool
	maxSnapshots   int
	maxTotalSizeMB int
	maxFileSizeMB  int
	timeout        time.Duration
	mu             sync.Mutex
	turn           map[string]bool
}

func New(o Options) (*Manager, error) {
	if o.Homes == nil {
		return nil, errors.New("checkpoint manager needs a home resolver")
	}
	if o.Tools == nil {
		return nil, errors.New("checkpoint manager needs a toolchain store")
	}
	return &Manager{
		homes:          o.Homes,
		tools:          o.Tools,
		backends:       o.Backends,
		delegated:      o.DelegatedChild,
		enabled:        o.Config.Enabled,
		maxSnapshots:   max(1, o.Config.MaxSnapshots),
		maxTotalSizeMB: max(0, o.Config.MaxTotalSizeMB),
		maxFileSizeMB:  max(0, o.Config.MaxFileSizeMB),
		timeout:        gitTimeout(),
		turn:           map[string]bool{},
	}, nil
}

func gitTimeout() time.Duration {
	seconds := 30
	if raw := strings.TrimSpace(os.Getenv("ICEBATH_CHECKPOINT_TIMEOUT")); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil {
			seconds = n
		}
	}
	return time.Duration(max(10, min(60, seconds))) * time.Second
}

func (m *Manager) Enabled() bool { return m.enabled }

func (m *Manager) NewTurn() {
	m.mu.Lock()
	defer m.mu.Unlock()
	clear(m.turn)
}

func (m *Manager) UnsupportedBackendReason(ctx context.Context, taskID string) (string, error) {
	if m.backends == nil {
		return "", &MissingInterfaceError{Name: "checkpoint.Backends"}
	}
	if taskID == "" {
		taskID = "default"
	}
	backend, err := m.backends.ContainerBackend(ctx, taskID)
	if err != nil {
		return "", err
	}
	if backend == "" {
		return "", nil
	}
	return fmt.Sprintf("Checkpoints are not taken for terminal.backend=%s: file paths belong to the container, not this host.", backend), nil
}

type layout struct {
	home      string
	base      string
	store     string
	indexes   string
	projects  string
	ledgers   string
	gcPending string
	marker    string
}

func layoutOf(h string) layout {
	return layout{
		home:      h,
		base:      home.Checkpoints.In(h),
		store:     home.CheckpointStore.In(h),
		indexes:   home.CheckpointIndexes.In(h),
		projects:  home.CheckpointProjects.In(h),
		ledgers:   home.CheckpointLedgers.In(h),
		gcPending: home.CheckpointGCPending.In(h),
		marker:    home.CheckpointPruneStamp.In(h),
	}
}

func (m *Manager) layout(ctx context.Context) (layout, error) {
	h, err := m.homes.Home(ctx)
	if err != nil {
		return layout{}, err
	}
	return layoutOf(h), nil
}

func (l layout) head() string { return filepath.Join(l.store, "HEAD") }

func (l layout) hasHead() bool { return exists(l.head()) }

func (l layout) index(hash string) string { return filepath.Join(l.indexes, hash) }

func (l layout) meta(hash string) string { return filepath.Join(l.projects, hash+".json") }

func (l layout) ledger(hash string) string { return filepath.Join(l.ledgers, hash+".json") }

func refName(hash string) string { return refsPrefix + "/" + hash }

type MissingInterfaceError struct {
	Name string
}

func (e *MissingInterfaceError) Error() string {
	return e.Name + " is not provided to the checkpoint manager"
}

type InputError struct {
	msg string
}

func (e *InputError) Error() string { return e.msg }

var ErrNoCheckpoints = errors.New("No checkpoints exist for this directory")

type NotFoundError struct {
	Hash   string
	Detail string
}

func (e *NotFoundError) Error() string { return fmt.Sprintf("Checkpoint '%s' not found", e.Hash) }

type NestedReposError struct {
	Paths []string
}

func (e *NestedReposError) Error() string {
	return "Checkpoint contains nested git repositories that were not captured (" + strings.Join(e.Paths, ", ") + "); rollback was not performed"
}

type BusyError struct {
	Base string
}

func (e *BusyError) Error() string { return "checkpoint store is busy: " + e.Base }

type PruneError struct {
	msg string
	err error
}

func (e *PruneError) Error() string { return e.msg }

func (e *PruneError) Unwrap() error { return e.err }

type TooManyFilesError struct {
	Dir   string
	Limit int
}

func (e *TooManyFilesError) Error() string {
	return fmt.Sprintf("Checkpoint skipped: >%d files in %s", e.Limit, e.Dir)
}
