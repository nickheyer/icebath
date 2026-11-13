package logging

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/nickheyer/icebath/internal/home"
)

// Longest a routed profile home goes between checks for a delete made outside this process
const livenessRecheck = 2 * time.Second

// Home a log file belongs to, by its comparison key and the resolved path its files go under
type homeRef struct {
	key  string
	path string
}

// One shared log file kind, the default home copy and the per-profile copies records are routed to
type fileSet struct {
	entry    home.Entry
	level    slog.Level
	maxBytes float64
	backups  int
	prefixes []string
	since    uint64
	def      homeRef
	routed   map[string]homeRef
	files    map[string]*rotatingFile
	checked  map[string]time.Time
}

// Reports whether records from the source name belong in this file
func (fs *fileSet) takes(name string) bool {
	return fs.prefixes == nil || underPrefix(name, fs.prefixes)
}

// Reference for a home path with the user expanded and every existing link followed
func (s *System) ref(path string) (homeRef, error) {
	abs, err := filepath.Abs(home.ExpandUser(path))
	if err != nil {
		return homeRef{}, err
	}
	real := resolveLax(abs)
	key, err := s.resolver.Key(real)
	if err != nil {
		return homeRef{}, err
	}
	return homeRef{key: key, path: real}, nil
}

// Path with its longest existing prefix resolved through every link and the missing rest kept as written
func resolveLax(p string) string {
	rest := ""
	for cur := p; ; {
		if real, err := filepath.EvalSymlinks(cur); err == nil {
			return filepath.Join(real, rest)
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return p
		}
		rest = filepath.Join(filepath.Base(cur), rest)
		cur = parent
	}
}

// Writes rec to its routed home file, falling back to the default home file when none applies or fails
func (s *System) deliver(fs *fileSet, rec *record, routing bool) {
	target, routed := fs.def, false
	if routing {
		target, routed = s.target(fs, rec)
	}
	if routed {
		line := s.redact.Redact(home.WithOverride(context.Background(), target.path), FileLayout.line(rec))
		f, err := s.fileFor(fs, target)
		switch {
		case err != nil:
			if !s.releaseIfDeleted(fs, target) {
				reportError(line, err)
			}
		case f != nil:
			f.append(line)
			if !f.skipped {
				return
			}
			s.releaseIfDeleted(fs, target)
		}
	}
	line := s.redact.Redact(context.Background(), FileLayout.line(rec))
	f, err := s.fileFor(fs, fs.def)
	if err != nil {
		reportError(line, err)
		return
	}
	f.append(line)
}

// Routed home rec belongs to, false when the record goes to the default home or its home was deleted
func (s *System) target(fs *fileSet, rec *record) (homeRef, bool) {
	if rec.home == "" {
		return fs.def, false
	}
	key, err := s.resolver.Key(rec.home)
	if err != nil || key == fs.def.key {
		return fs.def, false
	}
	s.mu.Lock()
	ref, ok := fs.routed[key]
	s.mu.Unlock()
	if !ok {
		return fs.def, false
	}
	if last, seen := fs.checked[key]; !seen || time.Since(last) >= livenessRecheck {
		if s.releaseIfDeleted(fs, ref) {
			return fs.def, false
		}
	}
	return ref, true
}

// Releases a routed home whose directory vanished or carries a tombstone, true when it did
func (s *System) releaseIfDeleted(fs *fileSet, ref homeRef) bool {
	fs.checked[ref.key] = time.Now()
	if st, err := os.Stat(ref.path); err == nil && st.IsDir() && !home.ProfileDeleted(ref.path) {
		return false
	}
	s.releaseIn(fs, ref.key)
	return true
}

// Closes and forgets the file of a home in one set, unrouting it, true when one was open
func (s *System) releaseIn(fs *fileSet, key string) bool {
	s.mu.Lock()
	delete(fs.routed, key)
	s.mu.Unlock()
	delete(fs.checked, key)
	f := fs.files[key]
	if f == nil {
		return false
	}
	delete(fs.files, key)
	f.close()
	return true
}

// File of a home in the set, opened on first use, nil without error for a home routing dropped
func (s *System) fileFor(fs *fileSet, ref homeRef) (*rotatingFile, error) {
	if f := fs.files[ref.key]; f != nil {
		return f, nil
	}
	if ref.key != fs.def.key {
		s.mu.Lock()
		_, live := fs.routed[ref.key]
		s.mu.Unlock()
		if !live {
			return nil, nil
		}
	}
	path := fs.entry.In(ref.path)
	if err := s.resolver.Mkdir(context.Background(), filepath.Dir(path)); err != nil {
		return nil, err
	}
	f, err := s.openRotating(path, fs.maxBytes, fs.backups, home.ManagedSystem(ref.path) != "", ref.key != fs.def.key)
	if err != nil {
		return nil, err
	}
	fs.files[ref.key] = f
	return f, nil
}

// Reports whether a source name is one of the prefixes or a dotted name below one
func underPrefix(name string, prefixes []string) bool {
	for _, p := range prefixes {
		if name == p || strings.HasPrefix(name, p+".") {
			return true
		}
	}
	return false
}
