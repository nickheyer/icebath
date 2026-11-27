// The Bot Desktop: each profile's own headless screen, a TigerVNC and Xfce desktop on this machine or inside the terminal backend's
// sandbox, that computer use and the browser drive and that a person can take over, the agent's actions refused until they hand it back
package botdesktop

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	bdconfig "github.com/nickheyer/icebath/internal/botdesktop/config"
	"github.com/nickheyer/icebath/internal/browser"
	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/terminal"
)

// What a feature hears of a profile's screen: that it came up or went down, or changed hands, with its status as of then
type Registrant interface {
	Changed(cfg *config.Config, event Event)
}

// A change of a profile's screen
type Event struct {
	// Running or stopped, or another holder of the lease
	Kind string
	// The screen as of the change
	Running bool
	Display string
	Lease   Lease
}

// Kinds of change
const (
	Runtime = "status"
	Leased  = "lease"
)

type Registry struct {
	registrants []Registrant
	// Host the registry is nested in, nil for the host's own
	host *Desktop
}

func (r *Registry) Register(registrant Registrant) {
	r.registrants = append(r.registrants, registrant)
}

func (r *Registry) Changed(cfg *config.Config, event Event) {
	for _, registrant := range r.registrants {
		registrant.Changed(cfg, event)
	}
}

// Host the registry is nested in
func (r *Registry) Host() *Desktop {
	return r.host
}

// The screens of every profile
type Desktop struct {
	Registry
	config   *config.Config
	terminal *terminal.Terminal
	browser  *browser.Browser
	// Facts probed of each sandbox environment
	facts sync.Map

	mu sync.Mutex
	// Install slots held by profile folder, by the release of their lock
	installing map[string]func()
	// Containers asked whether they still run
	alive map[string]aliveEntry
	// Profiles whose screens are watched for changes made outside this process, by name, and what was last seen of each
	watched  map[string]*config.Config
	seen     map[string]mark
	watching bool
	lastIdle time.Time
	// Watch streams told of every change, by id
	listeners map[int]func(cfg *config.Config, event Event)
	listener  int
}

func New(cfg *config.Config, host *terminal.Terminal, b *browser.Browser) *Desktop {
	bdconfig.Register(cfg)
	return &Desktop{config: cfg, terminal: host, browser: b, installing: map[string]func(){}, alive: map[string]aliveEntry{},
		watched: map[string]*config.Config{}, seen: map[string]mark{}, listeners: map[int]func(*config.Config, Event){}}
}

// Adds a registrant; a registry nested in the host reaches it through Host
func (d *Desktop) Register(registrant Registrant) {
	if nested, ok := registrant.(*Registry); ok {
		nested.host = d
	}
	d.Registry.Register(registrant)
}

// Tells the watch streams and every registrant of a change
func (d *Desktop) tell(cfg *config.Config, event Event) {
	d.mu.Lock()
	listeners := make([]func(*config.Config, Event), 0, len(d.listeners))
	for _, l := range d.listeners {
		listeners = append(listeners, l)
	}
	d.mu.Unlock()
	for _, l := range listeners {
		l(cfg, event)
	}
	d.Registry.Changed(cfg, event)
}

// Hears every change of a watched screen until the returned stop is called
func (d *Desktop) Listen(cfg *config.Config, hear func(cfg *config.Config, event Event)) func() {
	d.watch(cfg)
	d.mu.Lock()
	d.listener++
	id := d.listener
	d.listeners[id] = hear
	d.mu.Unlock()
	return func() {
		d.mu.Lock()
		delete(d.listeners, id)
		d.mu.Unlock()
	}
}

// A lease change made here, as the watch would also find it
func (d *Desktop) leased(cfg *config.Config, l Lease) {
	d.watch(cfg)
	d.mu.Lock()
	m := d.seen[cfg.Profile()]
	m.epoch, m.leaseKnown = l.Epoch, true
	m.lease = modified(leasePath(cfg))
	d.seen[cfg.Profile()] = m
	d.mu.Unlock()
	d.tell(cfg, Event{Kind: Leased, Lease: l})
}

// What the watch last saw of a profile: the times of its published variables, launcher record and lease, whether its launcher lived, and
// the lease epoch
type mark struct {
	env, launcher, lease time.Time
	alive                bool
	epoch                int64
	leaseKnown, known    bool
}

func modified(path string) time.Time {
	st, err := os.Stat(path)
	if err != nil {
		return time.Time{}
	}
	return st.ModTime()
}

// How often the watch looks, and how often it stops screens idle past their limit
const (
	watchEvery = 500 * time.Millisecond
	idleEvery  = 30 * time.Second
)

// Watches the profile's screen from now, the watch started once for every profile
func (d *Desktop) watch(cfg *config.Config) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.watched[cfg.Profile()] = cfg
	if d.watching {
		return
	}
	d.watching = true
	go func() {
		for {
			d.pass()
			time.Sleep(watchEvery)
		}
	}()
}

// One look at every watched screen: a lease that moved, a screen started or stopped outside this process or whose launcher died, and
// the idle stop now and then
func (d *Desktop) pass() {
	d.mu.Lock()
	profiles := make([]*config.Config, 0, len(d.watched))
	for _, cfg := range d.watched {
		profiles = append(profiles, cfg)
	}
	idleDue := d.lastIdle.IsZero() || time.Since(d.lastIdle) >= idleEvery
	if idleDue {
		d.lastIdle = time.Now()
	}
	d.mu.Unlock()
	ctx := context.Background()
	for _, cfg := range profiles {
		if idleDue {
			if _, err := d.StopIfIdle(ctx, cfg, nil); err != nil {
				slog.Debug("Bot Desktop idle stop failed", "profile", cfg.Profile(), "error", err)
			}
		}
		d.look(ctx, cfg)
	}
}

func (d *Desktop) look(ctx context.Context, cfg *config.Config) {
	dir := StateDir(cfg)
	_, alive := live(cfg)
	now := mark{env: modified(filepath.Join(dir, "env")), launcher: modified(filepath.Join(dir, "launcher.pid")), lease: modified(leasePath(cfg)),
		alive: alive, known: true}
	d.mu.Lock()
	before := d.seen[cfg.Profile()]
	now.epoch, now.leaseKnown = before.epoch, before.leaseKnown
	d.seen[cfg.Profile()] = now
	d.mu.Unlock()
	if !now.lease.Equal(before.lease) {
		l := d.Lease(cfg)
		d.mu.Lock()
		m := d.seen[cfg.Profile()]
		moved := m.leaseKnown && m.epoch != l.Epoch
		m.epoch, m.leaseKnown = l.Epoch, true
		d.seen[cfg.Profile()] = m
		d.mu.Unlock()
		if moved {
			d.tell(cfg, Event{Kind: Leased, Lease: l})
		}
	}
	if !before.known || now.env.Equal(before.env) && now.launcher.Equal(before.launcher) && now.alive == before.alive {
		return
	}
	published, err := d.Published(ctx, cfg)
	if err != nil {
		slog.Debug("Bot Desktop watch: status unreadable", "profile", cfg.Profile(), "error", err)
		return
	}
	d.tell(cfg, Event{Kind: Runtime, Running: published["DISPLAY"] != "", Display: published["DISPLAY"], Lease: d.Lease(cfg)})
}

// One line telling the agent of its screen: running on a display with who holds it, or no longer running; empty for a running screen
// whose display or holder is not known
func Note(running bool, display, holder string) string {
	if !running {
		return "Bot Screen: this profile's own headless desktop is no longer running. Do not refer to 'the bot screen' or route GUI launches at " +
			"it; GUI apps from the terminal open on the user's own display again."
	}
	held := ""
	switch holder {
	case Human:
		held = "a human holds it — do not drive the screen; ask them or wait"
	case Agent:
		held = "you hold it"
	}
	if display == "" || held == "" {
		return ""
	}
	return fmt.Sprintf("Bot Screen: this profile's own headless desktop is RUNNING on display %s (%s). 'screen N' / ':N' / 'the bot screen' / "+
		"'your screen' means THIS screen: GUI apps you launch from the terminal already open there (their DISPLAY is routed to it), and display "+
		"introspection (xrandr/xdotool/wmctrl) targets it. It is NOT the user's own display.", display, held)
}

// Line of the runtime environment block for the profile's running screen, empty while none runs
func (d *Desktop) Hint(ctx context.Context, cfg *config.Config) string {
	published, err := d.Published(ctx, cfg)
	if err != nil {
		return ""
	}
	return Note(true, published["DISPLAY"], d.Lease(cfg).Holder)
}

// Stops the deleted profile's screen and hands its lease back before its folder goes
func (d *Desktop) Delete(profile string) error {
	if !Supported() || !d.config.Exists(profile) {
		return nil
	}
	cfg := d.config.In(profile)
	stopped, err := d.Stop(context.Background(), cfg, nil)
	if err == nil && stopped {
		_, err = d.Release(cfg, Handback{})
	}
	if err != nil {
		slog.Warn(fmt.Sprintf("Could not stop the Bot Desktop of %s: %v", profile, err))
	}
	d.mu.Lock()
	delete(d.watched, profile)
	delete(d.seen, profile)
	d.mu.Unlock()
	return nil
}

// The screens as profiles come and go: a profile's stopped and its lease handed back before it is deleted or its folder moves to a new
// name
type Profiles struct{ Desktop *Desktop }

func (p Profiles) Delete(profile string) error { return p.Desktop.Delete(profile) }

func (p Profiles) Stop(profile string) error { return p.Desktop.Delete(profile) }
