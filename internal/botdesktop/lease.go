package botdesktop

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/pkg/fsx"
)

// Who drives the screen
const (
	Agent = "agent"
	Human = "human"
)

const humanMessage = "A human has taken over this desktop (they may be entering a credential). Screen actions and captures are refused until " +
	"they hand control back. Tell the user what you need in your reply."

// Refusal of a screen action while a person holds the screen
type HumanHasControl struct{ Message string }

func (e *HumanHasControl) Error() string { return e.Message }

// Who drives a profile's screen. Every change of hands moves the epoch on, so an action admitted before it can tell its result is void
type Lease struct {
	Holder string `json:"holder"`
	// Capability of the person's client that took it, never shown
	Viewer string `json:"viewer_id,omitempty"`
	// Unix seconds of the last change of hands
	Since  float64 `json:"since"`
	Reason string  `json:"reason"`
	Epoch  int64   `json:"epoch"`
}

func now() float64 { return float64(time.Now().UnixNano()) / 1e9 }

// Short hash of the viewer that holds the screen, empty when no viewer does
func (l Lease) Hash() string {
	if l.Viewer == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(l.Viewer))
	return hex.EncodeToString(sum[:])[:12]
}

func leasePath(cfg *config.Config) string { return filepath.Join(StateDir(cfg), "lease.json") }

// Lease of the profile as kept: the agent's with no file, a person's for one that cannot be read, so the agent never acts blind
func readLease(path string) Lease {
	raw, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return Lease{Holder: Agent, Since: now()}
	}
	corrupt := func(reason string) Lease {
		return Lease{Holder: Human, Viewer: "unreadable-lease", Since: now(), Reason: reason}
	}
	if err != nil {
		return corrupt("lease file unreadable")
	}
	var l Lease
	if json.Unmarshal(bytes.TrimPrefix(raw, []byte("\xef\xbb\xbf")), &l) != nil || l.Holder != Agent && l.Holder != Human {
		return corrupt("lease file corrupt")
	}
	return l
}

func writeLease(path string, l Lease) error {
	data, err := json.Marshal(l)
	if err != nil {
		return err
	}
	return fsx.Write(path, data, 0o600)
}

// Lease of the profile
func (d *Desktop) Lease(cfg *config.Config) Lease { return readLease(leasePath(cfg)) }

// Changes the lease under its lock across processes when mutate says so, moving the epoch on and telling whoever listens
func (d *Desktop) transition(cfg *config.Config, mutate func(*Lease) bool) (Lease, error) {
	path := leasePath(cfg)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return Lease{}, err
	}
	unlock, err := fsx.Lock(context.Background(), filepath.Join(filepath.Dir(path), "lease.lock"), -1)
	if err != nil {
		return Lease{}, err
	}
	l := readLease(path)
	changed := mutate(&l)
	if changed {
		l.Epoch++
		err = writeLease(path, l)
	}
	unlock()
	if err != nil || !changed {
		return l, err
	}
	d.leased(cfg, l)
	return l, nil
}

// A person takes the screen under the viewer id of their client, last one in winning; a hold by the same viewer stays as it is
func (d *Desktop) Take(cfg *config.Config, viewer, reason string) (Lease, error) {
	d.Touch(cfg)
	return d.transition(cfg, func(l *Lease) bool {
		if l.Holder == Human && l.Viewer == viewer {
			return false
		}
		*l = Lease{Holder: Human, Viewer: viewer, Since: now(), Reason: reason, Epoch: l.Epoch}
		return true
	})
}

// How control is handed back: by the viewer that took it, none meaning anyone; not while a person holds it when unless says so; only
// while the lease is still at the epoch judged, when one is
type Handback struct {
	Viewer string
	Unless bool
	Epoch  *int64
}

// Hands the screen back to the agent unless the lease moved since the epoch judged, or another viewer holds it
func (d *Desktop) Release(cfg *config.Config, h Handback) (Lease, error) {
	return d.transition(cfg, func(l *Lease) bool {
		switch {
		case h.Epoch != nil && l.Epoch != *h.Epoch:
			slog.Info("bot-desktop lease: release of an older epoch ignored, the lease changed since", "epoch", *h.Epoch)
			return false
		case h.Unless && l.Holder == Human:
			slog.Info("bot-desktop lease: bare release ignored, a human holds")
			return false
		case h.Viewer != "" && l.Holder == Human && l.Viewer != h.Viewer:
			slog.Info("bot-desktop lease: release by another viewer ignored")
			return false
		case l.Holder == Agent:
			return false
		}
		*l = Lease{Holder: Agent, Since: now(), Epoch: l.Epoch}
		return true
	})
}

// Whether a person holds the profile's screen
func (d *Desktop) HumanHolds(cfg *config.Config) bool { return d.Lease(cfg).Holder == Human }

// Epoch an agent action is admitted under, HumanHasControl while a person holds the screen
func (d *Desktop) Admit(cfg *config.Config) (int64, error) {
	l := d.Lease(cfg)
	if l.Holder == Human {
		return l.Epoch, &HumanHasControl{humanMessage}
	}
	return l.Epoch, nil
}

// Epoch the lease is at now, which an admitted action compares to its own once it ran
func (d *Desktop) Epoch(cfg *config.Config) int64 { return d.Lease(cfg).Epoch }
