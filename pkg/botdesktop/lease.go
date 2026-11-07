package botdesktop

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"math/big"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyfs"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

const (
	LeaseAgent = "agent"
	LeaseHuman = "human"
)

const humanHasControlMessage = "A human has taken over this desktop (they may be entering a credential). Screen actions and " +
	"captures are refused until they hand control back. Tell the user what you need in your reply."

// Raised by screen driving tools while a human holds the lease
type HumanHasControl struct{ Msg string }

func (e *HumanHasControl) Error() string { return e.Msg }

// Who drives a profile's Bot Desktop screen, the non holder fields kept as the JSON values Python carries
type BotDesktopLease struct {
	Holder   string
	ViewerID any
	Since    any
	Reason   any
	Epoch    any
}

func pyNow() float64 { return float64(time.Now().UnixNano()) / 1e9 }

func newLease() BotDesktopLease {
	return BotDesktopLease{Holder: LeaseAgent, Since: pyNow(), Reason: "", Epoch: int64(0)}
}

func corruptLease(reason string) BotDesktopLease {
	l := newLease()
	l.Holder, l.ViewerID, l.Reason = LeaseHuman, "unreadable-lease", reason
	return l
}

func (l BotDesktopLease) AsDict() *pyjson.Object {
	return pyjson.ObjectOf("holder", l.Holder, "viewer_id", l.ViewerID, "since", l.Since, "reason", l.Reason, "epoch", l.Epoch)
}

// Integer epoch for comparisons across an admitted action, bools and floats folded as Python int does
func (l BotDesktopLease) EpochInt() int {
	switch v := l.Epoch.(type) {
	case int64:
		return int(v)
	case int:
		return v
	case float64:
		return int(v)
	case bool:
		if v {
			return 1
		}
	case *big.Int:
		return int(v.Int64())
	}
	return 0
}

// Epoch plus one, the TypeError Python raises adding one to a non numeric epoch
func (l *BotDesktopLease) bumpEpoch() error {
	switch v := l.Epoch.(type) {
	case int64:
		l.Epoch = v + 1
	case int:
		l.Epoch = int64(v) + 1
	case float64:
		l.Epoch = v + 1
	case bool:
		l.Epoch = int64(l.EpochInt()) + 1
	case *big.Int:
		l.Epoch = new(big.Int).Add(v, big.NewInt(1))
	default:
		return fmt.Errorf("TypeError: can only concatenate %T (not \"int\") to it", v)
	}
	return nil
}

// Lease as RPC results and broadcasts show it, the viewer id capability replaced by a short hash
func LeasePublicView(l BotDesktopLease) *pyjson.Object {
	d := l.AsDict()
	d.Set("viewer_id", nil)
	if id, ok := l.ViewerID.(string); ok && id != "" {
		sum := sha256.Sum256([]byte(id))
		d.Set("viewer_hash", hex.EncodeToString(sum[:])[:12])
	} else {
		d.Set("viewer_hash", nil)
	}
	return d
}

var (
	leaseListenersMu sync.Mutex
	leaseListeners   []*leaseListener
)

type leaseListener struct {
	fn func(key string, lease BotDesktopLease)
}

func leaseHome(profileKey string) string {
	if profileKey != "" {
		return profileKey
	}
	return home.Process()
}

func botLeasePath(profileKey string) string {
	return filepath.Join(leaseHome(profileKey), "bot-desktop", "lease.json")
}

// Lease on disk, agent for no file and human for an unreadable or corrupt one so the agent never acts blind
func leaseRead(path string) BotDesktopLease {
	raw, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return newLease()
	}
	if err != nil {
		return corruptLease("lease file unreadable")
	}
	text, err := pyjson.DecodeBytes([]byte(strings.TrimPrefix(string(raw), "\xef\xbb\xbf")))
	if err != nil {
		return corruptLease("lease file unreadable")
	}
	data, err := pyjson.Loads(text)
	obj, ok := data.(*pyjson.Object)
	if err != nil || !ok {
		return corruptLease("lease file corrupt")
	}
	holder, _ := obj.Get("holder")
	h, _ := holder.(string)
	if h != LeaseAgent && h != LeaseHuman {
		return corruptLease("lease file corrupt")
	}
	l := newLease()
	l.Holder = h
	if v, ok := obj.Get("viewer_id"); ok {
		l.ViewerID = v
	}
	if v, ok := obj.Get("since"); ok {
		l.Since = v
	}
	if v, ok := obj.Get("reason"); ok {
		l.Reason = v
	}
	if v, ok := obj.Get("epoch"); ok {
		l.Epoch = v
	}
	return l
}

func leasePrivateDir(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o777); err != nil {
		return err
	}
	home.SecureParentDir(path)
	return nil
}

func leaseWrite(path string, l BotDesktopLease) error {
	if err := leasePrivateDir(path); err != nil {
		return err
	}
	text, err := pyjson.Dumps(l.AsDict())
	if err != nil {
		return err
	}
	tmp := strings.TrimSuffix(path, filepath.Ext(path)) + ".json.tmp"
	fh, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := fh.WriteString(text); err != nil {
		fh.Close()
		return err
	}
	if err := fh.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Cross process critical section over a sibling lock file, a no op where hermes has no fcntl
func leaseLocked(path string, fn func() error) error {
	if runtime.GOOS == "windows" {
		return fn()
	}
	lockfile := strings.TrimSuffix(path, filepath.Ext(path)) + ".lock"
	if err := leasePrivateDir(lockfile); err != nil {
		return err
	}
	fh, err := os.OpenFile(lockfile, os.O_RDWR|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer fh.Close()
	if err := pyfs.LockFile(fh); err != nil {
		return err
	}
	defer pyfs.UnlockFile(fh)
	return fn()
}

// Lease of a profile home, empty meaning the process home
func LeaseGet(profileKey string) BotDesktopLease { return leaseRead(botLeasePath(profileKey)) }

// Subscribes to transitions made in this process and returns the unsubscribe
func LeaseOnChange(fn func(key string, lease BotDesktopLease)) func() {
	l := &leaseListener{fn: fn}
	leaseListenersMu.Lock()
	leaseListeners = append(leaseListeners, l)
	leaseListenersMu.Unlock()
	return func() {
		leaseListenersMu.Lock()
		defer leaseListenersMu.Unlock()
		for i, x := range leaseListeners {
			if x == l {
				leaseListeners = append(leaseListeners[:i:i], leaseListeners[i+1:]...)
				return
			}
		}
	}
}

func leaseNotify(key string, l BotDesktopLease) {
	leaseListenersMu.Lock()
	snapshot := append([]*leaseListener(nil), leaseListeners...)
	leaseListenersMu.Unlock()
	for _, x := range snapshot {
		func() {
			defer func() { recover() }()
			x.fn(key, l)
		}()
	}
}

func leaseTransition(profileKey string, mutate func(*BotDesktopLease) bool) (BotDesktopLease, error) {
	key, path := home.Key(leaseHome(profileKey)), botLeasePath(profileKey)
	var out BotDesktopLease
	changed := false
	err := leaseLocked(path, func() error {
		out = leaseRead(path)
		if !mutate(&out) {
			return nil
		}
		if err := out.bumpEpoch(); err != nil {
			return err
		}
		changed = true
		return leaseWrite(path, out)
	})
	if err != nil || !changed {
		return out, err
	}
	leaseNotify(key, out)
	return out, nil
}

// Human viewer takes control, last writer wins and an existing hold by the same viewer stays put
func LeaseAcquire(viewerID, profileKey, reason string) (BotDesktopLease, error) {
	return leaseTransition(profileKey, func(l *BotDesktopLease) bool {
		if l.Holder == LeaseHuman && l.ViewerID == viewerID {
			return false
		}
		l.Holder, l.ViewerID, l.Since, l.Reason = LeaseHuman, viewerID, pyNow(), reason
		return true
	})
}

// Release scope, a nil viewer or epoch meaning any
type LeaseReleaseOptions struct {
	ViewerID    *string
	ProfileKey  string
	UnlessHuman bool
	Epoch       *int
}

// Returns control to the agent unless the lease changed since the judged epoch or another viewer holds
func LeaseRelease(opts LeaseReleaseOptions) (BotDesktopLease, error) {
	return leaseTransition(opts.ProfileKey, func(l *BotDesktopLease) bool {
		if opts.Epoch != nil && l.EpochInt() != *opts.Epoch {
			slog.Info(fmt.Sprintf("bot-desktop lease: release of epoch %d ignored, the lease changed since", *opts.Epoch))
			return false
		}
		if opts.UnlessHuman && l.Holder == LeaseHuman {
			slog.Info("bot-desktop lease: bare release ignored, a human holds")
			return false
		}
		if opts.ViewerID != nil && l.Holder == LeaseHuman && l.ViewerID != *opts.ViewerID {
			slog.Info(fmt.Sprintf("bot-desktop lease: release by '%s' ignored, another viewer holds", *opts.ViewerID))
			return false
		}
		if l.Holder == LeaseAgent {
			return false
		}
		l.Holder, l.ViewerID, l.Since, l.Reason = LeaseAgent, nil, pyNow(), ""
		return true
	})
}

func LeaseHumanHolds(profileKey string) bool { return LeaseGet(profileKey).Holder == LeaseHuman }

func LeaseViewerMaySendInput(viewerID, profileKey string) bool {
	l := LeaseGet(profileKey)
	return l.Holder == LeaseHuman && l.ViewerID == viewerID
}

// Lease as of now for an admitted action, HumanHasControl while a human holds the screen
func LeaseAssertAgentMayAct(profileKey string) (BotDesktopLease, error) {
	l := LeaseGet(profileKey)
	if l.Holder == LeaseHuman {
		return l, &HumanHasControl{humanHasControlMessage}
	}
	return l, nil
}
