//go:build !windows

package proc

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os/exec"
	"runtime"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// Owner death supervisor in sh, killing at end of input each registered group a recorded member still proves ours
const supervisorScript = `trap '' INT HUP TERM
export LC_ALL=C
parent=$1 mode=$2
case $parent in ''|*[!0-9]*) echo "icebath process supervisor: bad parent pgid '$parent', refusing to run" >&2; exit 2 ;; esac
case $mode in linux|darwin) ;; *) echo "icebath process supervisor: unknown platform '$mode', refusing to run" >&2; exit 2 ;; esac
kill -0 -$$ 2>/dev/null || {
  echo "icebath process supervisor: not a process group leader, refusing to run" >&2
  exit 2
}
NL='
'
reg=' ' pins='' table='' live=''
tok() {
  T=''
  if [ "$mode" = linux ]; then
    s='' l=''
    { while IFS= read -r l; do s="$s$l "; l=''; done < "/proc/$1/stat"; } 2>/dev/null
    s="$s$l"
    s=${s##*) }
    set -f; set -- "$1" $s; set +f
    [ $# -ge 21 ] || return 1
    T="$1 $4 ${21}"
  else
    s=$(command -p ps -o pgid= -o lstart= -p "$1" 2>/dev/null) || return 1
    set -f; set -- "$1" $s; set +f
    [ $# -ge 3 ] || return 1
    T=$*
  fi
}
pin() {
  tok "$2" || return 0
  set -f; set -- "$1" $T; set +f
  [ "$3" = "$1" ] || return 0
  case "$NL$pins" in *"$NL$1 $T$NL"*) ;; *) pins="$pins$1 $T$NL" ;; esac
}
drop() {
  keep=' '
  for g in $reg; do [ "$g" = "$1" ] || keep="$keep$g "; done
  reg=$keep rest=$pins pins=''
  while [ -n "$rest" ]; do
    line=${rest%%"$NL"*} rest=${rest#*"$NL"}
    case $line in "$1 "*) ;; *) pins="$pins$line$NL" ;; esac
  done
}
scan() {
  table=''
  if [ "$mode" = linux ]; then
    for d in /proc/[0-9]*; do
      tok "${d#/proc/}" && table="$table$T$NL"
    done
  else
    table=$(command -p ps -ax -o pid= -o pgid= -o lstart= 2>/dev/null | while IFS= read -r line; do set -f; set -- $line; set +f; [ $# -lt 3 ] || echo "$*"; done)$NL
  fi
}
verified() {
  rest=$pins
  while [ -n "$rest" ]; do
    line=${rest%%"$NL"*} rest=${rest#*"$NL"}
    case $line in "$1 "*) case "$NL$table" in *"$NL${line#* }$NL"*) return 0 ;; esac ;; esac
  done
  return 1
}
adopt() {
  rest=$table
  while [ -n "$rest" ]; do
    line=${rest%%"$NL"*} rest=${rest#*"$NL"}
    set -f; set -- $line; set +f
    case " $live " in *" $2 "*) case "$NL$pins" in *"$NL$2 $line$NL"*) ;; *) pins="$pins$2 $line$NL" ;; esac ;; esac
  done
}
round() {
  scan
  next=''
  for g in $1; do verified "$g" && next="$next $g"; done
  live=$next
  adopt
}
while IFS=' ' read -r verb pg arg rest; do
  case $pg in ''|*[!0-9]*) continue ;; esac
  [ -z "$rest" ] && [ ${#pg} -le 10 ] || continue
  case $verb in
    own)
      [ -z "$arg" ] && [ "$pg" -gt 1 ] && [ "$pg" -ne $$ ] && [ "$pg" -ne "$parent" ] || continue
      case $reg in *" $pg "*) ;; *) reg="$reg$pg " ;; esac
      pin "$pg" "$pg" ;;
    pin)
      case $arg in ''|*[!0-9]*) continue ;; esac
      [ ${#arg} -le 10 ] || continue
      case $reg in *" $pg "*) pin "$pg" "$arg" ;; esac ;;
    release)
      [ -z "$arg" ] && drop "$pg" ;;
  esac
done
case $reg in *[0-9]*) ;; *) exit 0 ;; esac
round "$reg"
for g in $live; do kill -TERM -"$g" 2>/dev/null; done
for _ in 1 2 3; do
  round "$live"
  [ -n "$live" ] || exit 0
  command -p sleep 1 2>/dev/null || sleep 1
done
round "$live"
for g in $live; do kill -KILL -"$g" 2>/dev/null; done
exit 0
`

// Supervisor timing, a young death left to the next registration change and an idle one released after a grace
const (
	supervisorMinUptime = 5 * time.Second
	supervisorIdleExit  = 30 * time.Second
)

// Owner death coverage could not be arranged, so the child runs without dying with this process
var ErrUnguarded = errors.New("owner death coverage unavailable")

// Owner death coverage for child process groups, each registered group killed the moment this process dies by any cause
type Guard struct {
	mu     sync.Mutex
	groups map[int]*ownedGroup
	sup    *supervisor
	idle   *time.Timer
}

// A registered group with the members known to belong to it, its leader first
type ownedGroup struct {
	known []Identity
	stop  context.CancelFunc
	// The leader's exit was handled, so later refreshes need a known member to prove the group
	exitSeen bool
}

// One running supervising shell and the write end of its control pipe
type supervisor struct {
	cmd     *exec.Cmd
	stdin   io.WriteCloser
	started time.Time
	young   bool
	done    chan struct{}
}

func (s *supervisor) exited() bool {
	select {
	case <-s.done:
		return true
	default:
		return false
	}
}

// Empty guard, its supervisor started by the first registration
func NewGuard() *Guard { return &Guard{groups: map[int]*ownedGroup{}} }

// Starts cmd leading a new session and registers its group, ErrUnguarded leaving the started child uncovered
func (g *Guard) Start(cmd *exec.Cmd) error {
	NewSession(cmd)
	if err := cmd.Start(); err != nil {
		return err
	}
	leader, err := Identify(cmd.Process.Pid)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrUnguarded, err)
	}
	return g.Own(leader)
}

// Why the group id leads cannot be owned, nil when it can
func ownable(id Identity) error {
	if err := signallableGroup(id.PID); err != nil {
		return fmt.Errorf("%w: %w", ErrUnguarded, err)
	}
	if !id.Running() {
		return fmt.Errorf("%w: pid %d: %w", ErrUnguarded, id.PID, ErrNotFound)
	}
	if !leadsGroup(id) {
		return fmt.Errorf("%w: pid %d does not lead a process group", ErrUnguarded, id.PID)
	}
	return nil
}

// Registers the groups these running leaders lead to be killed when this process dies
func (g *Guard) Own(leaders ...Identity) error {
	var failed []error
	var valid []Identity
	for _, id := range leaders {
		if err := ownable(id); err != nil {
			failed = append(failed, err)
		} else {
			valid = append(valid, id)
		}
	}
	if len(valid) == 0 {
		return errors.Join(failed...)
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	var payload strings.Builder
	for _, id := range valid {
		if og := g.groups[id.PID]; og != nil {
			if og.known[0] == id {
				continue
			}
			og.stop()
		}
		ctx, stop := context.WithCancel(context.Background())
		g.groups[id.PID] = &ownedGroup{known: []Identity{id}, stop: stop}
		go g.watchLeader(ctx, id)
		fmt.Fprintf(&payload, "own %d\n", id.PID)
	}
	return errors.Join(append(failed, g.flushLocked(payload.String()))...)
}

// Stops covering process groups this process ended or handed off itself
func (g *Guard) Release(pgids ...int) error {
	if len(pgids) == 0 {
		return nil
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	var payload strings.Builder
	for _, p := range pgids {
		if g.dropLocked(p) {
			fmt.Fprintf(&payload, "release %d\n", p)
		}
	}
	return g.flushLocked(payload.String())
}

// Forgets a registered group, false when it was not registered
func (g *Guard) dropLocked(pgid int) bool {
	og := g.groups[pgid]
	if og == nil {
		return false
	}
	og.stop()
	delete(g.groups, pgid)
	return true
}

// Whether a known member, an uncollected one included, still sits in pgid and so keeps its number from reuse
func (og *ownedGroup) pinned(pgid int) bool {
	return slices.ContainsFunc(og.known, func(id Identity) bool { return inGroup(id, pgid) })
}

// Whether a known member of pgid is still alive inside it, something left to cover
func (og *ownedGroup) holds(pgid int) bool {
	return slices.ContainsFunc(og.known, func(id Identity) bool { return notExited(id.PID) && inGroup(id, pgid) })
}

// Whether the incarnation id is running as a member of pgid
func inGroup(id Identity, pgid int) bool {
	if !id.Running() {
		return false
	}
	led, err := unix.Getpgid(id.PID)
	return err == nil && led == pgid
}

// Records every current member of pgid as known and returns the pin lines naming the new ones
func (og *ownedGroup) adopt(pgid int) string {
	members, err := groupMembers(pgid)
	if err != nil {
		return ""
	}
	var payload strings.Builder
	for _, m := range members {
		id := m.Identity()
		if m.Status.Exited() || slices.Contains(og.known, id) {
			continue
		}
		og.known = append(og.known, id)
		fmt.Fprintf(&payload, "pin %d %d\n", pgid, m.PID)
	}
	return payload.String()
}

// Pins the survivors of a group whose leader exited, trusting only an exit seen moments ago, dropping it once unprovable
func (g *Guard) refreshLocked(pgid int) string {
	og := g.groups[pgid]
	if og == nil {
		return ""
	}
	fresh := !og.exitSeen
	og.exitSeen = true
	if fresh || og.pinned(pgid) {
		payload := og.adopt(pgid)
		if og.holds(pgid) {
			return payload
		}
	}
	g.dropLocked(pgid)
	return fmt.Sprintf("release %d\n", pgid)
}

// Pins the survivors the moment the leader exits, while the exit is too recent for number reuse
func (g *Guard) watchLeader(ctx context.Context, leader Identity) {
	if leader.Wait(ctx) != nil {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if og := g.groups[leader.PID]; og == nil || og.known[0] != leader || og.exitSeen {
		return
	}
	if err := g.flushLocked(g.refreshLocked(leader.PID)); err != nil {
		slog.Warn("owner death supervisor update failed", "pgid", leader.PID, "err", err)
	}
}

// Drops registered groups with no member left and refreshes those whose leader is gone, returning the lines to send
func (g *Guard) pruneLocked() string {
	var payload strings.Builder
	for pgid, og := range g.groups {
		switch {
		case !GroupExists(pgid):
			g.dropLocked(pgid)
			fmt.Fprintf(&payload, "release %d\n", pgid)
		case !og.known[0].Running():
			payload.WriteString(g.refreshLocked(pgid))
		}
	}
	return payload.String()
}

// Sends a registration change to the supervisor, starting and replaying one when none runs
func (g *Guard) flushLocked(change string) error {
	change += g.pruneLocked()
	s := g.sup
	if s == nil || s.exited() {
		if len(g.groups) == 0 {
			return nil
		}
		return g.startLocked(false)
	}
	if len(g.groups) > 0 {
		g.cancelIdleLocked()
	}
	if change != "" {
		if _, err := io.WriteString(s.stdin, change); err != nil {
			g.sup = nil
			return fmt.Errorf("%w: %w", ErrUnguarded, err)
		}
	}
	if len(g.groups) == 0 {
		g.releaseWhenIdleLocked(s)
	}
	return nil
}

// Starts a supervisor in its own session and replays every registration and pin to it
func (g *Guard) startLocked(young bool) error {
	cmd := exec.Command("/bin/sh", "-c", supervisorScript, "icebath-supervisor", itoa(unix.Getpgrp()), runtime.GOOS)
	NewSession(cmd)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("%w: %w", ErrUnguarded, err)
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("%w: %w", ErrUnguarded, err)
	}
	s := &supervisor{cmd: cmd, stdin: stdin, started: time.Now(), young: young, done: make(chan struct{})}
	g.sup = s
	go g.watch(s)
	var payload strings.Builder
	for pgid, og := range g.groups {
		fmt.Fprintf(&payload, "own %d\n", pgid)
		for _, id := range og.known[1:] {
			fmt.Fprintf(&payload, "pin %d %d\n", pgid, id.PID)
		}
	}
	if _, err := io.WriteString(stdin, payload.String()); err != nil {
		g.sup = nil
		return fmt.Errorf("%w: %w", ErrUnguarded, err)
	}
	return nil
}

// Reaps a supervisor the moment it exits, restarting one that died while groups were still registered
func (g *Guard) watch(s *supervisor) {
	_ = s.cmd.Wait()
	exit := ExitOf(s.cmd.ProcessState)
	close(s.done)
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.sup != nil && g.sup != s {
		return
	}
	g.sup = nil
	g.pruneLocked()
	if len(g.groups) == 0 {
		return
	}
	oldEnough := time.Since(s.started) >= supervisorMinUptime
	youngSignal := !oldEnough && exit.Signal != 0 && !s.young
	restart := oldEnough || youngSignal
	slog.Warn("owner death supervisor exited with process groups registered", "exit", exit.String(), "groups", len(g.groups), "restarting", restart)
	if restart {
		if err := g.startLocked(youngSignal); err != nil {
			slog.Warn("owner death supervisor restart failed", "err", err)
		}
	}
}

// Closes the supervisor pipe once the registrations stay empty for the idle grace
func (g *Guard) releaseWhenIdleLocked(s *supervisor) {
	g.cancelIdleLocked()
	g.idle = time.AfterFunc(supervisorIdleExit, func() {
		g.mu.Lock()
		defer g.mu.Unlock()
		if g.sup != s || len(g.groups) > 0 {
			return
		}
		s.stdin.Close()
		g.sup = nil
	})
}

func (g *Guard) cancelIdleLocked() {
	if g.idle != nil {
		g.idle.Stop()
		g.idle = nil
	}
}

// Whether no live member is left in pgid, zombies waiting for their parent not counting
func groupGone(pgid int) bool {
	if !GroupExists(pgid) {
		return true
	}
	members, err := groupMembers(pgid)
	if err != nil {
		return false
	}
	for _, m := range members {
		if !m.Status.Exited() {
			return false
		}
	}
	return true
}

// Whether pgid has no live member left, releasing it when so, a refused probe keeping it covered
func (g *Guard) releaseIfEmpty(pgid int) bool {
	if !groupGone(pgid) {
		return false
	}
	if err := g.Release(pgid); err != nil {
		slog.Warn("owner death supervisor update failed", "pgid", pgid, "err", err)
	}
	return true
}

// Releases pgid once no live member is left, waiting until ctx ends
func (g *Guard) ReleaseWhenEmpty(ctx context.Context, pgid int) bool {
	for {
		if g.releaseIfEmpty(pgid) {
			return true
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// Signals an owned group while a known member is seen running in it, false when none proves it ours
func (g *Guard) signalOwned(pgid int, sig syscall.Signal) (bool, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	og := g.groups[pgid]
	if og == nil || !og.pinned(pgid) {
		return false, nil
	}
	if leader := og.known[0]; leader.Running() {
		return true, signalLedGroup(leader, sig)
	}
	return true, groupError(pgid, unix.Kill(-pgid, sig))
}

// Ends what is left of an owned group after its leader exited, SIGTERM then SIGKILL after grace, true once released
func (g *Guard) StopSurvivors(ctx context.Context, pgid int, grace time.Duration) bool {
	for _, step := range []struct {
		sig  syscall.Signal
		wait time.Duration
	}{{syscall.SIGTERM, grace}, {syscall.SIGKILL, time.Second}} {
		if g.releaseIfEmpty(pgid) {
			return true
		}
		ours, err := g.signalOwned(pgid, step.sig)
		if !ours || (err != nil && !errors.Is(err, ErrNotFound)) {
			return false
		}
		wait, cancel := context.WithTimeout(ctx, step.wait)
		released := g.ReleaseWhenEmpty(wait, pgid)
		cancel()
		if released {
			return true
		}
	}
	return false
}
