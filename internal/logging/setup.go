package logging

import (
	"context"
	"log/slog"
	"path/filepath"
	"slices"
	"time"

	"github.com/nickheyer/icebath/internal/home"
)

// Component a process logs for, which adds that component log file beside agent.log and errors.log
type Mode string

// Modes that add a component log file
const (
	// Adds gateway.log with the gateway sources
	ModeGateway Mode = "gateway"
	// Adds gui.log with the dashboard, desktop and terminal UI backend sources
	ModeGUI Mode = "gui"
)

// Bytes in the unit the configured rotation size is given in
const mebibyte = 1024 * 1024

// Logging settings of one Setup call, the level, size and backup count filled from the config logging section
type Options struct {
	Home        string
	Level       string
	MaxSizeMB   float64
	BackupCount int
	Mode        Mode
	Force       bool
}

// One shared log file Setup adds, its level, rotation and the component whose sources it takes
type fileSpec struct {
	entry     home.Entry
	level     slog.Level
	maxBytes  float64
	backups   int
	component string
	mode      Mode
}

// Adds the shared log files of a home and returns its logs directory, routing once a second home appears
func (s *System) Setup(ctx context.Context, o Options) (string, error) {
	h := o.Home
	if h == "" {
		var err error
		if h, err = s.resolver.Home(ctx); err != nil {
			return "", err
		}
	}
	logDir := home.Logs.In(h)
	if err := s.resolver.Mkdir(ctx, logDir); err != nil {
		return "", err
	}
	ref, err := s.ref(h)
	if err != nil {
		return "", err
	}
	if s.adoptSecondaryHome(ref) {
		return logDir, nil
	}
	level, ok := parseLevel(o.Level)
	if !ok {
		level = slog.LevelInfo
	}
	sizeMB := o.MaxSizeMB
	if sizeMB == 0 {
		sizeMB = 5
	}
	backups := o.BackupCount
	if backups == 0 {
		backups = 3
	}
	specs := []fileSpec{
		{entry: home.AgentLog, level: level, maxBytes: sizeMB * mebibyte, backups: backups},
		{entry: home.ErrorsLog, level: slog.LevelWarn, maxBytes: 2 * mebibyte, backups: 2},
		{entry: home.GatewayLog, level: slog.LevelInfo, maxBytes: 5 * mebibyte, backups: 3, component: "gateway", mode: ModeGateway},
		{entry: home.GUILog, level: slog.LevelInfo, maxBytes: 10 * mebibyte, backups: 5, component: "gui", mode: ModeGUI},
	}
	for _, spec := range specs {
		if spec.mode != "" && spec.mode != o.Mode {
			continue
		}
		if err := s.addFileSet(ref, spec); err != nil {
			return "", err
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.initialized && !o.Force {
		return logDir, nil
	}
	if s.root == levelNotSet || s.root > level {
		s.root = level
	}
	s.quietNoisyLocked()
	s.initialized = true
	s.publish()
	return logDir, nil
}

// Routes a further home to its own files when this process already logs for others, true when it did
func (s *System) adoptSecondaryHome(ref homeRef) bool {
	known := map[string]homeRef{}
	s.mu.Lock()
	for _, fs := range s.sets {
		known[fs.def.key] = fs.def
		for k, r := range fs.routed {
			known[k] = r
		}
	}
	s.mu.Unlock()
	if _, served := known[ref.key]; len(known) == 0 || served {
		return false
	}
	keys := make([]string, 0, len(known))
	for k := range known {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	homes := make([]string, 0, len(keys)+1)
	for _, k := range keys {
		homes = append(homes, known[k].path)
	}
	return s.EnableRouting(append(homes, ref.path))
}

// Adds one shared log file for the home unless a set already covers it, opening it at once on POSIX
func (s *System) addFileSet(ref homeRef, spec fileSpec) error {
	if s.covered(ref, spec.entry) {
		return nil
	}
	path := spec.entry.In(ref.path)
	if err := s.resolver.Mkdir(context.Background(), filepath.Dir(path)); err != nil {
		return err
	}
	f, err := s.openRotating(path, spec.maxBytes, spec.backups, home.ManagedSystem(ref.path) != "", false)
	if err != nil {
		return err
	}
	fs := &fileSet{entry: spec.entry, level: spec.level, maxBytes: spec.maxBytes, backups: spec.backups,
		def: ref, files: map[string]*rotatingFile{}, checked: map[string]time.Time{}}
	if spec.component != "" {
		fs.prefixes, _ = ComponentPrefixes(spec.component)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.coveredLocked(ref, spec.entry) {
		f.close()
		return nil
	}
	if s.routing {
		fs.routed = map[string]homeRef{}
		for _, other := range s.sets {
			fs.routed[other.def.key] = other.def
			for k, r := range other.routed {
				fs.routed[k] = r
			}
		}
		f.close()
	} else {
		fs.files[ref.key] = f
	}
	fs.since = s.q.next()
	s.sets = append(slices.Clip(s.sets), fs)
	s.publish()
	return nil
}

// Reports whether a set already writes the file of entry for the home
func (s *System) covered(ref homeRef, entry home.Entry) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.coveredLocked(ref, entry)
}

// Reports whether a set of the same file has the home as its default or routes it, caller holding mu
func (s *System) coveredLocked(ref homeRef, entry home.Entry) bool {
	for _, fs := range s.sets {
		if fs.entry != entry {
			continue
		}
		if fs.def.key == ref.key {
			return true
		}
		if _, ok := fs.routed[ref.key]; ok {
			return true
		}
	}
	return false
}

// Routes the shared log files by the home each record carries once two or more distinct homes are given
func (s *System) EnableRouting(homes []string) bool {
	var refs []homeRef
	for _, h := range homes {
		ref, err := s.ref(h)
		if err != nil || slices.ContainsFunc(refs, func(r homeRef) bool { return r.key == ref.key }) {
			continue
		}
		refs = append(refs, ref)
	}
	if len(refs) < 2 {
		return false
	}
	for _, ref := range refs {
		s.redact.ResolveHomePolicy(ref.path)
	}
	enabled := false
	s.run(func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		if len(s.sets) == 0 {
			return
		}
		enabled = true
		converting := !s.routing
		s.routing = true
		for _, fs := range s.sets {
			if fs.routed == nil {
				fs.routed = map[string]homeRef{}
			}
			for _, ref := range refs {
				fs.routed[ref.key] = ref
			}
			if converting {
				for k, f := range fs.files {
					f.close()
					delete(fs.files, k)
				}
			}
		}
	})
	return enabled
}

// Closes this process files of a profile home before the profile is removed and returns how many were open
func (s *System) ReleaseProfile(profileHome string) int {
	ref, err := s.ref(profileHome)
	if err != nil {
		return 0
	}
	released := 0
	s.run(func() {
		s.mu.Lock()
		routing, sets := s.routing, s.sets
		s.mu.Unlock()
		if !routing {
			return
		}
		for _, fs := range sets {
			if s.releaseIn(fs, ref.key) {
				released++
			}
		}
	})
	return released
}

// Log file of its own at path that records from the returned logger alone reach, one per path
type dedicated struct {
	level  slog.Level
	layout Layout
	file   *rotatingFile
}

// Logger writing only to its own rotating file at path, records below level dropped, never reaching the shared files
func (s *System) Dedicated(ctx context.Context, path string, maxBytes int64, backups int, level slog.Level, layout Layout) (*slog.Logger, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	d := s.dedicated[abs]
	s.mu.Unlock()
	if d != nil {
		return slog.New(&front{s: s, sink: d}), nil
	}
	if err := s.resolver.Mkdir(ctx, filepath.Dir(abs)); err != nil {
		return nil, err
	}
	f, err := s.openRotating(abs, float64(maxBytes), backups, false, false)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if existing := s.dedicated[abs]; existing != nil {
		f.close()
		return slog.New(&front{s: s, sink: existing}), nil
	}
	d = &dedicated{level: level, layout: layout, file: f}
	s.dedicated[abs] = d
	return slog.New(&front{s: s, sink: d}), nil
}
