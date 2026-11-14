// Fetches, verifies and publishes the pinned external programs, libraries and model files the product runs
package toolchain

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/internal/fileutil"
	"github.com/nickheyer/icebath/internal/home"
	"github.com/nickheyer/icebath/internal/hostinfo"
	"github.com/nickheyer/icebath/internal/httpclient"
)

// Variables that move the store and that switch installs on first use off
const (
	EnvRuntimeDir      = "ICEBATH_RUNTIME_DIR"
	EnvDisableLazy     = "ICEBATH_DISABLE_LAZY_INSTALLS"
	installLockName    = ".install.lock"
	stagingPrefix      = ".staging-"
	setAsidePrefix     = ".reclaim-"
	previousPrefix     = ".previous-"
	displacedPrefix    = ".displaced-"
	fetchPrefix        = "fetch-"
	lockNoticeAfter    = 2 * time.Second
	removeAttempts     = 5
	publishAttempts    = 5
	publishFirstDelay  = 500 * time.Millisecond
	downloadConnect    = 60 * time.Second
	downloadIdle       = 120 * time.Second
	sealedManifestName = "manifest.json"
)

// What a Store is built from
type Options struct {
	// Resolves the profile home and with it the root the store lives under
	Homes *home.Resolver
	// Machine facts that pick the artifact target
	Host *hostinfo.Host
	// Proxy and trust settings of download connections, timeouts left zero take the downloader's own
	HTTP httpclient.Options
	// Reports whether config allows installs on first use, an error refusing them
	LazyInstalls    func(ctx context.Context) (bool, error)
	DriverTelemetry func(ctx context.Context) (bool, error)
}

// Identity of a verified plain file, its digest with the inode, size and times it had
type fileSignature struct {
	digest string
	id     fileutil.FileID
	size   int64
	mtime  int64
}

// Catalog bound to a home resolver, the one value every install, lookup and listing goes through
type Store struct {
	homes     *home.Resolver
	host      *hostinfo.Host
	client    *http.Client
	lock      *lock
	recipes   map[string]*recipe
	order     []string
	lazy      func(ctx context.Context) (bool, error)
	telemetry func(ctx context.Context) (bool, error)

	targetOnce sync.Once
	target     hostTarget
	targetErr  error

	mu        sync.Mutex
	verified  map[string]fileSignature
	hostFixed map[string]hostRepair
}

// Builds a store over the embedded lock, refusing a lock and catalog that disagree
func New(o Options) (*Store, error) {
	if o.Homes == nil || o.Host == nil || o.LazyInstalls == nil {
		return nil, errors.New("toolchain: a home resolver, host facts and the lazy install policy are required")
	}
	pins, err := parseLock(embeddedLock)
	if err != nil {
		return nil, err
	}
	s := &Store{
		homes: o.Homes, host: o.Host, lock: pins, lazy: o.LazyInstalls, telemetry: o.DriverTelemetry,
		recipes: map[string]*recipe{}, verified: map[string]fileSignature{}, hostFixed: map[string]hostRepair{},
	}
	for _, r := range catalog() {
		if _, ok := pins.version(r.name); !ok {
			return nil, fmt.Errorf("toolchain: catalog entry %s has no pin in the lock", r.name)
		}
		s.recipes[r.name] = r
		s.order = append(s.order, r.name)
	}
	for _, name := range pins.names() {
		if voice, ok := strings.CutPrefix(name, piperVoicePrefix); ok && s.recipes[name] == nil {
			s.recipes[name] = piperVoiceRecipe(voice)
			s.order = append(s.order, name)
		}
	}
	sort.Strings(s.order)
	for _, name := range pins.names() {
		if s.recipes[name] == nil {
			return nil, fmt.Errorf("toolchain: the lock pins %s, which the catalog does not define", name)
		}
	}
	if o.HTTP.Timeouts == (httpclient.Timeouts{}) {
		o.HTTP.Timeouts = httpclient.Timeouts{Connect: downloadConnect, TLS: downloadConnect, Write: downloadConnect, Header: downloadIdle, Idle: downloadIdle}
	}
	transport, err := httpclient.New(o.HTTP)
	if err != nil {
		return nil, fmt.Errorf("toolchain: %w", err)
	}
	s.client = &http.Client{Transport: transport, CheckRedirect: downloadRedirects}
	return s, nil
}

// Recipe of a catalog name
func (s *Store) recipe(name string) (*recipe, error) {
	r := s.recipes[name]
	if r == nil {
		return nil, fmt.Errorf("%s: %w", name, ErrUnknown)
	}
	return r, nil
}

// Target and C library of this machine, detected once
func (s *Store) hostTarget(ctx context.Context) (hostTarget, error) {
	s.targetOnce.Do(func() { s.target, s.targetErr = detectTarget(ctx, s.host) })
	return s.target, s.targetErr
}

// Root every profile of the install shares, from the home in effect for ctx
func (s *Store) sharedRoot(ctx context.Context) (string, error) {
	h, err := s.homes.Home(ctx)
	if err != nil {
		return "", err
	}
	return home.RootOf(h)
}

// Store root this process reads first and the root it may write, the two differing for a sealed payload
func (s *Store) roots(ctx context.Context) (primary, writable string, err error) {
	root, err := s.sharedRoot(ctx)
	if err != nil {
		return "", "", err
	}
	own := home.ToolStore.In(root)
	primary = own
	if override := os.Getenv(EnvRuntimeDir); override != "" {
		if primary, err = filepath.Abs(override); err != nil {
			return "", "", err
		}
	}
	if sealedStore(primary) {
		return primary, own, nil
	}
	return primary, primary, nil
}

// Reports a store shipped read only beside a payload manifest
func sealedStore(root string) bool {
	fi, err := os.Stat(filepath.Join(filepath.Dir(root), sealedManifestName))
	return err == nil && fi.Mode().IsRegular()
}

// Store roots in lookup order without repeats
func (s *Store) searchRoots(ctx context.Context) ([]string, error) {
	primary, writable, err := s.roots(ctx)
	if err != nil {
		return nil, err
	}
	if primary == writable {
		return []string{primary}, nil
	}
	return []string{primary, writable}, nil
}

// Directory resumable partial downloads of store installs are kept in
func (s *Store) partialsRoot(ctx context.Context) (string, error) {
	root, err := s.sharedRoot(ctx)
	if err != nil {
		return "", err
	}
	return home.DownloadPartials.In(root), nil
}

// Downloader over the store transport keeping partials in dir
func (s *Store) downloader(dir string, connections int) *downloader {
	if connections < 1 {
		connections = defaultConnections
	}
	return &downloader{client: s.client, partials: dir, connections: connections, mirrorHost: hostOf(s.lock.mirror.Origin)}
}

// Source of a pinned download with the content addressed mirror as its fallback
func (s *Store) pinnedSource(a Artifact, dest string) Source {
	src := Source{URL: a.URL, Dest: dest, SHA256: a.SHA256, Size: a.Size}
	if mirror := s.lock.mirrorURL(a.SHA256); mirror != "" && mirror != a.URL {
		src.Fallbacks = []string{mirror}
	}
	return src
}

// Takes the writer lock of a store root, telling the caller when another install holds it
func (s *Store) lockRoot(ctx context.Context, root string, notify func(Progress)) (*fileutil.Lock, error) {
	if err := os.MkdirAll(root, 0o777); err != nil {
		return nil, err
	}
	path := filepath.Join(root, installLockName)
	first, cancel := context.WithTimeout(ctx, lockNoticeAfter)
	l, err := fileutil.OpenLock(first, path, fileutil.LockPerm(0o600), fileutil.RecordHolder())
	cancel()
	if err == nil {
		return l, nil
	}
	if ctx.Err() != nil || !errors.Is(err, fileutil.ErrLocked) {
		return nil, err
	}
	slog.Info(fmt.Sprintf("waiting for %s (another install holds it)", path))
	if notify != nil {
		notify(Progress{Stage: StageWait, Label: path})
	}
	return fileutil.OpenLock(ctx, path, fileutil.LockPerm(0o600), fileutil.RecordHolder())
}

// Thirty two random hex digits for a name no other process picks
func randomName() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

// Renames a staged directory to its published name, a concurrent winner's directory kept
func publish(ctx context.Context, staged, target string) error {
	isDir := func() bool {
		fi, err := os.Stat(target)
		return err == nil && fi.IsDir()
	}
	delay := publishFirstDelay
	for attempt := 0; attempt < publishAttempts; attempt++ {
		if os.Rename(staged, target) == nil || isDir() {
			return nil
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
		delay *= 2
	}
	if err := os.Rename(staged, target); err != nil && !isDir() {
		return err
	}
	return nil
}

// Removes a replaced or failed directory of root, riding out short holds and reporting what survives
func removeEntry(ctx context.Context, root, name string, attempts int) error {
	path := filepath.Join(root, name)
	var last error
	for attempt := 0; attempt < attempts; attempt++ {
		last = removeAll(path)
		if last == nil || errors.Is(last, fs.ErrNotExist) {
			return nil
		}
		if attempt == attempts-1 {
			break
		}
		timer := time.NewTimer(time.Duration(attempt+1) * 200 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
	return last
}

// Drops a directory a finished publish left behind, renaming aside one a running program still maps
func discardEntry(ctx context.Context, root, name string) {
	err := removeEntry(ctx, root, name, removeAttempts)
	if err == nil {
		return
	}
	aside, nameErr := randomName()
	if nameErr != nil || os.Rename(filepath.Join(root, name), filepath.Join(root, setAsidePrefix+aside)) != nil {
		slog.Warn(fmt.Sprintf("could not remove or set aside %s: %v", name, err))
		return
	}
	slog.Warn(fmt.Sprintf("%s is still in use (%v); set aside for the next install or `icebath pm gc`", name, err))
}

// Deletes the set aside directories whose hold is gone, under the store lock
func reclaimSetAside(ctx context.Context, root string) int {
	entries, err := os.ReadDir(root)
	if err != nil {
		return 0
	}
	removed := 0
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), setAsidePrefix) && removeEntry(ctx, root, e.Name(), 1) == nil {
			removed++
		}
	}
	return removed
}

// Deletes the scratch directories a killed installer left, under the store lock
func removeStaleScratch(root string) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return
	}
	for _, e := range entries {
		if e.IsDir() && strings.HasPrefix(e.Name(), stagingPrefix) {
			_ = fileutil.RemoveTree(filepath.Join(root, e.Name()), true)
		}
	}
}
