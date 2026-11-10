package gateway

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pydict"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

// Maintenance purpose a browse schema catch up holder publishes
const browseReconcilePurpose = "browse_schema_reconcile"

// A profile home whose gateway.lock another holder owns
type OwnershipConflict struct {
	Msg       string
	Cancelled bool
	Cause     error
}

func (e *OwnershipConflict) Error() string { return e.Msg }

func (e *OwnershipConflict) Unwrap() error { return e.Cause }

// Canonical absolute home path with host case rules, the key every reservation uses
func CanonicalHome(h string) (string, error) {
	resolved, err := pypath.Realpath(pypath.Expanduser(h), pypath.Lax)
	if err != nil {
		return "", err
	}
	return pypath.Normcase(resolved), nil
}

// Lock open flags, never following a symlink planted at the lock path
var lockFlags = os.O_RDWR | os.O_CREATE | oNoFollow

// Opens path and validates the descriptor, closing it when the lock is unsafe
func openChecked(path string, flags int) (*os.File, error) {
	f, err := os.OpenFile(path, flags, 0o600)
	if err != nil {
		return nil, err
	}
	if err := tightenLockMode(f); err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}

// Recovery text for a foreign gateway.lock this user cannot read
func ForeignLockRecovery(path string) string {
	return fmt.Sprintf("foreign_stale_lock: %s is not readable by this user (left by a gateway run as root?) "+
		"and cannot be proven unheld. If `sudo lsof %s` lists no process, run `sudo rm %s`.", path, path, path)
}

// Recovery a client prints for a gateway.lock it cannot open read only, false when it can
func UnreadableLockRecovery(path string) (string, bool) {
	f, err := os.OpenFile(path, os.O_RDONLY|oNoFollow|oNonblock, 0)
	if err == nil {
		f.Close()
		return "", false
	}
	if errors.Is(err, fs.ErrPermission) {
		return ForeignLockRecovery(path), true
	}
	return "", false
}

// Descriptor for path, replacing a provably stale inode this user cannot use once
func openLock(path string) (*os.File, error) {
	f, err := openChecked(path, lockFlags)
	if err == nil {
		return f, nil
	}
	if !errors.Is(err, fs.ErrPermission) {
		return nil, err
	}
	unlinked, uerr := unlinkStaleLock(path)
	if uerr != nil {
		return nil, uerr
	}
	if !unlinked {
		return nil, err
	}
	return openChecked(path, lockFlags)
}

// Reservations of canonical profile homes held through open locked descriptors
type ProfileOwnership struct {
	mu      sync.Mutex
	handles *pydict.Dict[string, *os.File]
	writers []*ownershipWriter
}

type ownershipWriter struct{ done chan struct{} }

func (w *ownershipWriter) alive() bool {
	select {
	case <-w.done:
		return false
	default:
		return true
	}
}

// Empty ownership with no reservations
func NewProfileOwnership() *ProfileOwnership {
	return &ProfileOwnership{handles: pydict.New[string, *os.File]()}
}

// Process wide reservations of the running gateway
var ProcessOwnership = NewProfileOwnership()

// Homes reserved in reservation order
func (o *ProfileOwnership) Homes() []string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.handles.Keys()
}

// Reserves every home all or nothing, purpose marking a maintenance holder in the lock record
func (o *ProfileOwnership) Reserve(ctx context.Context, homes []string, purpose string) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	set := map[string]bool{}
	for _, p := range homes {
		c, err := CanonicalHome(p)
		if err != nil {
			return err
		}
		set[c] = true
	}
	canon := make([]string, 0, len(set))
	for c := range set {
		canon = append(canon, c)
	}
	sort.Strings(canon)
	var added []string
	for _, h := range canon {
		if o.handles.Has(h) {
			continue
		}
		if err := o.reserveOne(ctx, h, purpose); err != nil {
			for i := len(added) - 1; i >= 0; i-- {
				o.releaseLocked(added[i])
			}
			return err
		}
		added = append(added, h)
	}
	return nil
}

func (o *ProfileOwnership) reserveOne(ctx context.Context, h, purpose string) error {
	if _, err := home.MakeUserDir(ctx, h, 0o700, true, true); err != nil {
		return err
	}
	path := pypath.PathJoin(h, gatewayLockFilename)
	f, err := openLock(path)
	if err != nil {
		return err
	}
	if !tryAcquireFileLock(f) || !stillNames(path, f) {
		f.Close()
		return &OwnershipConflict{Msg: "Gateway runtime already owns profile " + h}
	}
	record := pyjson.NewObject()
	base := statusBuildPidRecord()
	for _, k := range base.Keys {
		record.Set(k, recGet(base, k))
	}
	record.Set("hermes_home", h)
	if purpose != "" {
		record.Set("purpose", purpose)
	}
	text, err := pyjson.Dumps(record)
	if err == nil {
		err = rewriteLockRecord(f, text)
	}
	if err != nil {
		f.Close()
		return err
	}
	o.handles.Set(h, f)
	return nil
}

// Replaces the lock file content with text and syncs it to disk
func rewriteLockRecord(f *os.File, text string) error {
	if _, err := f.Seek(0, 0); err != nil {
		return err
	}
	if err := f.Truncate(0); err != nil {
		return err
	}
	if _, err := f.WriteString(text); err != nil {
		return err
	}
	return f.Sync()
}

// Whether home is reserved here
func (o *ProfileOwnership) Owns(h string) bool {
	c, err := CanonicalHome(h)
	if err != nil {
		return false
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.handles.Has(c)
}

// Releases the reservation of home, its lock file left in place
func (o *ProfileOwnership) Release(h string) {
	c, err := CanonicalHome(h)
	if err != nil {
		return
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	o.releaseLocked(c)
}

func (o *ProfileOwnership) releaseLocked(c string) {
	f, ok := o.handles.Get(c)
	if !ok {
		return
	}
	o.handles.Delete(c)
	releaseFileLock(f)
	f.Close()
}

// Registers then starts a writer so a partial startup cannot forget a live one
func (o *ProfileOwnership) StartWriter(run func()) {
	o.mu.Lock()
	defer o.mu.Unlock()
	w := &ownershipWriter{done: make(chan struct{})}
	o.writers = append(o.writers, w)
	go func() {
		defer close(w.done)
		run()
	}()
}

// Releases every reservation unless a registered writer is still running
func (o *ProfileOwnership) Close() {
	o.mu.Lock()
	defer o.mu.Unlock()
	for _, w := range o.writers {
		if w.alive() {
			return
		}
	}
	o.writers = nil
	keys := o.handles.Keys()
	for i := len(keys) - 1; i >= 0; i-- {
		o.releaseLocked(keys[i])
	}
}

// Live holder record of home's gateway.lock when it is a maintenance reservation for purpose
func MaintenanceHolder(h, purpose string) *pyjson.Object {
	c, err := CanonicalHome(h)
	if err != nil {
		return nil
	}
	record := readGatewayLockRecord(pypath.PathJoin(c, gatewayLockFilename))
	if record == nil || record.Len() == 0 || !pyEq(recGet(record, "purpose"), purpose) {
		return nil
	}
	if _, live := liveFromRecord(record); !live {
		return nil
	}
	return record
}

// Whether home's lock record names no live holder other than the caller yet
func LockRecordUnpublished(h string) bool {
	c, err := CanonicalHome(h)
	if err != nil {
		return false
	}
	record := readGatewayLockRecord(pypath.PathJoin(c, gatewayLockFilename))
	if record == nil || record.Len() == 0 {
		return true
	}
	pid, live := liveFromRecord(record)
	return !live || pid == statusGetpid()
}

const (
	probeRetryBackoff    = 15 * time.Millisecond
	transientHolderPoll  = 100 * time.Millisecond
	browseReconcileLabel = "Profile %s is held by a short session-store schema catch-up; waiting up to %.0fs"
)

// Homes another holder has right now, each probed alone and released at once
func conflictingHomes(ctx context.Context, homes []string) []string {
	var out []string
	for _, h := range homes {
		probe := NewProfileOwnership()
		var conflict *OwnershipConflict
		if err := probe.Reserve(ctx, []string{h}, ""); errors.As(err, &conflict) {
			out = append(out, h)
		}
		probe.Close()
	}
	return out
}

// Reserves homes, retrying while every contender is transient, until wait passes or cancel closes
func ReserveWaitingOutTransientHolders(ctx context.Context, ownership *ProfileOwnership, homes []string, wait time.Duration, cancel <-chan struct{}) error {
	if cancel == nil {
		cancel = make(chan struct{})
	}
	cancelled := func() bool {
		select {
		case <-cancel:
			return true
		default:
			return false
		}
	}
	pause := func(d time.Duration) {
		select {
		case <-cancel:
		case <-time.After(d):
		}
	}
	deadline := time.Now().Add(wait)
	logged := false
	for !cancelled() {
		before := map[string]bool{}
		for _, h := range ownership.Homes() {
			before[h] = true
		}
		err := ownership.Reserve(ctx, homes, "")
		var conflict *OwnershipConflict
		if errors.As(err, &conflict) {
			conflicting := conflictingHomes(ctx, homes)
			var holders []string
			for _, h := range conflicting {
				if MaintenanceHolder(h, browseReconcilePurpose) != nil || LockRecordUnpublished(h) {
					holders = append(holders, h)
				}
			}
			if len(holders) < len(conflicting) || !time.Now().Before(deadline) {
				return err
			}
			if len(conflicting) == 0 {
				pause(probeRetryBackoff)
				continue
			}
			if !logged {
				slog.Info(fmt.Sprintf(browseReconcileLabel, holders[0], wait.Seconds()))
				logged = true
			}
			pause(transientHolderPoll)
			continue
		}
		if err != nil {
			return err
		}
		if !cancelled() {
			return nil
		}
		for _, p := range homes {
			if c, cerr := CanonicalHome(p); cerr == nil && !before[c] {
				ownership.Release(c)
			}
		}
	}
	return &OwnershipConflict{Msg: "Reservation of " + strings.Join(homes, ", ") + " cancelled", Cancelled: true}
}

// Pid of a live runtime owner of home, a gateway or a reservation not marked as maintenance
func LiveRuntimeOwner(h string) (int, bool, error) {
	c, err := CanonicalHome(h)
	if err != nil {
		return 0, false, err
	}
	record := readGatewayLockRecord(pypath.PathJoin(c, gatewayLockFilename))
	livePid, live := 0, false
	if record != nil && record.Len() > 0 {
		livePid, live = liveFromRecord(record)
	}
	if live && pyrepr.Truthy(recGet(record, "purpose")) {
		return 0, false, nil
	}
	pid, ok, err := GetRunningPid(pypath.PathJoin(c, "gateway.pid"), false)
	if err != nil {
		return 0, false, err
	}
	if ok && pid != 0 {
		return pid, true, nil
	}
	return livePid, live, nil
}

type maintenanceKey struct{}

type maintenanceState struct {
	pid   int
	owner *ProfileOwnership
}

// Reserves the homes' exact locks for maintenance, nested calls under the returned context reusing the reservation
func ExclusiveMaintenance(ctx context.Context, homes []string, purpose string) (context.Context, func(), error) {
	state, _ := ctx.Value(maintenanceKey{}).(*maintenanceState)
	outermost := state == nil || state.pid != statusGetpid()
	owner := NewProfileOwnership()
	if !outermost {
		owner = state.owner
	}
	previous := map[string]bool{}
	for _, h := range owner.Homes() {
		previous[h] = true
	}
	releaseNew := func() {
		homesNow := owner.Homes()
		for i := len(homesNow) - 1; i >= 0; i-- {
			if !previous[homesNow[i]] {
				owner.Release(homesNow[i])
			}
		}
	}
	if err := owner.Reserve(ctx, homes, purpose); err != nil {
		releaseNew()
		var conflict *OwnershipConflict
		if errors.As(err, &conflict) {
			return ctx, nil, &OwnershipConflict{Msg: "Exclusive maintenance refused: " + conflict.Msg + ". Drain and stop the gateway, then retry.", Cause: err}
		}
		return ctx, nil, err
	}
	inner := ctx
	if outermost {
		inner = context.WithValue(ctx, maintenanceKey{}, &maintenanceState{statusGetpid(), owner})
	}
	var once sync.Once
	return inner, func() { once.Do(releaseNew) }, nil
}
