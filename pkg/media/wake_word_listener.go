package media

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"sync"
	"syscall"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyfs"
)

var (
	wakeDetectorLock     sync.Mutex
	wakeDetector         *WakeWordDetector
	wakeDetectorOwner    any
	wakeDetectorFileLock *os.File
	wakeLockPath         = func() string { return filepath.Join(home.Root(), "runtime", "wake-word.lock") }
)

// Identity of two owners like Python is, never panicking on uncomparable values
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

// Non blocking exclusive lock or unlock of the lease file, one byte through msvcrt semantics on Windows
func wakeFlock(f *os.File, acquire bool) error {
	if runtime.GOOS == "windows" {
		if acquire {
			end, err := f.Seek(0, io.SeekEnd)
			if err != nil {
				return err
			}
			if end == 0 {
				if _, err := f.Write([]byte{0}); err != nil {
					return err
				}
			}
		}
		if _, err := f.Seek(0, io.SeekStart); err != nil {
			return err
		}
	}
	if !acquire {
		return pyfs.UnlockFile(f)
	}
	ok, err := pyfs.TryLockFile(f)
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
func StartListening(ctx context.Context, onWake func() error, owner any, cfg map[string]any, externalAudio bool) (*WakeWordDetector, error) {
	if owner == nil {
		return nil, &WakeWordError{Class: "ValueError", Msg: "wake-word owner must not be None"}
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
		engineCfg := make(map[string]any, len(cfg)+1)
		for k, v := range cfg {
			engineCfg[k] = v
		}
		engineCfg["capture"] = "local"
		if externalAudio {
			engineCfg["capture"] = "client"
		}
		engine, err := wakeBuildEngine(ctx, engineCfg)
		if err != nil {
			return nil, err
		}
		wakeDetector = NewWakeWordDetector(engine, onWake, WakeDetectorOptions{OnFailure: detectorFailed, InputDevice: wakeInputDevice(cfg), ExternalAudio: externalAudio})
		wakeDetectorOwner = owner
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
func GetInputDeviceStatus(ctx context.Context, cfg map[string]any) WakeInputDeviceDetails {
	if det := currentDetector(); det != nil {
		return det.InputDeviceDetails()
	}
	if cfg == nil {
		cfg = LoadWakeWordConfig(ctx)
	}
	return describeInputDevice(wakeInputDevice(cfg), nil)
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

// FeedAudio pushes client captured PCM into the owner's armed client detector, true when accepted
func FeedAudio(owner any, pcm any) (bool, error) {
	wakeDetectorLock.Lock()
	det := ownedDetector(owner)
	wakeDetectorLock.Unlock()
	if det == nil || !det.externalAudio {
		return false, nil
	}
	if err := det.Feed(pcm); err != nil {
		return false, err
	}
	return true, nil
}

// WakeFrameInfo is the sample rate and frame length client capture streamers use
type WakeFrameInfo struct {
	SampleRate    int   `json:"sample_rate"`
	ExternalAudio *bool `json:"external_audio,omitempty"`
	FrameLength   int   `json:"frame_length"`
}

// DetectorFrameInfo returns the frame contract of the armed detector or the default one
func DetectorFrameInfo() WakeFrameInfo {
	det := currentDetector()
	if det == nil {
		return WakeFrameInfo{SampleRate: WakeSampleRate, FrameLength: 1280}
	}
	external := det.externalAudio
	fl := det.Engine.FrameLength()
	if fl == 0 {
		fl = 1280
	}
	return WakeFrameInfo{SampleRate: WakeSampleRate, ExternalAudio: &external, FrameLength: fl}
}
