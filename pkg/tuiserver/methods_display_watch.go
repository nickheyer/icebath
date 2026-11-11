package tuiserver

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/botdesktop"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/tools"
	"github.com/nickheyer/icebath/pkg/tuigateway"
)

const (
	leasePollInterval = 500 * time.Millisecond
	idleCheckInterval = 30 * time.Second
)

// Modification time in nanoseconds, none set for a file that cannot be stat'ed
type fileMtime struct {
	none bool
	ns   int64
}

// Screen identity of one home, the env and launcher.pid mtimes plus launcher liveness
type runtimeMark struct {
	env, launcher fileMtime
	alive         bool
}

var (
	leaseWatcherStarted atomic.Bool
	leaseWatchPassMu    sync.Mutex
	leaseWatchMu        sync.Mutex
	leaseEpochs         = map[string]any{}
	leaseMtimes         = map[string]fileMtime{}
	runtimeMarks        = map[string]runtimeMark{}
	lastIdleCheck       time.Time
)

// Payload of a display.lease event, the public view the in process broadcast carries
func leaseEventPayload(profileKey string, lease botdesktop.BotDesktopLease) map[string]any {
	return map[string]any{"profile_key": profileKey, "lease": botdesktop.LeasePublicView(lease)}
}

// Launch home followed by every served profile home in a stable order
func watchedLeaseHomes() []string {
	servedProfileMu.Lock()
	defer servedProfileMu.Unlock()
	served := make([]string, 0, len(servedProfileHomes))
	for h := range servedProfileHomes {
		served = append(served, h)
	}
	slices.Sort(served)
	return append([]string{hermesHome}, served...)
}

// Modification time of a path, none when it cannot be stat'ed
func mtime(path string) fileMtime {
	info, err := os.Stat(path)
	if err != nil {
		return fileMtime{none: true}
	}
	return fileMtime{ns: info.ModTime().UnixNano()}
}

// Stages the Bot Screen line as a per turn note on every live agent session of the profile
func notifyAgentsOfScreenChange(profileKey string, payload *pyjson.Object) {
	line, built := func() (line string, built bool) {
		defer func() {
			if recover() != nil {
				line, built = "", false
			}
		}()
		holder := ""
		if lease, isDict := payload.GetOr("lease", nil).(*pyjson.Object); isDict {
			holder = strOr(lease.GetOr("holder", nil))
		}
		display, _ := payload.GetOr("display", nil).(string)
		return agent.BotScreenNote(pyrepr.Truthy(payload.GetOr("running", nil)), display, holder), true
	}()
	if !built || line == "" {
		return
	}
	var targets []*agent.AIAgent
	for _, live := range tuigateway.LiveSessions() {
		a := sessionAgent(live.Session)
		if a == nil {
			continue
		}
		owner := strOr(live.Session.Get("profile_home"))
		if owner == "" {
			owner = home.Process()
		}
		if home.Key(owner) == profileKey {
			targets = append(targets, a)
		}
	}
	for _, a := range targets {
		if prior := a.GatewayTurnContextNotes(); prior != "" {
			a.SetGatewayTurnContextNotes(prior + "\n\n" + line)
		} else {
			a.SetGatewayTurnContextNotes(line)
		}
	}
}

// Shallow map of an ordered snapshot for a global event payload
func objectPayload(o *pyjson.Object) map[string]any {
	out := make(map[string]any, o.Len())
	for _, k := range o.Keys {
		out[k] = o.Vals[k]
	}
	return out
}

// Broadcasts display.status when a home's screen started or stopped outside this process or its launcher died
func pollRuntimeFiles() error {
	leaseWatchPassMu.Lock()
	defer leaseWatchPassMu.Unlock()
	for _, h := range watchedLeaseHomes() {
		key := home.Key(h)
		ctx := home.WithOverride(context.Background(), h)
		env := tools.EnvForProfileHome(ctx, nil, h)
		sd := filepath.Join(h, "bot-desktop")
		_, alive := botdesktop.LauncherPid(env)
		mark := runtimeMark{env: mtime(filepath.Join(sd, "env")), launcher: mtime(filepath.Join(sd, "launcher.pid")), alive: alive}
		leaseWatchMu.Lock()
		prev, seen := runtimeMarks[key]
		if seen && prev == mark {
			leaseWatchMu.Unlock()
			continue
		}
		runtimeMarks[key] = mark
		leaseWatchMu.Unlock()
		if !seen {
			continue
		}
		payload, err := displaySnapshot(ctx)
		if err != nil {
			return err
		}
		broadcastGlobalEvent("display.status", objectPayload(payload))
		notifyAgentsOfScreenChange(key, payload)
	}
	return nil
}

// Stops screens idle past their limit, at most once per idle check interval
func pollIdleScreens() error {
	leaseWatchMu.Lock()
	if !lastIdleCheck.IsZero() && time.Since(lastIdleCheck) < idleCheckInterval {
		leaseWatchMu.Unlock()
		return nil
	}
	lastIdleCheck = time.Now()
	leaseWatchMu.Unlock()
	for _, h := range watchedLeaseHomes() {
		ctx := home.WithOverride(context.Background(), h)
		if _, err := botdesktop.StopIfIdle(tools.EnvForProfileHome(ctx, nil, h), nil); err != nil {
			return err
		}
	}
	return nil
}

// Reads a home's lease only when its file moved and broadcasts when its epoch did
func pollLeaseFiles() {
	leaseWatchPassMu.Lock()
	defer leaseWatchPassMu.Unlock()
	for _, h := range watchedLeaseHomes() {
		key := home.Key(h)
		moved := mtime(filepath.Join(h, "bot-desktop", "lease.json"))
		leaseWatchMu.Lock()
		if moved == leaseMtimes[key] {
			leaseWatchMu.Unlock()
			continue
		}
		leaseMtimes[key] = moved
		leaseWatchMu.Unlock()
		lease := botdesktop.LeaseGet(h)
		leaseWatchMu.Lock()
		seen, known := leaseEpochs[key]
		leaseEpochs[key] = lease.Epoch
		leaseWatchMu.Unlock()
		if !known || pyrepr.Equal(seen, lease.Epoch) {
			continue
		}
		broadcastGlobalEvent("display.lease", leaseEventPayload(key, lease))
	}
}

// Starts the lease file poll once per process and marks in process transitions seen so they broadcast once
func ensureLeaseWatcher() {
	if !leaseWatcherStarted.CompareAndSwap(false, true) {
		return
	}
	botdesktop.LeaseOnChange(func(profileKey string, lease botdesktop.BotDesktopLease) {
		if leaseListenerInstalled.Load() {
			leaseWatchMu.Lock()
			leaseEpochs[profileKey] = lease.Epoch
			leaseWatchMu.Unlock()
		}
	})
	go func() {
		for {
			if err := leaseWatchPass(); err != nil {
				logger().Debug(fmt.Sprintf("lease watcher poll failed: %s", err))
			}
			time.Sleep(leasePollInterval)
		}
	}()
}

// One watcher tick over the lease files, idle screens and runtime files, a panic surfacing as its failure
func leaseWatchPass() (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("%s", panicMessage(r))
		}
	}()
	pollLeaseFiles()
	if err := pollIdleScreens(); err != nil {
		return err
	}
	return pollRuntimeFiles()
}
