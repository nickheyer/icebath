package home

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// Creates path with mode perm, missing parents at 0777 when parents is set, an existing directory accepted
func makeDir(path string, perm fs.FileMode, parents bool) error {
	err := mkdirLeaf(path, perm)
	if err == nil {
		return nil
	}
	if errors.Is(err, fs.ErrNotExist) && parents {
		if parent := filepath.Dir(path); parent != path {
			if perr := makeDir(parent, 0o777, true); perr != nil {
				return perr
			}
			if err = mkdirLeaf(path, perm); err == nil {
				return nil
			}
		}
	}
	if isDir(path) {
		return nil
	}
	return err
}

// Creates path, an existing directory accepted, keeping a directory an elevated Windows process makes usable unelevated
func (r *Resolver) MakeUserDir(ctx context.Context, path string, perm fs.FileMode, parents bool) error {
	return r.makeUserDir(ctx, filepath.Clean(path), perm, parents)
}

// Creates path and its parents as a user directory, refusing to bring back a deleted or missing named profile
func (r *Resolver) Mkdir(ctx context.Context, path string) error {
	if err := AssertProfileLive(path); err != nil {
		return err
	}
	return r.MakeUserDir(ctx, path, 0o777, true)
}

// Directories a fresh home receives, the older cache and pairing names included as the skeleton always made them
var homeSkeleton = [...]Entry{Cron, Sessions, Logs, LogsCurator, Memories, PairingStore.Old(), Hooks, ImageCache.Old(), AudioCache.Old(), Skills}

// Directories a managed home must already hold, the activation script owning their creation
var managedSkeleton = [...]Entry{Cron, Sessions, Logs, Memories}

// Home skeleton that could not be created or verified, never a malformed config document
type InitError struct {
	Path  string
	Links string
	Err   error
	seed  bool
}

// Path, cause and the repair the operator owns
func (e *InitError) Error() string {
	if e.seed {
		return fmt.Sprintf("Cannot initialize icebath home %s: %v. Check storage availability and access permissions.", e.Path, e.Err)
	}
	links := ""
	if e.Links != "" {
		links = "Directory links: " + e.Links + ". "
	}
	return fmt.Sprintf("Cannot initialize icebath directory %s: %v. %sCheck the directory/link target, mount availability "+
		"and access permissions; restore the mount or repair the link before retrying. icebath has not replaced the link "+
		"or created its missing target.", e.Path, e.Err, links)
}

// Cause of the failure
func (e *InitError) Unwrap() error { return e.Err }

// Ensures the home of ctx has its directory skeleton with secure modes, then runs seed for the default persona file
func (r *Resolver) EnsureHome(ctx context.Context, seed func(home string) error) error {
	home, err := r.Home(ctx)
	if err != nil {
		return err
	}
	if err := AssertProfileLive(home); err != nil {
		return err
	}
	r.initMu.Lock()
	defer r.initMu.Unlock()
	if r.ensured[home] && isDir(home) {
		return nil
	}
	if err := r.initializeHome(ctx, home, seed); err != nil {
		return err
	}
	real, err := resolvePath(home)
	if err != nil {
		return err
	}
	r.ensured[home] = true
	r.ensured[real] = true
	return nil
}

// Creates or verifies each skeleton directory, a managed install under umask 007 that never chmods
func (r *Resolver) initializeHome(ctx context.Context, home string, seed func(string) error) error {
	managed := ManagedSystem(home) != ""
	if managed {
		defer restoreUmask(setUmask(0o007))
	}
	if err := r.ensureDir(ctx, home, home, !managed, !managed); err != nil {
		return err
	}
	required := homeSkeleton[:]
	if managed {
		required = managedSkeleton[:]
	}
	for _, sub := range required {
		if err := r.ensureDir(ctx, sub.In(home), home, !managed, !managed); err != nil {
			return err
		}
	}
	if managed {
		if err := r.ensureDir(ctx, LogsCurator.In(home), home, true, false); err != nil {
			return err
		}
	}
	if err := seed(home); err != nil {
		return &InitError{Path: home, Err: err, seed: true}
	}
	return nil
}

// Path and each lexical ancestor of it that is a symlink, outermost first
func directoryLinks(path string) []string {
	var chain []string
	for c := path; ; c = filepath.Dir(c) {
		chain = append(chain, c)
		if filepath.Dir(c) == c {
			break
		}
	}
	var links []string
	for i := len(chain) - 1; i >= 0; i-- {
		if fi, err := os.Lstat(chain[i]); err == nil && fi.Mode()&fs.ModeSymlink != 0 {
			links = append(links, chain[i])
		}
	}
	return links
}

// Creates or verifies one skeleton directory, never materializing a missing link target, securing it unless the operator owns a link
func (r *Resolver) ensureDir(ctx context.Context, path, home string, create, secure bool) error {
	links := directoryLinks(path)
	detail := make([]string, 0, len(links))
	for _, l := range links {
		target, err := os.Readlink(l)
		if err != nil {
			return &InitError{Path: path, Err: err}
		}
		detail = append(detail, l+" -> "+target)
	}
	fail := func(err error) error { return &InitError{Path: path, Links: strings.Join(detail, "; "), Err: err} }
	for _, l := range links {
		if !isDir(l) {
			return fail(fmt.Errorf("Directory link is unavailable: %s", l))
		}
	}
	if create {
		if err := r.MakeUserDir(ctx, path, 0o777, true); err != nil {
			return fail(err)
		}
	} else if !isDir(path) {
		return fail(fmt.Errorf("Required directory does not exist: %s", path))
	}
	if secure && !operatorOwnsLink(links, home) {
		ApplySecureDirPolicy(path, home)
	}
	return nil
}

// Reports whether any link is the home itself or lies under it, making the operator own its modes
func operatorOwnsLink(links []string, home string) bool {
	for _, l := range links {
		if _, ok := within(l, home); ok {
			return true
		}
	}
	return false
}
