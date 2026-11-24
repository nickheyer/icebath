//go:build !icebath_core && !icebath_noextras

package media

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"syscall"

	"github.com/nickheyer/icebath/pkg/home"
	fsutil "github.com/nickheyer/icebath/pkg/utils/fs"
)

var (
	wakeDetectorLock     sync.Mutex
	wakeDetector         *WakeWordDetector
	wakeDetectorOwner    any
	wakeDetectorFileLock *os.File
	// Phrase and provider the running detector's engine was built for
	wakeDetectorPhrase   string
	wakeDetectorProvider string
	wakeLockPath         = func() string { return filepath.Join(home.Root(), "runtime", "wake-word.lock") }
)

// Identity of two owners, uncomparable maps, slices and funcs compared by their pointer, never panicking
func sameOwner(a, b any) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	ta, tb := reflect.TypeOf(a), reflect.TypeOf(b)
	if ta != tb {
		return false
	}
	if ta.Comparable() {
		return a == b
	}
	switch ta.Kind() {
	case reflect.Func, reflect.Map, reflect.Slice:
		return reflect.ValueOf(a).Pointer() == reflect.ValueOf(b).Pointer()
	}
	return false
}

// Non blocking exclusive lock or unlock of the lease file
func wakeFlock(f *os.File, acquire bool) error {
	if !acquire {
		return fsutil.UnlockFile(f)
	}
	ok, err := fsutil.TryLockFile(f)
	if err != nil {
		return err
	}
	if !ok {
		return &os.PathError{Op: "flock", Path: f.Name(), Err: syscall.EWOULDBLOCK}
	}
	return nil
}

// Takes the cross process microphone lease, WakeWordInUse when another handle holds it
func acquireMachineLock(path string) (*os.File, error) {
	if path == "" {
		path = wakeLockPath()
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o777); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_APPEND, 0o666)
	if err != nil {
		return nil, err
	}
	if err := wakeFlock(f, true); err != nil {
		f.Close()
		return nil, &WakeWordInUse{Msg: "Wake-word microphone is already owned.", Cause: err}
	}
	return f, nil
}

// Unlocks and closes the lease handle, ignoring an unlock failure
func releaseMachineLock(f *os.File) {
	if f == nil {
		return
	}
	_ = wakeFlock(f, false)
	f.Close()
}

// Forgets the singleton and runs closeFn, always releasing the machine lease, caller holds the lock
func teardownLocked(closeFn func()) {
	handle := wakeDetectorFileLock
	wakeDetector, wakeDetectorOwner, wakeDetectorFileLock = nil, nil, nil
	wakeDetectorPhrase, wakeDetectorProvider = "", ""
	defer releaseMachineLock(handle)
	closeFn()
}

// The armed detector when owner holds the lease, caller holds the lock
func ownedDetector(owner any) *WakeWordDetector {
	if wakeDetector != nil && sameOwner(wakeDetectorOwner, owner) {
		return wakeDetector
	}
	return nil
}

// Releases ownership when the active microphone stream dies unexpectedly
func detectorFailed(det *WakeWordDetector) {
	wakeDetectorLock.Lock()
	defer wakeDetectorLock.Unlock()
	if wakeDetector == det {
		teardownLocked(det.Engine.Close)
	}
}

// StartListening claims, builds and starts the detector, idempotent for its owner and WakeWordInUse for others
func StartListening(ctx context.Context, onWake func() error, owner any, cfg *WakeConfig) (*WakeWordDetector, error) {
	if owner == nil {
		return nil, &WakeWordError{Kind: errWakeInvalid, Msg: "wake-word owner must not be nil"}
	}
	wakeDetectorLock.Lock()
	defer wakeDetectorLock.Unlock()
	if wakeDetector != nil {
		if !sameOwner(wakeDetectorOwner, owner) {
			return nil, &WakeWordInUse{Msg: "Wake-word microphone is already owned."}
		}
		wakeDetector.setOnWake(onWake)
		if err := wakeDetector.Resume(); err != nil {
			return nil, err
		}
		return wakeDetector, nil
	}
	handle, err := acquireMachineLock("")
	if err != nil {
		return nil, err
	}
	wakeDetectorFileLock = handle
	det, err := func() (*WakeWordDetector, error) {
		if cfg == nil {
			cfg = LoadWakeWordConfig(ctx)
		}
		provider, err := wakeProvider(ctx, cfg, nil)
		if err != nil {
			return nil, err
		}
		engine, err := wakeBuildEngine(ctx, cfg)
		if err != nil {
			return nil, err
		}
		wakeDetector = NewWakeWordDetector(engine, onWake, WakeDetectorOptions{OnFailure: detectorFailed, InputDevice: cfg.InputDevice})
		wakeDetectorOwner = owner
		wakeDetectorPhrase, wakeDetectorProvider = cfg.Phrase, provider
		return wakeDetector, wakeDetector.Start()
	}()
	if err != nil {
		closeFn := func() {}
		if wakeDetector != nil {
			closeFn = wakeDetector.Stop
		}
		teardownLocked(closeFn)
		return nil, err
	}
	return det, nil
}

// Under the lock, whether owner holds the lease, running action on its detector when given
func ownedCall(owner any, action func(*WakeWordDetector) error) (bool, error) {
	wakeDetectorLock.Lock()
	defer wakeDetectorLock.Unlock()
	det := ownedDetector(owner)
	if det == nil {
		return false, nil
	}
	if action != nil {
		if err := action(det); err != nil {
			return true, err
		}
	}
	return true, nil
}

// Phrase and provider owner's running detector was built for, false when owner holds no detector
func ListeningFor(owner any) (phrase, provider string, ok bool) {
	wakeDetectorLock.Lock()
	defer wakeDetectorLock.Unlock()
	if ownedDetector(owner) == nil {
		return "", "", false
	}
	return wakeDetectorPhrase, wakeDetectorProvider, true
}

// OwnsListener reports whether owner holds the wake word lease
func OwnsListener(owner any) bool {
	ok, _ := ownedCall(owner, nil)
	return ok
}

// PauseListening releases the microphone only when owner holds the lease
func PauseListening(owner any) bool {
	ok, _ := ownedCall(owner, func(d *WakeWordDetector) error { d.Pause(); return nil })
	return ok
}

// ResumeListening reopens the microphone only when owner holds the lease
func ResumeListening(owner any) (bool, error) {
	return ownedCall(owner, func(d *WakeWordDetector) error { return d.Resume() })
}

// StopListening fully stops the detector only when owner holds the lease
func StopListening(owner any) bool {
	ok, _ := ownedCall(owner, func(d *WakeWordDetector) error { teardownLocked(d.Stop); return nil })
	return ok
}

func currentDetector() *WakeWordDetector {
	wakeDetectorLock.Lock()
	defer wakeDetectorLock.Unlock()
	return wakeDetector
}

// IsListening reports an armed detector with a live reader
func IsListening() bool {
	det := currentDetector()
	return det != nil && det.Running()
}

// AudioIsSilent reports an armed stream that opens fine but delivers only silence
func AudioIsSilent() bool {
	det := currentDetector()
	return det != nil && det.AudioSilent()
}

// GetInputDeviceStatus returns the active or configured PortAudio input diagnostics for status UIs
func GetInputDeviceStatus(ctx context.Context, cfg *WakeConfig) WakeInputDeviceDetails {
	if det := currentDetector(); det != nil {
		return det.InputDeviceDetails()
	}
	if cfg == nil {
		cfg = LoadWakeWordConfig(ctx)
	}
	return describeInputDevice(cfg.InputDevice, nil)
}

// Engines that report which phrase and profile the last fire matched
type wakeLastMatcher interface {
	LastMatch() (phrase, profile string, ok bool)
}

// GetLastMatch returns the phrase and profile of the latest fire when the engine routes per phrase
func GetLastMatch() (phrase, profile string, ok bool) {
	det := currentDetector()
	if det == nil {
		return "", "", false
	}
	if m, isMatcher := det.Engine.(wakeLastMatcher); isMatcher {
		return m.LastMatch()
	}
	return "", "", false
}
