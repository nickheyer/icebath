//go:build !windows

package proc

import (
	"context"
	"log/slog"
	"os/exec"
	"time"
)

const zombiesExist = true

const supervisorName = "icebath-supervisor"

func reparentedOrphan(pid int, sshClaimed func() map[int]bool) bool {
	parent, err := ParentOf(pid)
	if err != nil || (parent != 0 && parent != 1) {
		return false
	}
	if sshClaimed()[pid] {
		return false
	}
	started, err := Started(pid)
	if err != nil {
		return false
	}
	return max(0, float64(time.Now().UnixNano())/1e9-started) >= reapMinAge
}

func stopHelper(ctx context.Context, id Identity) bool { return terminateThenKill(ctx, id) }

func AttachSelfToKillOnCloseJob() (bool, error) { return false, nil }

func IsOwnerDeathSupervisor(argv []string) bool {
	return len(argv) >= 4 && argv[1] == "-c" && argv[2] == supervisorScript && argv[3] == supervisorName
}

func (g *Guard) StartOwned(ctx context.Context, cmd *exec.Cmd, ledger *Ledger, purpose string) error {
	err := g.Start(cmd)
	if cmd.Process == nil {
		return err
	}
	if regErr := ledger.RegisterChild(ctx, cmd.Process.Pid, purpose); regErr != nil {
		slog.Debug("spawn ledger registration failed", "pid", cmd.Process.Pid, "purpose", purpose, "err", regErr)
	}
	return err
}
