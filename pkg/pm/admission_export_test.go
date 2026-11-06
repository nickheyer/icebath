package pm

import (
	"context"
	"os"
	"path/filepath"
)

// Survival harness pieces the admission contract tests of package pm_test drive
var (
	SvAdmissionEnv     = svAdmissionEnv
	SvConflictMembers  = svConflictMembers
	SvWriteEnabled     = svWriteEnabled
	SvSelected         = svSelected
	SvConfig           = svConfig
	SvVenvFact         = svVenvFact
	SvInstallingMember = svInstallingMember
	SvCurrent          = svCurrent
	WsHost             = wsHost
	PeUV               = peUV
	PeHostPython       = peHostPython
	SyncVenvInProcess  = syncVenv
)

// Bytes of the latest machine readable sync receipt
func LatestReceiptBytes(ctx context.Context) ([]byte, error) {
	return os.ReadFile(filepath.Join(receiptDir(ctx), "latest.json"))
}
