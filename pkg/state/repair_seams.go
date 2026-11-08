package state

import (
	"os"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyfs"
)

// Repair collaborators as swappable values, the module attributes the hermes repair tests patch
var (
	repairProbeFn        = dbOpensCleanly
	repairScratchSpaceFn = repairScratchSpaceError
	repairBackupFn       = backupDBFile
	repairSnapshotFn     = copyDatabaseSnapshot
	repairStrategiesFn   = runRepairStrategies
	repairGuardFn        = exclusiveRepairDBGuard
	repairConnectFn      = connectRepairDurable
	repairLockFn         = crossProcessRepairLock
	repairRecordFn       = recordRepairOutcome
	repairExhaustedFn    = persistentRepairAttemptsExhausted
	repairLockedFn       = repairStateDBSchemaLocked
	repairUnlinkFn       = unlinkDBTriple
	repairJournalProbeFn = probeJournalModeForRepair
	repairLiveWriterFn   = liveWriterHoldsDB
	applyWALFn           = ApplyWALWithFallback
	openRepairFn         = RepairStateDBSchema
	diskUsageFn          = pyfs.DiskUsage
	copyFileFn           = pyfs.Copy2
	replaceFileFn        = os.Rename
	snapshotClock        = time.Now
)

// Seconds a repair waits for the cross process schema surgery lock
var repairLockTimeoutSeconds = 120.0

// Slowest copy throughput a repair snapshot deadline assumes
var repairSnapshotMinThroughputBytesPerSecond = 10.0 * 1024 * 1024
