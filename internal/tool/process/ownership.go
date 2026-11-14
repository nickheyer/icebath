package process

import (
	"context"
	"log/slog"
	"os/exec"
	"runtime"

	"github.com/nickheyer/icebath/internal/proc"
	"github.com/nickheyer/icebath/internal/tool"
)

func (r *Registry) ownsJob(persistOnRelease bool) bool {
	return !(runtime.GOOS == "windows" || persistOnRelease || r.jobsOutliveHost.Load() ||
		r.role.Worker() || r.role.SupervisedGateway())
}

func (r *Registry) startJob(ctx context.Context, s *Session, cmd *exec.Cmd, persistOnRelease bool) error {
	if !r.ownsJob(persistOnRelease) {
		detach(cmd)
		return cmd.Start()
	}
	err := r.guard.StartOwned(ctx, cmd, r.ledger, purposeJob)
	if cmd.Process == nil {
		return err
	}
	if err != nil {
		slog.Debug("background job runs without owner-death coverage", "pid", cmd.Process.Pid, "err", err)
	}
	s.ownedPgid = cmd.Process.Pid
	return nil
}

func (r *Registry) ownPTYJob(s *Session, j ptyJob) {
	if !r.ownsJob(s.persistOnRelease) {
		return
	}
	leader, err := proc.Identify(j.pid())
	if err == nil {
		err = r.guard.Own(leader)
	}
	if err != nil {
		slog.Debug("pseudo-terminal job runs without owner-death coverage", "pid", j.pid(), "err", err)
	}
	s.ownedPgid = j.pid()
}

func (r *Registry) takeHandoffGroup(s *Session, p tool.Process) int {
	h, ok := p.(GroupHandoff)
	if !ok {
		return 0
	}
	pgid, ok := h.HandOffGroup()
	if !ok {
		return 0
	}
	if r.ownsJob(false) {
		s.ownedPgid = pgid
		return pgid
	}
	if err := r.guard.Release(pgid); err != nil {
		slog.Debug("could not release handed-off process group", "pgid", pgid, "err", err)
	}
	return pgid
}

func (r *Registry) returnHandoffGroup(s *Session, p tool.Process, pgid int) {
	s.mu.Lock()
	s.ownedPgid = 0
	s.mu.Unlock()
	if pgid == 0 {
		return
	}
	leader, err := proc.Identify(pgid)
	if err == nil {
		err = r.guard.Own(leader)
	}
	if err != nil {
		slog.Debug("could not re-cover handed-back process group", "pgid", pgid, "err", err)
	}
	if h, ok := p.(GroupHandoff); ok {
		h.ReturnGroup(pgid)
	}
}

func (r *Registry) releaseJobGroup(s *Session) {
	s.mu.Lock()
	pgid := s.ownedPgid
	s.ownedPgid = 0
	s.mu.Unlock()
	if pgid == 0 {
		return
	}
	if err := r.guard.Release(pgid); err != nil {
		slog.Debug("could not release job process group", "pgid", pgid, "err", err)
	}
}
