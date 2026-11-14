//go:build !windows

package env

import (
	"errors"
	"io/fs"
	"log/slog"
	"os/exec"
	"syscall"
	"time"

	"github.com/nickheyer/icebath/internal/proc"
)

func startGroup(cmd *exec.Cmd) {
	proc.NewSession(cmd)
}

func multiLinked(info fs.FileInfo) bool {
	st, ok := info.Sys().(*syscall.Stat_t)
	return ok && st.Nlink > 1
}

func (p *cmdProcess) adopt() {
	pid := p.cmd.Process.Pid
	p.pgid = pid
	if g, err := proc.GroupOf(pid); err == nil {
		p.pgid = g
	}
	id, err := proc.Identify(pid)
	if err != nil {
		return
	}
	p.leader, p.known = id, true
	if p.guard == nil {
		return
	}
	if err := p.guard.Own(id); err != nil {
		slog.Debug("terminal command group runs without owner-death coverage", "pid", pid, "err", err)
		return
	}
	p.mu.Lock()
	p.owned = true
	p.mu.Unlock()
}

func (p *cmdProcess) leaderIsOurs(pgid int) bool {
	if !p.known {
		return true
	}
	if !proc.Exists(pgid) {
		return true
	}
	return proc.Identity{PID: pgid, Start: p.leader.Start}.Running()
}

func (p *cmdProcess) descendants() []proc.Identity {
	table, err := proc.ReadTable()
	if err != nil {
		return nil
	}
	root := p.leader
	if !p.known {
		id, err := proc.Identify(p.cmd.Process.Pid)
		if err != nil {
			return nil
		}
		root = id
	}
	return table.Descendants(root)
}

func sweepEscaped(descendants []proc.Identity, pgid int) {
	for _, d := range descendants {
		if !d.Alive() {
			continue
		}
		if g, err := proc.GroupOf(d.PID); err == nil && g == pgid {
			continue
		}
		_ = d.Signal(syscall.SIGKILL)
	}
}

func (p *cmdProcess) killKnown(descendants []proc.Identity) {
	_ = p.cmd.Process.Kill()
	for _, d := range descendants {
		_ = d.Signal(syscall.SIGKILL)
	}
}

func (p *cmdProcess) waitGroupExit(pgid int, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for {
		if !proc.GroupExists(pgid) {
			return true
		}
		if !time.Now().Before(deadline) {
			return false
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func killGroup(p *cmdProcess) {
	pgid := p.pgid
	if g, err := proc.GroupOf(p.cmd.Process.Pid); err == nil {
		pgid = g
	} else if pgid == 0 {
		_ = p.cmd.Process.Kill()
		return
	}
	if !p.leaderIsOurs(pgid) {
		sweepEscaped(p.descendants(), pgid)
		return
	}
	descendants := p.descendants()
	if pgid == syscall.Getpgrp() {
		p.killKnown(descendants)
		sweepEscaped(descendants, pgid)
		return
	}
	switch err := syscall.Kill(-pgid, syscall.SIGTERM); {
	case errors.Is(err, syscall.ESRCH):
	case errors.Is(err, syscall.EPERM):
		p.killKnown(descendants)
	case err != nil:
		_ = p.cmd.Process.Kill()
		return
	default:
		if !p.waitGroupExit(pgid, time.Second) {
			if !p.leaderIsOurs(pgid) {
				return
			}
			switch err := syscall.Kill(-pgid, syscall.SIGKILL); {
			case errors.Is(err, syscall.ESRCH):
			case errors.Is(err, syscall.EPERM):
				p.killKnown(descendants)
			case err != nil:
				_ = p.cmd.Process.Kill()
				return
			default:
				p.waitGroupExit(pgid, 2*time.Second)
				select {
				case <-p.done:
				case <-time.After(200 * time.Millisecond):
				}
			}
		}
	}
	sweepEscaped(descendants, pgid)
}

func forceKillGroup(p *cmdProcess) {
	pgid := p.pgid
	if pgid == 0 {
		if g, err := proc.GroupOf(p.cmd.Process.Pid); err == nil {
			pgid = g
		}
	}
	if pgid != 0 && pgid != syscall.Getpgrp() && p.leaderIsOurs(pgid) {
		_ = syscall.Kill(-pgid, syscall.SIGKILL)
	}
	_ = p.cmd.Process.Kill()
}
