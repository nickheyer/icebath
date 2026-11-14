package toolchain

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/nickheyer/icebath/internal/fileutil"
)

const (
	stagePrefix  = "stage-"
	stagePinName = ".pm-stage-pin.json"
)

var ErrStoresOverlap = errors.New("tool stores must be disjoint")

func stagePin(target Target, identity []string) (string, error) {
	t, err := json.Marshal(string(target))
	if err != nil {
		return "", err
	}
	shas := make([]string, 0, len(identity))
	for _, sha := range identity {
		quoted, err := json.Marshal(sha)
		if err != nil {
			return "", err
		}
		shas = append(shas, string(quoted))
	}
	return `{"target": ` + string(t) + `, "sha256": [` + strings.Join(shas, ", ") + `]}`, nil
}

func (s *Store) stageCurrent(ctx context.Context, r *recipe, entry, pin string, host hostTarget) bool {
	data, err := os.ReadFile(filepath.Join(entry, stagePinName))
	if err != nil {
		return false
	}
	return string(bytes.TrimPrefix(data, []byte("\xef\xbb\xbf"))) == pin && s.verifyEntry(ctx, r, entry, host.target, host.runs()) == ""
}

func (s *Store) copyVerifiedSource(ctx context.Context, r *recipe, from *location, version string, host hostTarget, staged string) error {
	source, ok := from.facts.get(r.name)
	if !ok || !from.facts.installed(r.name, version, s.identity(r.name, host), host.target, from.root) || !s.entryVerified(ctx, r, source, from.root, host) {
		return &InstallError{Name: r.name, Cause: "bundled copy source failed verification"}
	}
	if err := fileutil.CopyTree(filepath.Join(from.root, source.Entry), staged, fileutil.CopyTreeOptions{Symlinks: true}); err != nil {
		return installFailure(r.name, err)
	}
	if !digestMatches(staged, source.Digest) {
		return &InstallError{Name: r.name, Cause: "copied bytes do not match the bundled source"}
	}
	return nil
}

func (s *Store) targetFor(ctx context.Context, target Target) (hostTarget, error) {
	machine, err := s.hostTarget(ctx)
	if err != nil {
		return hostTarget{}, err
	}
	if target == machine.target {
		return machine, nil
	}
	return hostTarget{target: target, machine: machine.target}, nil
}

func (s *Store) Stage(ctx context.Context, name string, target Target, o EnsureOptions) (string, error) {
	r, err := s.recipe(name)
	if err != nil {
		return "", err
	}
	host, err := s.targetFor(ctx, target)
	if err != nil {
		return "", err
	}
	_, writable, err := s.roots(ctx)
	if err != nil {
		return "", err
	}
	report := func(p Progress) {
		if o.Progress != nil {
			p.Name = name
			o.Progress(p)
		}
	}
	return s.realize(ctx, r, host, writable, report, realizeMode{stage: true})
}

func resolvedDir(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	if real, err := filepath.EvalSymlinks(abs); err == nil {
		return real, nil
	}
	return abs, nil
}

func within(path, root string) bool {
	rel, err := filepath.Rel(root, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func linksStayInside(name, entry string) error {
	return filepath.WalkDir(entry, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if p == entry || (d.Type()&fs.ModeSymlink == 0 && !fileutil.IsJunction(p)) {
			return nil
		}
		target, err := os.Readlink(p)
		escapes := err != nil || filepath.IsAbs(target)
		if !escapes {
			resolved, rerr := filepath.EvalSymlinks(p)
			escapes = rerr != nil || !within(resolved, entry)
		}
		if escapes {
			return &InstallError{Name: name, Cause: "tool copy link escapes entry: " + p}
		}
		if d.IsDir() {
			return filepath.SkipDir
		}
		return nil
	})
}

func (s *Store) verifiedSource(ctx context.Context, r *recipe, f *facts, source string, host hostTarget) (fact, error) {
	failed := &InstallError{Name: r.name, Cause: "tool source failed verification", Remedy: "run preparation again"}
	version, _ := s.lock.version(r.name)
	identity := s.identity(r.name, host)
	if version == "" || identity == nil {
		return fact{}, &InstallError{Name: r.name, Cause: "tool source failed verification: missing pin"}
	}
	row, ok := f.get(r.name)
	expected := r.directory(version, host.target)
	entry := filepath.Join(source, expected)
	if !ok || row.Entry != expected {
		return fact{}, failed
	}
	if fi, err := os.Stat(entry); err != nil || !fi.IsDir() {
		return fact{}, failed
	}
	if real, err := filepath.EvalSymlinks(entry); err != nil || !within(real, source) {
		return fact{}, failed
	}
	if !f.installed(r.name, version, identity, host.target, source) || !digestMatches(entry, row.Digest) {
		return fact{}, failed
	}
	if err := linksStayInside(r.name, entry); err != nil {
		return fact{}, err
	}
	broken := &InstallError{Name: r.name, Cause: "tool source failed verification: binary or environment"}
	if _, pattern := r.binaries(entry, host.target); pattern != "" {
		binary := r.binary(entry, host.target)
		if binary == "" {
			return fact{}, broken
		}
		fi, err := os.Stat(binary)
		if err != nil || !fi.Mode().IsRegular() {
			return fact{}, broken
		}
		if real, err := filepath.EvalSymlinks(binary); err != nil || !within(real, entry) {
			return fact{}, broken
		}
	}
	if !equalEnv(f.env(r.name, source), r.environment(entry, host.target)) {
		return fact{}, broken
	}
	return row, nil
}

func (s *Store) CopyVerified(ctx context.Context, names []string, source, out string, target Target) error {
	source, err := resolvedDir(source)
	if err != nil {
		return err
	}
	if out, err = resolvedDir(out); err != nil {
		return err
	}
	if within(source, out) || within(out, source) {
		return ErrStoresOverlap
	}
	if fi, err := os.Stat(filepath.Join(source, factsName)); err != nil || !fi.Mode().IsRegular() {
		return &InstallError{Name: "tools", Cause: "source facts missing: " + source}
	}
	recipes := make([]*recipe, 0, len(names))
	for _, name := range names {
		r, err := s.recipe(name)
		if err != nil {
			return err
		}
		if r.inHome() {
			return fmt.Errorf("%s: copies accept tool entries, not application state", name)
		}
		recipes = append(recipes, r)
	}
	host, err := s.targetFor(ctx, target)
	if err != nil {
		return err
	}
	roots := []string{source, out}
	slices.Sort(roots)
	for _, root := range roots {
		l, err := s.lockRoot(ctx, root, nil)
		if err != nil {
			return err
		}
		defer l.Release()
	}
	f := &facts{path: filepath.Join(source, factsName)}
	if err := f.reload(); err != nil {
		return err
	}
	rows := make([]fact, len(recipes))
	for i, r := range recipes {
		if rows[i], err = s.verifiedSource(ctx, r, f, source, host); err != nil {
			return err
		}
	}
	for i, r := range recipes {
		if !s.entryVerified(ctx, r, rows[i], source, host) {
			return &InstallError{Name: r.name, Cause: "tool copy source failed verification", Remedy: "run preparation again"}
		}
	}
	from := &location{root: source, facts: f}
	for _, r := range recipes {
		started := time.Now()
		if _, err := s.realize(ctx, r, host, out, func(Progress) {}, realizeMode{from: from, lockHeld: true}); err != nil {
			return err
		}
		slog.Info(fmt.Sprintf("    copied %s (%.1fs)", r.name, time.Since(started).Seconds()))
	}
	return nil
}
