package checkpoint

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
)

type MigrateResult struct {
	Scanned  int
	Migrated int
	Errors   int
}

func rebaseLedger(ledger map[string]any, oldWorkdir, newWorkdir string) map[string]any {
	out := make(map[string]any, len(ledger))
	for _, raw := range slices.Sorted(maps.Keys(ledger)) {
		if rel, ok := relativeTo(raw, oldWorkdir); ok {
			out[filepath.Join(newWorkdir, rel)] = ledger[raw]
			continue
		}
		out[raw] = ledger[raw]
	}
	return out
}

func (m *Manager) MigrateProfileProjects(ctx context.Context, oldProfileDir, newProfileDir string) (MigrateResult, error) {
	var res MigrateResult
	oldRoot, err := canonical(oldProfileDir)
	if err != nil {
		return res, err
	}
	newRoot, err := canonical(newProfileDir)
	if err != nil {
		return res, err
	}
	l := layoutOf(newRoot)
	if !l.hasHead() {
		return res, nil
	}
	available, err := m.gitAvailable(ctx)
	if err != nil {
		return res, err
	}
	if !available {
		res.Errors = 1
		return res, errors.New("Cannot migrate checkpoint projects after profile rename: git not found")
	}
	projects, err := listProjects(l)
	if err != nil {
		return res, err
	}
	var errs []error
	for _, p := range projects {
		res.Scanned++
		raw := p.workdir()
		if raw == "" {
			continue
		}
		oldWorkdir, err := canonical(raw)
		if err != nil {
			res.Errors++
			errs = append(errs, err)
			continue
		}
		rel, inside := relativeTo(oldWorkdir, oldRoot)
		if !inside {
			continue
		}
		newWorkdir := filepath.Join(newRoot, rel)
		if info, err := os.Stat(newWorkdir); err != nil || !info.IsDir() {
			continue
		}
		if err := m.rekeyProject(ctx, l, p, oldWorkdir, newWorkdir); err != nil {
			res.Errors++
			errs = append(errs, fmt.Errorf("Cannot migrate checkpoint project %s -> %s after profile rename: %w", oldWorkdir, newWorkdir, err))
			continue
		}
		res.Migrated++
	}
	return res, errors.Join(errs...)
}

func (m *Manager) rekeyProject(ctx context.Context, l layout, p project, oldWorkdir, newWorkdir string) error {
	newHash, err := projectHash(newWorkdir)
	if err != nil {
		return err
	}
	oldRef, newRef := refName(p.hash), refName(newHash)
	oldTip, err := m.refTip(ctx, l.store, newWorkdir, oldRef)
	if err != nil {
		return err
	}
	newTip, err := m.refTip(ctx, l.store, newWorkdir, newRef)
	if err != nil {
		return err
	}
	if oldTip != "" && newTip != "" {
		if _, err := m.runGit(ctx, l.store, newWorkdir, []string{"merge-base", "--is-ancestor", oldTip, newTip}, gitOpts{}); err != nil {
			if _, exited := exitCode(err); !exited {
				return err
			}
			return fmt.Errorf("target identity already exists: %s does not descend from %s", newRef, oldRef)
		}
	}
	metaPath := l.meta(newHash)
	if !exists(metaPath) {
		meta := maps.Clone(p.meta)
		meta["workdir"] = newWorkdir
		if dev, ino, ok := volumeEvidence(newWorkdir); ok {
			meta["workdir_parent_dev"] = dev
			meta["workdir_parent_ino"] = ino
		}
		if err := os.MkdirAll(l.projects, 0o777); err != nil {
			return err
		}
		if err := writeJSONObject(ctx, metaPath, meta); err != nil {
			return err
		}
	}
	oldLedger := l.ledger(p.hash)
	if exists(oldLedger) {
		ledger := rebaseLedger(loadLedger(l, p.hash), oldWorkdir, newWorkdir)
		maps.Copy(ledger, loadLedger(l, newHash))
		if err := saveLedger(ctx, l, newHash, ledger); err != nil {
			return err
		}
	}
	if oldTip != "" {
		if newTip == "" {
			if _, err := m.runGit(ctx, l.store, newWorkdir, []string{"update-ref", newRef, oldTip}, gitOpts{}); err != nil {
				return fmt.Errorf("could not create %s: %w", newRef, err)
			}
		}
		if err := m.deleteRef(ctx, l, oldRef); err != nil {
			return fmt.Errorf("could not delete %s: %w", oldRef, err)
		}
	}
	return errors.Join(removeIfPresent(l.meta(p.hash)), removeIfPresent(oldLedger), removeIfPresent(l.index(p.hash)))
}
