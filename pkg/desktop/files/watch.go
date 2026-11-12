package files

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
)

const (
	WatchStormMaxEvents  = 1000
	WatchStormWindowMS   = 1000
	WatchStormPollMS     = 5000
	previewWatchDebounce = 120 * time.Millisecond
	PreviewFileChanged   = "preview-file-changed"
)

// WatchListener receives the event type and file name of one raw watch event
type WatchListener func(eventType, filename string)

// Closer is a watcher that can be closed
type Closer interface{ Close() }

// Clock is the time source and interval timer of a guarded watch
type Clock interface {
	NowMS() int64
	Every(ms int64, fn func()) (stop func())
}

type realClock struct{}

func (realClock) NowMS() int64 { return time.Now().UnixMilli() }

func (realClock) Every(ms int64, fn func()) func() {
	t := time.NewTicker(time.Duration(ms) * time.Millisecond)
	done := make(chan struct{})
	go func() {
		for {
			select {
			case <-t.C:
				fn()
			case <-done:
				return
			}
		}
	}()
	var once sync.Once
	return func() { once.Do(func() { t.Stop(); close(done) }) }
}

// GuardedWatchOptions configures a raw watch with the storm breaker
type GuardedWatchOptions struct {
	Platform     string
	Watch        func(listener WatchListener) (Closer, error)
	OnEvent      WatchListener
	Snapshot     func() (string, error)
	OnPollChange func()
	OnTrip       func(events int, windowMS int64)
	Clock        Clock
	MaxEvents    int
	WindowMS     int64
	PollMS       int64
}

// GuardedWatch is a watch that may have fallen back to polling
type GuardedWatch struct {
	mu       sync.Mutex
	state    string
	watcher  Closer
	stopPoll func()
}

// True on win32, the platform whose ReadDirectoryChangesW storms
func StormBreakerArmed(platform string) bool { return platform == "win32" }

// Snapshot text, nil when the snapshot failed so a vanished empty folder still differs
func safeSnapshot(snapshot func() (string, error)) *string {
	s, err := snapshot()
	if err != nil {
		return nil
	}
	return &s
}

func sameSnapshot(a, b *string) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

// Raw watch that trips to a slow snapshot poll past the event rate on win32
func NewGuardedWatch(o GuardedWatchOptions) (*GuardedWatch, error) {
	clock := o.Clock
	if clock == nil {
		clock = realClock{}
	}
	maxEvents, windowMS, pollMS := o.MaxEvents, o.WindowMS, o.PollMS
	if maxEvents == 0 {
		maxEvents = WatchStormMaxEvents
	}
	if windowMS == 0 {
		windowMS = WatchStormWindowMS
	}
	if pollMS == 0 {
		pollMS = WatchStormPollMS
	}
	armed := StormBreakerArmed(o.Platform)
	g := &GuardedWatch{state: "watch"}
	windowStart := clock.NowMS()
	count := 0
	trip := func() {
		g.state = "poll"
		if g.watcher != nil {
			g.watcher.Close()
			g.watcher = nil
		}
		if o.OnTrip != nil {
			o.OnTrip(count, windowMS)
		}
		last := safeSnapshot(o.Snapshot)
		var pollMu sync.Mutex
		g.stopPoll = clock.Every(pollMS, func() {
			pollMu.Lock()
			next := safeSnapshot(o.Snapshot)
			changed := !sameSnapshot(next, last)
			if changed {
				last = next
			}
			pollMu.Unlock()
			if changed {
				o.OnPollChange()
			}
		})
	}
	w, err := o.Watch(func(eventType, filename string) {
		g.mu.Lock()
		if g.state != "watch" {
			g.mu.Unlock()
			return
		}
		if armed {
			t := clock.NowMS()
			if t-windowStart >= windowMS {
				windowStart = t
				count = 0
			}
			count++
			if count > maxEvents {
				trip()
				g.mu.Unlock()
				o.OnEvent(eventType, filename)
				return
			}
		}
		g.mu.Unlock()
		o.OnEvent(eventType, filename)
	})
	if err != nil {
		return nil, err
	}
	g.mu.Lock()
	if g.state == "watch" {
		g.watcher = w
	} else {
		w.Close()
	}
	g.mu.Unlock()
	return g, nil
}

// Closes the watcher and the poll, a second call does nothing
func (g *GuardedWatch) Close() {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.state == "closed" {
		return
	}
	g.state = "closed"
	if g.watcher != nil {
		g.watcher.Close()
		g.watcher = nil
	}
	if g.stopPoll != nil {
		g.stopPoll()
		g.stopPoll = nil
	}
}

// Mode is watch, poll or closed
func (g *GuardedWatch) Mode() string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.state
}

type fsWatcher struct {
	w    *fsnotify.Watcher
	done chan struct{}
	once sync.Once
}

func (f *fsWatcher) Close() {
	f.once.Do(func() {
		close(f.done)
		f.w.Close()
	})
}

// Raw directory watch with Node fs.watch event names, rename for entry churn and change for writes
func watchDirectoryRaw(dir string) func(WatchListener) (Closer, error) {
	return func(listener WatchListener) (Closer, error) {
		w, err := fsnotify.NewWatcher()
		if err != nil {
			return nil, nodeFsError(err, "watch", dir)
		}
		if err := w.Add(dir); err != nil {
			w.Close()
			return nil, nodeFsError(err, "watch", dir)
		}
		fw := &fsWatcher{w: w, done: make(chan struct{})}
		go func() {
			for {
				select {
				case <-fw.done:
					return
				case ev, ok := <-w.Events:
					if !ok {
						return
					}
					kind := "change"
					if ev.Op&(fsnotify.Create|fsnotify.Remove|fsnotify.Rename) != 0 {
						kind = "rename"
					}
					listener(kind, filepath.Base(ev.Name))
				case _, ok := <-w.Errors:
					if !ok {
						return
					}
				}
			}
		}()
		return fw, nil
	}
}

// WatchOwner is the window that registered a watch
type WatchOwner interface {
	IsDestroyed() bool
	Send(event string, payload any)
	OnDestroyed(listener func()) (off func())
}

// PreviewFileChangedPayload is the preview-file-changed event body
type PreviewFileChangedPayload struct {
	ID   string `json:"id"`
	Path string `json:"path"`
	URL  string `json:"url"`
}

// Delivers a change to the window that owns the watch, false and a teardown when it is gone
func SendPreviewFileChangedToOwner(owner WatchOwner, payload PreviewFileChangedPayload, onDestroyed func()) bool {
	if owner == nil || owner.IsDestroyed() {
		if onDestroyed != nil {
			onDestroyed()
		}
		return false
	}
	owner.Send(PreviewFileChanged, payload)
	return true
}

// Tears the watch down when its owner window dies, returning the unsubscribe for close
func OnPreviewWatchOwnerDestroyed(owner WatchOwner, teardown func()) func() {
	if owner == nil {
		return func() {}
	}
	return owner.OnDestroyed(teardown)
}

// PreviewWatches holds every preview, plugin and directory watch by id
type PreviewWatches struct {
	mu       sync.Mutex
	watchers map[string]func()
	platform string
	clock    Clock
}

// Registry of file watches for the given Electron platform name
func NewPreviewWatches(platform string) *PreviewWatches {
	return &PreviewWatches{watchers: map[string]func(){}, platform: platform, clock: realClock{}}
}

func watchID() string {
	b := make([]byte, 12)
	rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

// PreviewWatch is the id and path of a started watch
type PreviewWatch struct {
	ID   string `json:"id"`
	Path string `json:"path"`
}

type debouncer struct {
	mu    sync.Mutex
	timer *time.Timer
}

func (d *debouncer) call(fn func()) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.timer != nil {
		d.timer.Stop()
	}
	d.timer = time.AfterFunc(previewWatchDebounce, func() {
		d.mu.Lock()
		d.timer = nil
		d.mu.Unlock()
		fn()
	})
}

func (d *debouncer) stop() {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.timer != nil {
		d.timer.Stop()
		d.timer = nil
	}
}

func stormLog(dir string) func(int, int64) {
	return func(events int, windowMS int64) {
		slog.Warn(fmt.Sprintf("[icebath] fs.watch storm on %s (%d events within %dms); polling instead", dir, events, windowMS))
	}
}

func fileExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.Mode().IsRegular()
}

// Watches a previewed file, a missing file answered as data, changes sent to the owner window
func (p *PreviewWatches) WatchPreviewFile(owner WatchOwner, rawURL string) (any, error) {
	resolved, err := ResolveReadableFileForIpc(rawURL, ReadableFileOptions{Purpose: "Preview file"})
	if err != nil {
		if IsMissingFileError(err) {
			return missingFileResult(rawURL, err), nil
		}
		return nil, err
	}
	filePath := resolved.ResolvedPath
	watchDir, targetName := filepath.Dir(filePath), filepath.Base(filePath)
	id := watchID()
	var deb debouncer
	offOwner := OnPreviewWatchOwnerDestroyed(owner, func() { p.Stop(id) })
	emit := func() {
		if !fileExists(filePath) {
			return
		}
		SendPreviewFileChangedToOwner(owner, PreviewFileChangedPayload{ID: id, Path: filePath, URL: pathToFileURL(filePath)}, func() { p.Stop(id) })
	}
	g, err := NewGuardedWatch(GuardedWatchOptions{
		Platform: p.platform,
		Watch:    watchDirectoryRaw(watchDir),
		OnEvent: func(_ string, filename string) {
			if filename != "" && filepath.Base(filename) != targetName {
				return
			}
			deb.call(emit)
		},
		Snapshot: func() (string, error) {
			st, err := os.Stat(filePath)
			if err != nil {
				return "", err
			}
			return fmt.Sprintf("%s:%d", jsNumber(mtimeMs(st)), st.Size()), nil
		},
		OnPollChange: emit,
		OnTrip:       stormLog(watchDir),
		Clock:        p.clock,
	})
	if err != nil {
		offOwner()
		return nil, err
	}
	p.mu.Lock()
	p.watchers[id] = func() {
		offOwner()
		deb.stop()
		g.Close()
	}
	p.mu.Unlock()
	return PreviewWatch{ID: id, Path: filePath}, nil
}

// Watches a directory for entries appearing or vanishing, changes sent to the owner window
func (p *PreviewWatches) WatchDirectory(owner WatchOwner, rawDir string) (PreviewWatch, error) {
	watchDir := resolvePath(rawDir)
	if st, err := os.Stat(watchDir); err != nil || !st.IsDir() {
		return PreviewWatch{}, fmt.Errorf("Not a directory: %s", watchDir)
	}
	id := watchID()
	var deb debouncer
	offOwner := OnPreviewWatchOwnerDestroyed(owner, func() { p.Stop(id) })
	emit := func() {
		SendPreviewFileChangedToOwner(owner, PreviewFileChangedPayload{ID: id, Path: watchDir, URL: pathToFileURL(watchDir)}, func() { p.Stop(id) })
	}
	g, err := NewGuardedWatch(GuardedWatchOptions{
		Platform: p.platform,
		Watch:    watchDirectoryRaw(watchDir),
		OnEvent:  func(string, string) { deb.call(emit) },
		Snapshot: func() (string, error) {
			entries, err := os.ReadDir(watchDir)
			if err != nil {
				return "", err
			}
			names := make([]string, len(entries))
			for i, e := range entries {
				names[i] = e.Name()
			}
			sort.Strings(names)
			return strings.Join(names, "\x00"), nil
		},
		OnPollChange: emit,
		OnTrip:       stormLog(watchDir),
		Clock:        p.clock,
	})
	if err != nil {
		offOwner()
		return PreviewWatch{}, err
	}
	p.mu.Lock()
	p.watchers[id] = func() {
		offOwner()
		deb.stop()
		g.Close()
	}
	p.mu.Unlock()
	return PreviewWatch{ID: id, Path: watchDir}, nil
}

// Stops one watch, false when the id is unknown
func (p *PreviewWatches) Stop(id string) bool {
	p.mu.Lock()
	closer, ok := p.watchers[id]
	delete(p.watchers, id)
	p.mu.Unlock()
	if !ok {
		return false
	}
	closer()
	return true
}

// Stops every watch, the quit teardown
func (p *PreviewWatches) CloseAll() {
	p.mu.Lock()
	ids := make([]string, 0, len(p.watchers))
	for id := range p.watchers {
		ids = append(ids, id)
	}
	p.mu.Unlock()
	for _, id := range ids {
		p.Stop(id)
	}
}
