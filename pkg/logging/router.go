package logging

import (
	"context"
	"path/filepath"
	"sync"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pythread"
)

// Seconds between delete checks of one routed profile home
const profileLivenessRecheckS = 2.0

var (
	monotonic             = pythread.Monotonic
	namedProfileIsDeleted = home.ProfileDeleted
	newFileHandlerFn      = newFileHandler
)

// Routes queued records to the log file of the home each record carries
type profileRoutingFileHandler struct {
	handlerCore
	baseFilename      string
	routedLogPath     string
	defaultHome       string
	filename          string
	maxBytes          float64
	backupCount       int
	mu                sync.Mutex
	profileHomes      map[string]bool
	profileHandlers   map[string]*managedRotatingFileHandler
	routerClosed      bool
	livenessCheckedAt map[string]float64
	pick              func(home string) (*managedRotatingFileHandler, error)
}

// Router taking over the path, level, rotation, formatter and filters of existing
func newProfileRoutingFileHandler(existing *managedRotatingFileHandler, profileHomes []string) *profileRoutingFileHandler {
	resolved := resolvePath(existing.baseFilename)
	r := &profileRoutingFileHandler{
		baseFilename:      resolved,
		routedLogPath:     resolved,
		defaultHome:       resolvePath(filepath.Dir(filepath.Dir(resolved))),
		filename:          filepath.Base(resolved),
		maxBytes:          existing.maxBytes,
		backupCount:       existing.backupCount,
		profileHomes:      map[string]bool{},
		profileHandlers:   map[string]*managedRotatingFileHandler{},
		livenessCheckedAt: map[string]float64{},
	}
	for _, h := range profileHomes {
		if resolvedHome, err := resolveHome(h); err == nil {
			r.profileHomes[resolvedHome] = true
		}
	}
	r.level = existing.Level()
	existing.mu.Lock()
	r.formatter = existing.formatter
	r.filters = append([]Filter(nil), existing.filters...)
	existing.mu.Unlock()
	r.pick = r.handlerForHome
	registerHandler(r)
	return r
}

// Lexically resolved path with symlinks followed where they exist, the Path.resolve form
func resolvePath(p string) string {
	if r, err := pypath.Realpath(p, pypath.Lax); err == nil {
		return r
	}
	return p
}

// Home path with the user expanded and symlinks resolved
func resolveHome(p string) (string, error) {
	expanded, err := pypath.PathExpanduser(p)
	if err != nil {
		return "", err
	}
	return pypath.Realpath(expanded, pypath.Lax)
}

// Home a record routes to, the default home unless it carries a live routed profile home
func (r *profileRoutingFileHandler) homeForRecord(rec *Record) string {
	candidate, err := resolveHome(rec.HermesHome)
	if err != nil {
		candidate = r.defaultHome
	}
	r.mu.Lock()
	routed := r.profileHomes[candidate]
	last, seen := r.livenessCheckedAt[candidate]
	r.mu.Unlock()
	if candidate == r.defaultHome || !routed {
		return r.defaultHome
	}
	if !seen || monotonic()-last >= profileLivenessRecheckS {
		if r.releaseIfDeleted(candidate) {
			return r.defaultHome
		}
	}
	return candidate
}

// Releases a home deleted out of band, true when it was
func (r *profileRoutingFileHandler) releaseIfDeleted(h string) bool {
	r.mu.Lock()
	r.livenessCheckedAt[h] = monotonic()
	r.mu.Unlock()
	if isDir(h) && !namedProfileIsDeleted(h) {
		return false
	}
	r.releaseProfile(h)
	return true
}

// Open handler for a home, nil once the router or that home is retired, built with no router lock held
func (r *profileRoutingFileHandler) handlerForHome(h string) (*managedRotatingFileHandler, error) {
	r.mu.Lock()
	handler := r.profileHandlers[h]
	r.mu.Unlock()
	if handler != nil {
		return handler, nil
	}
	built, err := newFileHandlerFn(context.Background(), filepath.Join(h, "logs", r.filename), r.Level(), r.maxBytes, r.backupCount, r.Formatter())
	if err != nil {
		return nil, err
	}
	built.lock.Lock()
	built.skipWriteWhenDirMissing = h != r.defaultHome
	built.neverReopenAfterClose = true
	built.lock.Unlock()
	r.mu.Lock()
	retired := r.routerClosed || (h != r.defaultHome && !r.profileHomes[h])
	handler = nil
	if !retired {
		if existing := r.profileHandlers[h]; existing != nil {
			handler = existing
		} else {
			r.profileHandlers[h] = built
			handler = built
		}
	}
	r.mu.Unlock()
	if handler != built {
		quietly(built.Close)
	}
	return handler, nil
}

// Filters and emits without the router's own I/O lock, each per home handler locking its own write
func (r *profileRoutingFileHandler) Handle(ctx context.Context, rec *Record) bool {
	if !r.filter(rec) {
		return false
	}
	r.emit(ctx, rec)
	return true
}

// Writes rec to its home's file under that home's redaction policy, falling back to the default home
func (r *profileRoutingFileHandler) emit(ctx context.Context, rec *Record) {
	h := r.homeForRecord(rec)
	handler, err := r.pick(h)
	if err == nil && handler == nil && h != r.defaultHome {
		h = r.defaultHome
		handler, err = r.pick(r.defaultHome)
	}
	if err != nil {
		handleErrorBase(rec, err)
		return
	}
	if handler == nil {
		return
	}
	if h == r.defaultHome {
		handler.Handle(ctx, rec)
		return
	}
	handler.Handle(home.WithOverride(ctx, h), rec)
	handler.lock.Lock()
	skipped := handler.skippedMissingDir
	handler.lock.Unlock()
	if skipped {
		r.releaseIfDeleted(h)
		fallback, err := r.pick(r.defaultHome)
		if err != nil {
			handleErrorBase(rec, err)
			return
		}
		if fallback != nil {
			fallback.Handle(ctx, rec)
		}
	}
}

// Closes the router for good along with every per home handler
func (r *profileRoutingFileHandler) Close() error {
	r.mu.Lock()
	r.routerClosed = true
	handlers := make([]*managedRotatingFileHandler, 0, len(r.profileHandlers))
	for _, h := range r.profileHandlers {
		handlers = append(handlers, h)
	}
	clear(r.profileHandlers)
	r.mu.Unlock()
	for _, h := range handlers {
		quietly(h.Close)
	}
	unregisterHandler(r)
	return nil
}

// Closes and forgets the routed files of a deleted profile, true when one was open
func (r *profileRoutingFileHandler) releaseProfile(h string) bool {
	r.mu.Lock()
	handler := r.profileHandlers[h]
	delete(r.profileHandlers, h)
	delete(r.profileHomes, h)
	delete(r.livenessCheckedAt, h)
	r.mu.Unlock()
	if handler == nil {
		return false
	}
	quietly(handler.Close)
	return true
}

// Profile homes the router routes besides its default home
func (r *profileRoutingFileHandler) routedHomes() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, 0, len(r.profileHomes))
	for h := range r.profileHomes {
		out = append(out, h)
	}
	return out
}
