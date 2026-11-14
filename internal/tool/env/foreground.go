package env

import (
	"sync"
	"time"

	"github.com/nickheyer/icebath/internal/tool"
)

const hardKillBudget = 500 * time.Millisecond

type liveCommand struct {
	shell *Shell
	p     tool.Process
}

type Foreground struct {
	mu       sync.Mutex
	changed  chan struct{}
	live     map[*liveCommand]struct{}
	fenced   bool
	inFlight int
}

func NewForeground() *Foreground {
	return &Foreground{changed: make(chan struct{}), live: map[*liveCommand]struct{}{}}
}

func (f *Foreground) notifyLocked() {
	close(f.changed)
	f.changed = make(chan struct{})
}

func (f *Foreground) enter() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fenced {
		return false
	}
	f.inFlight++
	return true
}

func (f *Foreground) leave(s *Shell, p tool.Process) (*liveCommand, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.inFlight--
	var entry *liveCommand
	if p != nil {
		entry = &liveCommand{shell: s, p: p}
		f.live[entry] = struct{}{}
	}
	f.notifyLocked()
	return entry, f.fenced
}

func (f *Foreground) remove(entry *liveCommand) {
	if entry == nil {
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.live, entry)
}

func (f *Foreground) snapshotLocked() []*liveCommand {
	out := make([]*liveCommand, 0, len(f.live))
	for entry := range f.live {
		out = append(out, entry)
	}
	return out
}

func (f *Foreground) KillAll(now bool) int {
	if !now {
		f.mu.Lock()
		live := f.snapshotLocked()
		f.mu.Unlock()
		for _, entry := range live {
			entry.shell.killProcess(entry.p)
		}
		return len(live)
	}
	deadline := time.Now().Add(hardKillBudget)
	f.mu.Lock()
	f.fenced = true
	for f.inFlight > 0 {
		wait := f.changed
		f.mu.Unlock()
		remaining := time.Until(deadline)
		if remaining <= 0 {
			f.mu.Lock()
			break
		}
		select {
		case <-wait:
		case <-time.After(remaining):
		}
		f.mu.Lock()
	}
	live := f.snapshotLocked()
	f.mu.Unlock()
	var remote sync.WaitGroup
	for _, entry := range live {
		if entry.p.PID() > 0 {
			entry.shell.forceKillProcess(entry.p)
			continue
		}
		remote.Add(1)
		go func(entry *liveCommand) {
			defer remote.Done()
			entry.shell.forceKillProcess(entry.p)
		}(entry)
	}
	joined := make(chan struct{})
	go func() {
		remote.Wait()
		close(joined)
	}()
	select {
	case <-joined:
	case <-time.After(max(0, time.Until(deadline))):
	}
	return len(live)
}
