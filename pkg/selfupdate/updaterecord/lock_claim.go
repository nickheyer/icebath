package updaterecord

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/proc"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

const (
	OwnPIDTmpStaleSeconds         = 60.0
	MutexSuffix                   = ".lock"
	MutexWaitSeconds              = 10.0
	CheckoutContentionWaitSeconds = 3.0
	CreateSuspended               = 0x00000004
	maxAncestryDepth              = 128
	windowsLockOffset             = 1 << 20
	leaseSlots                    = 16
)

type UpdateHolder struct {
	PID        int
	AgeSeconds float64
	Reason     string
	Held       bool
}

type MarkerBusy struct{ Path string }

func (e *MarkerBusy) Error() string { return e.Path + " is held by another update" }

// Reports whether err stands for a Python OSError, MarkerBusy and the pause record failures included
func isOSError(err error) bool {
	var busy *MarkerBusy
	var pause *PauseRecordError
	return errors.As(err, &busy) || errors.As(err, &pause) || pypath.IsOSError(err)
}

var runtimeHostMatcher atomic.Pointer[func([]string) bool]

// Installs the command line test naming a long lived icebath host, called by pkg/selfupdate
func SetRuntimeHostMatcher(match func([]string) bool) {
	runtimeHostMatcher.Store(&match)
}

var (
	parentOf    = psutilParent
	cmdlineOf   = proc.Cmdline
	linkMarker  = os.Link
	writeMarker = (*os.File).Write
	sharedProbe = trySharedLock
	pausePoint  atomic.Pointer[func(string)]
)

// Runs the probe hook installed for a named step of the marker protocol
func pauseAt(point string) {
	if hook := pausePoint.Load(); hook != nil {
		(*hook)(point)
	}
}

// Pid of the orchestrating updater named by the handoff variable, zero when absent or malformed
func handoffPID() int {
	value, ok := pyint.Parse(pystr.Strip(os.Getenv(HandoffPIDEnv)))
	if !ok || value.Sign() <= 0 || !value.IsInt64() || value.Int64() > math.MaxInt32 {
		return 0
	}
	return int(value.Int64())
}

// Parent of pid as psutil reports it, absent when it has none, is gone or started after the child
func psutilParent(pid int) (int, bool, error) {
	ppid, ok := proc.PPID(pid)
	if !ok || ppid <= 0 {
		return 0, false, nil
	}
	childBirth, err := proc.Birth(pid)
	if err != nil {
		return 0, false, err
	}
	parentBirth, err := proc.Birth(ppid)
	if err != nil {
		if !proc.Exists(ppid) {
			return 0, false, nil
		}
		return 0, false, err
	}
	if parentBirth <= childBirth {
		return ppid, true, nil
	}
	return 0, false, nil
}

// PsutilParent exposes the psutil parent of pid to the gateway orphan reaper
func PsutilParent(pid int) (int, bool, error) { return psutilParent(pid) }

// Reports whether pid is a live ancestor of this process, walking one link at a time
func isAncestorPID(pid int) bool {
	if pid <= 0 {
		return false
	}
	if pid == os.Getppid() {
		return true
	}
	current := os.Getpid()
	seen := map[int]bool{current: true}
	for range maxAncestryDepth {
		parent, ok, err := parentOf(current)
		if err != nil {
			slog.Debug("Could not walk process ancestry", "pid", pid, "error", err)
			return false
		}
		if !ok {
			return false
		}
		if parent == pid {
			return true
		}
		if seen[parent] {
			return false
		}
		seen[parent] = true
		current = parent
	}
	slog.Debug("Gave up walking process ancestry", "pid", pid, "links", maxAncestryDepth)
	return false
}

// Reports whether a gateway, serve or dashboard host sits between this process and holderPID
func runtimeHostBelow(holderPID int) bool {
	match := runtimeHostMatcher.Load()
	if match == nil {
		return false
	}
	current, ok, err := parentOf(os.Getpid())
	if err != nil {
		return false
	}
	for range maxAncestryDepth {
		if !ok || current == holderPID {
			return false
		}
		if cmdline, err := cmdlineOf(current); err == nil && (*match)(cmdline) {
			return true
		}
		if current, ok, err = parentOf(current); err != nil {
			return false
		}
	}
	return false
}

// Private sibling of path named for this pid and a random token
func tmpSibling(path string) string {
	token := make([]byte, 4)
	_, _ = rand.Read(token)
	return fmt.Sprintf("%s.%d.%s.tmp", path, os.Getpid(), hex.EncodeToString(token))
}

// Reports whether the writer of a tmp sibling is gone, an old file under our own pid counting as gone
func tmpWriterGone(owner *uint64, entry string) (bool, error) {
	if owner == nil {
		return true, nil
	}
	if int(*owner) != os.Getpid() {
		return !aliveOf(int(*owner)), nil
	}
	info, err := os.Stat(entry)
	if err != nil {
		return false, err
	}
	return float64(time.Now().UnixNano())/1e9-float64(info.ModTime().UnixNano())/1e9 > OwnPIDTmpStaleSeconds, nil
}

// Removes marker tmp siblings whose writer died between write and publish
func sweepDeadTmpSiblings(path string) {
	dir, name := filepath.Dir(path), filepath.Base(path)
	prefix := name + "."
	names, err := readDirNames(dir)
	if err != nil {
		return
	}
	for _, entry := range names {
		if len(entry) < len(prefix) || entry[:len(prefix)] != prefix || filepath.Ext(entry) != ".tmp" {
			continue
		}
		owner, _, _ := cutDot(entry[len(prefix):])
		if !intLineRE.MatchString(owner) {
			continue
		}
		var bounded *uint64
		if value, ok := boundedInt(owner, u32Max); ok {
			bounded = &value
		}
		full := filepath.Join(dir, entry)
		if gone, err := tmpWriterGone(bounded, full); err == nil && gone {
			_ = os.Remove(full)
		}
	}
}

func cutDot(text string) (string, string, bool) {
	for i := 0; i < len(text); i++ {
		if text[i] == '.' {
			return text[:i], text[i+1:], true
		}
	}
	return text, "", false
}

// Entry names of dir in directory order, as os.listdir returns them
func readDirNames(dir string) ([]string, error) {
	f, err := os.Open(dir)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return f.Readdirnames(-1)
}

// Sidecar every marker mutation holds a kernel lock on, never deleted
func MarkerMutexPath(path string) string { return path + MutexSuffix }

type mutexHeldKey struct{}

// Holds the marker sidecar lock, reentrant along ctx, MarkerBusy after wait
func MarkerMutex(ctx context.Context, path string, wait time.Duration) (context.Context, func(), error) {
	key := MarkerMutexPath(path)
	held, _ := ctx.Value(mutexHeldKey{}).(map[string]bool)
	if held[key] {
		return ctx, func() {}, nil
	}
	release, err := mutexAcquire(key, wait)
	if err != nil {
		return ctx, nil, err
	}
	next := make(map[string]bool, len(held)+1)
	for k := range held {
		next[k] = true
	}
	next[key] = true
	return context.WithValue(ctx, mutexHeldKey{}, next), release, nil
}

// Replaces path with new through a fsynced tmp only while it still holds expected
func compareAndSwap(ctx context.Context, path string, expected, replacement []byte) (bool, error) {
	_, release, err := MarkerMutex(ctx, path, MutexWaitSeconds*time.Second)
	if err != nil {
		return false, err
	}
	defer release()
	if raw := readMarkerBytes(path); raw == nil || !bytes.Equal(raw, expected) {
		return false, nil
	}
	tmp := tmpSibling(path)
	defer os.Remove(tmp)
	if err := writeSynced(tmp, replacement, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o666); err != nil {
		slog.Debug("Could not rewrite update marker", "path", path, "error", err)
		return false, nil
	}
	if err := os.Rename(tmp, path); err != nil {
		slog.Debug("Could not rewrite update marker", "path", path, "error", err)
		return false, nil
	}
	return true, nil
}

// Writes data to a new file opened with flag and fsyncs it before closing
func writeSynced(path string, data []byte, flag int, perm os.FileMode) error {
	f, err := os.OpenFile(path, flag, perm)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// Live identities behind a marker, owner then delegate, zero alone for a claim still being written
func livePartners(m marker, w World) []int {
	if m.inFlight {
		return []int{0}
	}
	var partners []int
	if m.ownerLive(w) {
		partners = append(partners, m.pid)
	}
	if m.delegateLive(w) {
		partners = append(partners, *m.delegatePID)
	}
	return partners
}

// Age of a marker for a holder, zero when its start is absent or zero
func holderAge(m marker, w World, clamp bool) float64 {
	if m.startedAt == nil || *m.startedAt == 0 {
		return 0
	}
	if clamp {
		return math.Max(m.age(w), 0)
	}
	return m.age(w)
}

// Live update holding the marker, a held holder while the checkout lock outlives a dead claim, nil when clear
func ReadLiveUpdate(ctx context.Context, path, installRoot string) (holder *UpdateHolder) {
	if path == "" {
		path = UpdateMarkerPath()
	}
	defer func() {
		if failure := recover(); failure != nil {
			slog.Debug("Could not judge update marker", "path", path, "error", failure)
			holder = nil
		}
	}()
	for range 2 {
		parsed := markerReader(path)
		if parsed == nil {
			return nil
		}
		w := realWorld()
		if live := parsed.livePID(w); live != nil {
			age := 0.0
			if parsed.startedAt != nil {
				age = parsed.age(w)
			}
			return &UpdateHolder{PID: *live, AgeSeconds: age}
		}
		verdict, err := reclaimDead(ctx, path, installRoot)
		if err != nil {
			slog.Debug("Could not judge update marker", "path", path, "error", err)
			return nil
		}
		if verdict == "held" {
			age := 0.0
			if parsed.startedAt != nil {
				age = math.Max(parsed.age(realWorld()), 0)
			}
			return &UpdateHolder{Held: true, AgeSeconds: age}
		}
		if verdict != "live" {
			break
		}
	}
	return nil
}

// Removes a dead marker under the mutex, answering reclaimed, held, live, absent or busy
func reclaimDead(ctx context.Context, path, installRoot string) (string, error) {
	pauseAt("reclaim")
	ctx, release, err := MarkerMutex(ctx, path, MutexWaitSeconds*time.Second)
	if err != nil {
		slog.Debug("Left update marker for its current mutator", "path", path, "error", err)
		return "busy", nil
	}
	defer release()
	current := markerReader(path)
	if current == nil {
		return "absent", nil
	}
	if current.livePID(realWorld()) != nil {
		return "live", nil
	}
	held, err := CheckoutLockHeld(ctx, installRoot)
	if err != nil {
		return "", err
	}
	if held {
		return "held", nil
	}
	pauseAt("unlink")
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		slog.Debug("Left update marker for its current mutator", "path", path, "error", err)
		return "busy", nil
	}
	return "reclaimed", nil
}

// Live claims in pre root marker files under root/profiles, read only and in directory order
func LegacyProfileClaims(root string) []UpdateHolder {
	if root == "" {
		root = filepath.Dir(UpdateMarkerPath())
	}
	var claims []UpdateHolder
	names, err := readDirNames(filepath.Join(root, "profiles"))
	if err != nil {
		return nil
	}
	for _, profile := range names {
		m := markerReader(filepath.Join(root, "profiles", profile, MarkerName))
		if m == nil || m.inFlight {
			continue
		}
		w := realWorld()
		if live := m.livePID(w); live != nil && *live != 0 {
			claims = append(claims, UpdateHolder{PID: *live, AgeSeconds: m.age(w)})
		}
	}
	return claims
}

// User facing explanation of who holds the update lock or why it could not be taken
func DescribeHolder(holder *UpdateHolder) string {
	if holder != nil && holder.Reason != "" {
		return "✗ Cannot lock this install for the update: " + holder.Reason + ".\n" +
			"\n" +
			"  Updating without the lock could let two updates corrupt the install, or run\n" +
			"  under a Desktop app, gateway or installer that cannot see it. Make that path\n" +
			"  writable (fix its owner or permissions, or remount a read-only filesystem\n" +
			"  read-write), or run `icebath update` as the user that owns the install."
	}
	age := 0.0
	if holder != nil {
		age = holder.AgeSeconds
	}
	total := int64(math.Max(age, 0))
	minutes, seconds := total/60, total%60
	elapsed := fmt.Sprintf("%ds", seconds)
	if minutes != 0 {
		elapsed = fmt.Sprintf("%dm %ds", minutes, seconds)
	}
	who := ""
	if holder != nil && holder.PID != 0 {
		who = fmt.Sprintf(", process %d", holder.PID)
	}
	if holder != nil && holder.Held {
		who = "; its owner exited but a process it started still holds the checkout"
	}
	return "✗ Another icebath update is already running (started " + elapsed + " ago" + who + ").\n" +
		"\n" +
		"  Running two at once would corrupt the install. Wait for it to finish\n" +
		"  (watch `icebath logs`), or close the Desktop/dashboard window that\n" +
		"  started it, then run `icebath update` again."
}

type heldCheckout struct {
	path        string
	file        *os.File
	owned       bool
	depth       int
	leaseFile   *os.File
	leaseOffset int64
}

var checkout struct {
	sync.Mutex
	held      *heldCheckout
	inherited map[int]*os.File
}

// Shared file for an inherited descriptor, created once so no finalizer ever closes the tree's fd
func inheritedFile(fd int, path string) *os.File {
	if checkout.inherited == nil {
		checkout.inherited = map[int]*os.File{}
	}
	if f, ok := checkout.inherited[fd]; ok {
		return f
	}
	f := os.NewFile(uintptr(fd), path)
	checkout.inherited[fd] = f
	return f
}

// Who holds the checkout lock from its holder record, held when the recorded updater is gone
func lockHolder(path string) UpdateHolder {
	raw := readMarkerBytes(path)
	if raw == nil {
		raw = []byte{}
	}
	parsed := parseMarker(raw, nil)
	w := realWorld()
	age := holderAge(parsed, w, false)
	if parsed.pid > 0 {
		if live := incarnation(parsed.pid, parsed.createTime, w); live != nil && !*live {
			return UpdateHolder{AgeSeconds: age, Held: true}
		}
	}
	return UpdateHolder{PID: max(parsed.pid, 0), AgeSeconds: age}
}

// C strerror of an OSError, its whole text when it carries no errno
func strerrorOf(err error) string {
	var errno syscall.Errno
	if errors.As(err, &errno) {
		return pypath.Strerror(errno)
	}
	return pypath.OSErrorText(err)
}

// Takes or joins the checkout lock, the refusal holder when another tree holds it or it cannot be made
func acquireCheckout(installRoot string) (*UpdateHolder, error) {
	path, err := CheckoutLockPath(installRoot)
	if err != nil {
		return nil, err
	}
	checkout.Lock()
	defer checkout.Unlock()
	if checkout.held != nil && checkout.held.path == path {
		checkout.held.depth++
		return nil, nil
	}
	if fd, ok := inheritedLockFD(path); ok {
		checkout.held = &heldCheckout{path: path, file: inheritedFile(fd, path), depth: 1}
		return nil, nil
	}
	f, writable, reason := openLockFile(path)
	if f == nil {
		return &UpdateHolder{Reason: fmt.Sprintf("%s is not writable (%s); the update locks the checkout there, in its git directory, which git must write too", path, reason)}, nil
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	links, err := linkCount(f)
	if err != nil {
		f.Close()
		return nil, err
	}
	if !info.Mode().IsRegular() || links != 1 {
		f.Close()
		return &UpdateHolder{Reason: path + " is not a regular single-link file; delete it"}, nil
	}
	refuse := func(err error) (*UpdateHolder, error) {
		unlockBytes(f, windowsLockOffset, 1)
		f.Close()
		return &UpdateHolder{Reason: fmt.Sprintf("%s could not be locked (%s)", path, pypath.OSErrorText(err))}, nil
	}
	locked, err := lockWithContentionWait(f, path)
	if err != nil {
		return refuse(err)
	}
	if !locked {
		if heldByOurWindowsAncestor(path) {
			lease, leased := takeLease(f)
			if heldByOurWindowsAncestor(path) {
				held := &heldCheckout{path: path, depth: 1}
				if leased {
					held.leaseFile, held.leaseOffset = f, lease
				} else {
					f.Close()
				}
				checkout.held = held
				return nil, nil
			}
			if leased {
				unlockBytes(f, lease, 1)
			}
		}
		f.Close()
		holder := lockHolder(path)
		return &holder, nil
	}
	if !writable && runtime.GOOS == "windows" {
		unlockBytes(f, windowsLockOffset, 1)
		f.Close()
		return &UpdateHolder{Reason: path + " is read-only; delete it or fix its permissions"}, nil
	}
	if writable {
		record := fmt.Sprintf("%d\n%d\n%s\n", os.Getpid(), time.Now().Unix(), IdentityLine(os.Getpid()))
		if _, err := f.Seek(0, 0); err != nil {
			return refuse(err)
		}
		if err := f.Truncate(0); err != nil {
			return refuse(err)
		}
		if _, err := f.Write([]byte(record)); err != nil {
			return refuse(err)
		}
	}
	checkout.held = &heldCheckout{path: path, file: f, owned: true, depth: 1}
	return nil, nil
}

// Opens the lock file read write without following a link, read only when we may not write it
func openLockFile(path string) (*os.File, bool, string) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|noFollow, 0o644)
	if err == nil {
		return f, true, ""
	}
	if !errors.Is(err, fs.ErrPermission) {
		return nil, false, strerrorOf(err)
	}
	denied := err
	if f, err := os.OpenFile(path, os.O_RDONLY|noFollow, 0); err == nil {
		return f, false, ""
	}
	return nil, false, strerrorOf(denied)
}

// Takes the lock, waiting out a momentary held probe unless our own Windows update tree holds it
func lockWithContentionWait(f *os.File, path string) (bool, error) {
	if ok, err := tryLock(f); ok || err != nil {
		return ok, err
	}
	if heldByOurWindowsAncestor(path) {
		return false, nil
	}
	deadline := time.Now().Add(CheckoutContentionWaitSeconds * time.Second)
	for time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
		if ok, err := tryLock(f); ok || err != nil {
			return ok, err
		}
	}
	return false, nil
}

// Reports on Windows whether the lock record names a live incarnation of one of our ancestors
func heldByOurWindowsAncestor(path string) bool {
	if runtime.GOOS != "windows" {
		return false
	}
	raw := readMarkerBytes(path)
	if raw == nil {
		raw = []byte{}
	}
	record := parseMarker(raw, nil)
	if record.startedAt == nil || record.pid == os.Getpid() {
		return false
	}
	live := incarnation(record.pid, record.createTime, realWorld())
	return live != nil && *live && isAncestorPID(record.pid)
}

// Drops one level of this process's checkout hold, closing an owned descriptor without unlocking it
func releaseCheckout() {
	checkout.Lock()
	defer checkout.Unlock()
	held := checkout.held
	if held == nil {
		return
	}
	held.depth--
	if held.depth > 0 {
		return
	}
	checkout.held = nil
	if held.leaseFile != nil {
		unlockBytes(held.leaseFile, held.leaseOffset, 1)
		held.leaseFile.Close()
	}
	if held.owned {
		unlockBytes(held.file, windowsLockOffset, 1)
		held.file.Close()
	}
}

// ReleaseCheckout exports releaseCheckout
func ReleaseCheckout() { releaseCheckout() }

// AcquireCheckout exports acquireCheckout
func AcquireCheckout(installRoot string) (*UpdateHolder, error) { return acquireCheckout(installRoot) }

// Descriptors an update tree child inherits so the checkout stays locked while it runs, POSIX only
func CheckoutLockFiles(installRoot string) ([]*os.File, error) {
	if runtime.GOOS == "windows" {
		return nil, nil
	}
	checkout.Lock()
	defer checkout.Unlock()
	if checkout.held != nil {
		if checkout.held.file == nil {
			return nil, nil
		}
		return []*os.File{checkout.held.file}, nil
	}
	path, err := CheckoutLockPath(installRoot)
	if err != nil {
		return nil, err
	}
	if fd, ok := inheritedLockFD(path); ok {
		return []*os.File{inheritedFile(fd, path)}, nil
	}
	return nil, nil
}

// Extra files that keep a checkout mutator child in the update tree's custody, none without a held lock
func CustodySpawnFiles() []*os.File {
	if runtime.GOOS == "windows" {
		return nil
	}
	checkout.Lock()
	defer checkout.Unlock()
	if checkout.held == nil || checkout.held.file == nil {
		return nil
	}
	return []*os.File{checkout.held.file}
}

// Reports whether this process holds or joined the checkout lock of installRoot
func HoldsCheckoutLock(installRoot string) (bool, error) {
	checkout.Lock()
	held := checkout.held
	checkout.Unlock()
	if held == nil {
		return false, nil
	}
	path, err := CheckoutLockPath(installRoot)
	if err != nil {
		return false, err
	}
	heldReal, _ := pypath.Realpath(held.path, pypath.Lax)
	wantReal, _ := pypath.Realpath(path, pypath.Lax)
	return heldReal == wantReal, nil
}

// Reports whether an update owns the install, by a live marker or a held checkout lock
func UpdateInProgress(ctx context.Context, installRoot string) (bool, error) {
	if ReadLiveUpdate(ctx, "", installRoot) != nil {
		return true, nil
	}
	return CheckoutLockHeld(ctx, installRoot)
}

// Reports whether any process holds the checkout kernel lock, by a shared try that never blocks a holder
func CheckoutLockHeld(ctx context.Context, installRoot string) (bool, error) {
	if holds, err := HoldsCheckoutLock(installRoot); holds || err != nil {
		return holds, err
	}
	path, err := CheckoutLockPath(installRoot)
	if err != nil {
		return false, err
	}
	f, err := os.Open(path)
	if err != nil {
		return !errors.Is(err, fs.ErrNotExist), nil
	}
	defer f.Close()
	free, err := sharedProbe(f)
	if err != nil {
		return false, nil
	}
	return !free, nil
}

// Publishes body at path only when nothing is there, by hard link of a tmp or an exclusive create
func publishExclusive(ctx context.Context, path string, body []byte) (bool, error) {
	tmp := tmpSibling(path)
	if err := writeSynced(tmp, body, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o666); err != nil {
		_ = os.Remove(tmp)
		return false, err
	}
	err := linkMarker(tmp, path)
	_ = os.Remove(tmp)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, fs.ErrExist) {
		return false, nil
	}
	slog.Debug("No hard link for the update marker, exclusive create instead", "error", err)
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		if errors.Is(err, fs.ErrExist) {
			return false, nil
		}
		return false, err
	}
	made, err := f.Stat()
	if err != nil {
		f.Close()
		return false, err
	}
	n, err := writeMarker(f, body)
	if err == nil && n != len(body) {
		err = &os.PathError{Op: "write", Path: path, Err: syscall.ENOSPC}
	}
	if err == nil {
		err = f.Sync()
	}
	if err != nil {
		f.Close()
		if _, release, merr := MarkerMutex(ctx, path, MutexWaitSeconds*time.Second); merr == nil {
			if now, serr := os.Stat(path); serr == nil && os.SameFile(now, made) {
				_ = os.Remove(path)
			}
			release()
		}
		return false, err
	}
	return true, f.Close()
}

// Why an unwritable marker location refuses the update
func markerUnwritable(path string, err error) string {
	return fmt.Sprintf("%s is not writable (%s); the update records itself there so the Desktop app, gateways, installers and other updaters wait for it", path, pypath.OSErrorText(err))
}

var claims struct {
	sync.Mutex
	count map[string]int
}

type UpdateLock struct {
	Path          string
	InstallRoot   string
	CheckoutFirst bool
	Acquired      bool
	Holder        *UpdateHolder
	claimed       bool
	checkout      bool
}

// Update lock over the shared marker, path empty for the root marker, installRoot empty for this install
func NewUpdateLock(path, installRoot string, checkoutFirst bool) *UpdateLock {
	if path == "" {
		path = UpdateMarkerPath()
	}
	return &UpdateLock{Path: path, InstallRoot: installRoot, CheckoutFirst: checkoutFirst}
}

// Takes the checkout lock when asked, then claims or adopts the marker, false with Holder set when refused
func (l *UpdateLock) Acquire(ctx context.Context) (bool, error) {
	if l.InstallRoot != "" && l.CheckoutFirst {
		ok, err := l.AcquireCheckout(l.InstallRoot)
		if !ok || err != nil {
			return false, err
		}
	}
	ok, err := l.claimMarker(ctx)
	if err != nil || !ok {
		l.dropCheckout()
		return false, err
	}
	claims.Lock()
	if claims.count == nil {
		claims.count = map[string]int{}
	}
	claims.count[l.Path]++
	claims.Unlock()
	l.claimed = true
	return true, nil
}

// Takes or joins the checkout lock a caller mutates the install under
func (l *UpdateLock) AcquireCheckout(installRoot string) (bool, error) {
	if l.checkout {
		return true, nil
	}
	refused, err := acquireCheckout(installRoot)
	if err != nil {
		return false, err
	}
	if refused != nil {
		l.Holder = refused
		return false, nil
	}
	l.checkout = true
	return true, nil
}

func (l *UpdateLock) claimMarker(ctx context.Context) (bool, error) {
	dir := filepath.Dir(l.Path)
	if err := os.MkdirAll(dir, 0o777); err != nil {
		l.Holder = &UpdateHolder{Reason: markerUnwritable(dir, err)}
		return false, nil
	}
	sweepDeadTmpSiblings(l.Path)
	claims.Lock()
	_, outer := claims.count[l.Path]
	claims.Unlock()
	if !outer {
		for _, claim := range LegacyProfileClaims(dir) {
			if !isPartner(claim.PID) {
				l.Holder = &claim
				return false, nil
			}
		}
	}
	body := []byte(fmt.Sprintf("%d\n%d\n%s\n", os.Getpid(), time.Now().Unix(), IdentityLine(os.Getpid())))
	done, ok, err := l.publishOrJudge(ctx, body)
	if err != nil {
		var busy *MarkerBusy
		switch {
		case errors.As(err, &busy):
			l.Holder = &UpdateHolder{}
			return false, nil
		case isOSError(err):
			l.Holder = &UpdateHolder{Reason: markerUnwritable(l.Path, err)}
			return false, nil
		}
		return false, err
	}
	if done {
		return ok, nil
	}
	if holder := ReadLiveUpdate(ctx, l.Path, l.InstallRoot); holder != nil {
		l.Holder = holder
	} else {
		l.Holder = &UpdateHolder{}
	}
	return false, nil
}

// Three publish attempts, each judging the claim in the way under the marker mutex
func (l *UpdateLock) publishOrJudge(ctx context.Context, body []byte) (bool, bool, error) {
	for range 3 {
		published, err := publishExclusive(ctx, l.Path, body)
		if err != nil {
			return true, false, err
		}
		if published {
			l.Acquired = true
			return true, true, nil
		}
		done, ok, err := l.judgeExisting(ctx, body)
		if done || err != nil {
			return done, ok, err
		}
	}
	return false, false, nil
}

func (l *UpdateLock) judgeExisting(ctx context.Context, body []byte) (bool, bool, error) {
	ctx, release, err := MarkerMutex(ctx, l.Path, MutexWaitSeconds*time.Second)
	if err != nil {
		return true, false, err
	}
	defer release()
	existing := markerReader(l.Path)
	if existing == nil {
		return false, false, nil
	}
	w := realWorld()
	if len(livePartners(*existing, w)) > 0 {
		ok, err := l.adoptOrRefuse(ctx, *existing)
		return true, ok, err
	}
	elsewhere, err := l.checkoutHeldElsewhere(ctx)
	if err != nil {
		return true, false, err
	}
	if elsewhere {
		age := 0.0
		if existing.startedAt != nil {
			age = math.Max(existing.age(w), 0)
		}
		l.Holder = &UpdateHolder{Held: true, AgeSeconds: age}
		return true, false, nil
	}
	if err := os.Remove(l.Path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return true, false, err
	}
	published, err := publishExclusive(ctx, l.Path, body)
	if err != nil {
		return true, false, err
	}
	if published {
		l.Acquired = true
		return true, true, nil
	}
	return false, false, nil
}

// Reports whether the checkout lock is held by a tree other than this process's own
func (l *UpdateLock) checkoutHeldElsewhere(ctx context.Context) (bool, error) {
	if l.checkout {
		return false, nil
	}
	path, err := CheckoutLockPath(l.InstallRoot)
	if err != nil {
		return false, err
	}
	checkout.Lock()
	ours := checkout.held != nil && checkout.held.path == path
	checkout.Unlock()
	if ours {
		return false, nil
	}
	held, err := CheckoutLockHeld(ctx, l.InstallRoot)
	if err != nil || !held {
		return false, err
	}
	_, inherited := inheritedLockFD(path)
	return !inherited, nil
}

// Reports whether pid is this process, the handoff partner or one of our ancestors
func isPartner(pid int) bool {
	return pid == os.Getpid() || (pid != 0 && (pid == handoffPID() || isAncestorPID(pid)))
}

// Runs under a live claim of ours, an ancestor or the handoff partner, writing ourselves in as delegate
func (l *UpdateLock) adoptOrRefuse(ctx context.Context, existing marker) (bool, error) {
	w := realWorld()
	partners := livePartners(existing, w)
	adopt := false
	for _, p := range partners {
		if isPartner(p) && (p == os.Getpid() || !runtimeHostBelow(p)) {
			adopt = true
			break
		}
	}
	if !adopt {
		age := 0.0
		if existing.startedAt != nil && *existing.startedAt != 0 {
			age = existing.age(w)
		}
		l.Holder = &UpdateHolder{PID: partners[0], AgeSeconds: age}
		return false, nil
	}
	own := IdentityLine(os.Getpid())
	inPartners := func(pid int) bool {
		for _, p := range partners {
			if p == pid {
				return true
			}
		}
		return false
	}
	delegateIn := existing.delegatePID != nil && inPartners(*existing.delegatePID)
	if !inPartners(os.Getpid()) && !delegateIn && existing.createTime != nil && own != "" {
		base := markerBase(existing.raw)
		if !bytes.HasSuffix(base, []byte("\n")) {
			base = append(base, '\n')
		}
		delegated := append(append([]byte{}, base...), fmt.Sprintf("delegate:%d %s\n", os.Getpid(), own)...)
		for _, run := range existing.runs {
			delegated = append(delegated, "run:"+run+"\n"...)
		}
		if _, err := compareAndSwap(ctx, l.Path, existing.raw, delegated); err != nil {
			return false, err
		}
	}
	return true, nil
}

// First three lines of raw exactly as written, bytes splitlines line ends kept
func markerBase(raw []byte) []byte {
	var out []byte
	lines := 0
	for i := 0; i < len(raw) && lines < 3; {
		j := i
		for j < len(raw) && raw[j] != '\n' && raw[j] != '\r' {
			j++
		}
		if j < len(raw) {
			if raw[j] == '\r' && j+1 < len(raw) && raw[j+1] == '\n' {
				j++
			}
			j++
		}
		out = append(out, raw[i:j]...)
		i = j
		lines++
	}
	return out
}

func (l *UpdateLock) dropCheckout() {
	if l.checkout {
		l.checkout = false
		releaseCheckout()
	}
}

// Gives back this incarnation's part of the claim under the marker mutex, only the outermost claim releasing
func (l *UpdateLock) Release(ctx context.Context) {
	defer func() {
		l.Acquired, l.claimed = false, false
		l.dropCheckout()
	}()
	if !l.claimed {
		return
	}
	claims.Lock()
	count, ok := claims.count[l.Path]
	if !ok {
		count = 1
	}
	count--
	last := count <= 0
	if last {
		delete(claims.count, l.Path)
	} else {
		claims.count[l.Path] = count
	}
	claims.Unlock()
	if last {
		if err := releaseMarker(ctx, l.Path); err != nil {
			slog.Debug("Could not release update marker", "path", l.Path, "error", err)
		}
	}
}

// Deletes or rewrites the marker by identity under the mutex as the release decision says
func releaseMarker(ctx context.Context, path string) error {
	ctx, release, err := MarkerMutex(ctx, path, MutexWaitSeconds*time.Second)
	if err != nil {
		return err
	}
	defer release()
	raw := readMarkerBytes(path)
	if raw == nil {
		return nil
	}
	switch action, replacement := releaseDecision(raw, nil); action {
	case "delete":
		if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	case "rewrite":
		_, err := compareAndSwap(ctx, path, raw, replacement)
		return err
	}
	return nil
}
