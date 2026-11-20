package agentimport

import (
	"context"
	"errors"
	"sync"

	"github.com/nickheyer/icebath/pkg/cliui"
)

// OpenClawArgs are the flags of an OpenClaw import as the command line parsed them, empty meaning not passed
type OpenClawArgs struct {
	Console         *cliui.Console
	Source          string
	DryRun          bool
	Overwrite       bool
	Yes             bool
	Preset          string
	MigrateSecrets  bool
	NoBackup        bool
	WorkspaceTarget string
	SkillConflict   string
	FromSetup       bool
	Include         []string
	Exclude         []string
	OutputDir       string
	JSON            bool
}

// OpenClawCleanupArgs are the flags of claw cleanup
type OpenClawCleanupArgs struct {
	Console *cliui.Console
	Source  string
	DryRun  bool
	Yes     bool
}

// OpenClawImporter is the OpenClaw half of the importer, installed by its package when the binary links it
type OpenClawImporter struct {
	FindDirs func() []string
	Migrate  func(ctx context.Context, args OpenClawArgs) (int, error)
	Cleanup  func(ctx context.Context, args OpenClawCleanupArgs) (int, error)
}

var (
	openClawMu  sync.RWMutex
	openClawImp *OpenClawImporter
)

// ErrOpenClawMissing reports that no OpenClaw importer is linked into the binary
var ErrOpenClawMissing = errors.New("the OpenClaw importer (pkg/agentimport/openclaw) is not linked into this binary")

// RegisterOpenClaw installs the OpenClaw importer
func RegisterOpenClaw(i OpenClawImporter) {
	openClawMu.Lock()
	defer openClawMu.Unlock()
	cp := i
	openClawImp = &cp
}

// Returns the registered OpenClaw importer or ErrOpenClawMissing
func openClaw() (*OpenClawImporter, error) {
	openClawMu.RLock()
	defer openClawMu.RUnlock()
	if openClawImp == nil {
		return nil, ErrOpenClawMissing
	}
	return openClawImp, nil
}
