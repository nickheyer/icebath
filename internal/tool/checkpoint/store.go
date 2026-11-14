package checkpoint

import (
	"cmp"
	"context"
	jsonv1 "encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/nickheyer/icebath/internal/fileutil"
)

func exists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

func nowSeconds() float64 {
	return float64(time.Now().UnixNano()) / 1e9
}

func mkdirExistOK(p string) error {
	if err := os.Mkdir(p, 0o777); err != nil && !errors.Is(err, fs.ErrExist) {
		return err
	}
	return nil
}

func (m *Manager) initStore(ctx context.Context, l layout) error {
	if !exists(l.store) {
		if err := os.MkdirAll(l.base, 0o777); err != nil {
			return fmt.Errorf("Could not create checkpoint base: %w", err)
		}
		if err := migrateLegacy(l.base); err != nil {
			return err
		}
	}
	if l.hasHead() {
		return nil
	}
	if err := os.MkdirAll(l.store, 0o777); err != nil {
		return err
	}
	for _, dir := range []string{l.indexes, l.projects} {
		if err := mkdirExistOK(dir); err != nil {
			return err
		}
	}
	if err := m.initBare(ctx, l.store); err != nil {
		return err
	}
	var errs []error
	for _, kv := range [][2]string{
		{"user.email", "icebath@local"},
		{"user.name", "icebath Checkpoint"},
		{"commit.gpgsign", "false"},
		{"tag.gpgSign", "false"},
		{"gc.auto", "0"},
	} {
		if _, err := m.runGit(ctx, l.store, l.base, []string{"config", kv[0], kv[1]}, gitOpts{}); err != nil {
			errs = append(errs, fmt.Errorf("checkpoint store config %s: %w", kv[0], err))
		}
	}
	info := filepath.Join(l.store, "info")
	if err := mkdirExistOK(info); err != nil {
		errs = append(errs, err)
	} else if err := fileutil.WriteText(ctx, filepath.Join(info, "exclude"), strings.Join(defaultExcludes, "\n")+"\n"); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

func migrateLegacy(base string) error {
	entries, err := os.ReadDir(base)
	if err != nil {
		return err
	}
	legacyRoot := ""
	for _, e := range entries {
		name := e.Name()
		if name == storeDirName || name == pruneMarkerName || strings.HasPrefix(name, legacyPrefix) {
			continue
		}
		if legacyRoot == "" {
			legacyRoot = filepath.Join(base, legacyPrefix+time.Now().Format("20060102-150405"))
			if err := os.MkdirAll(legacyRoot, 0o777); err != nil {
				slog.Warn("Could not create legacy archive dir", "error", err)
				return nil
			}
		}
		src := filepath.Join(base, name)
		if err := fileutil.Move(src, filepath.Join(legacyRoot, name)); err != nil {
			slog.Warn("Could not archive legacy checkpoint", "path", src, "error", err)
		}
	}
	if legacyRoot != "" {
		slog.Info("Migrated pre-v2 checkpoint repos. Clear with `icebath checkpoints clear-legacy` when safe.", "archive", legacyRoot)
	}
	return nil
}

func volumeEvidence(workdir string) (uint64, uint64, bool) {
	if !exists(workdir) {
		return 0, 0, false
	}
	parent, err := filepath.EvalSymlinks(filepath.Dir(workdir))
	if err != nil {
		return 0, 0, false
	}
	id, err := fileutil.LstatID(parent)
	if err != nil || id.Device == 0 || id.Inode == 0 {
		return 0, 0, false
	}
	return id.Device, id.Inode, true
}

func readJSONObject(path string) (map[string]any, error) {
	var v any
	if err := fileutil.ReadJSON(path, &v); err != nil {
		return nil, err
	}
	obj, ok := v.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("%s: not a JSON object", path)
	}
	return obj, nil
}

func writeJSONObject(ctx context.Context, path string, v any) error {
	data, err := fileutil.EncodeJSON(v, fileutil.JSONStyle{ASCII: true})
	if err != nil {
		return err
	}
	return fileutil.WriteFile(ctx, path, data)
}

func registerProject(ctx context.Context, l layout, workdir, hash string) error {
	path := l.meta(hash)
	now := nowSeconds()
	meta := map[string]any{"workdir": workdir, "created_at": now, "last_touch": now}
	dev, ino, ok := volumeEvidence(workdir)
	if ok {
		meta["workdir_parent_dev"] = dev
		meta["workdir_parent_ino"] = ino
	}
	if exists(path) {
		if existing, err := readJSONObject(path); err == nil {
			if created, has := existing["created_at"]; has {
				meta["created_at"] = created
			}
			if !ok {
				for _, key := range []string{"workdir_parent_dev", "workdir_parent_ino"} {
					if v, has := existing[key]; has {
						meta[key] = v
					}
				}
			}
		}
	}
	if err := os.MkdirAll(l.projects, 0o777); err != nil {
		return err
	}
	return writeJSONObject(ctx, path, meta)
}

func touchProject(ctx context.Context, l layout, workdir, hash string) error {
	path := l.meta(hash)
	if !exists(path) {
		return registerProject(ctx, l, workdir, hash)
	}
	meta, err := readJSONObject(path)
	if err != nil {
		meta = map[string]any{}
	}
	now := nowSeconds()
	meta["workdir"] = workdir
	meta["last_touch"] = now
	if _, has := meta["created_at"]; !has {
		meta["created_at"] = now
	}
	if dev, ino, ok := volumeEvidence(workdir); ok {
		meta["workdir_parent_dev"] = dev
		meta["workdir_parent_ino"] = ino
	}
	return writeJSONObject(ctx, path, meta)
}

type project struct {
	hash string
	meta map[string]any
}

func (p project) workdir() string {
	s, _ := p.meta["workdir"].(string)
	return s
}

func listProjects(l layout) ([]project, error) {
	entries, err := os.ReadDir(l.projects)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []project
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".json") {
			continue
		}
		stem := strings.TrimSuffix(name, ".json")
		if stem == "" {
			stem = name
		}
		meta, err := readJSONObject(filepath.Join(l.projects, name))
		if err != nil {
			continue
		}
		out = append(out, project{hash: stem, meta: meta})
	}
	return out, nil
}

func numberValue(v any) (float64, error) {
	switch n := v.(type) {
	case nil:
		return 0, nil
	case jsonv1.Number:
		return n.Float64()
	case float64:
		return n, nil
	case bool:
		if n {
			return 1, nil
		}
		return 0, nil
	case string:
		if n == "" {
			return 0, nil
		}
		return strconv.ParseFloat(strings.TrimSpace(n), 64)
	}
	return 0, fmt.Errorf("not a number: %v", v)
}

func optionalNumber(v any) *float64 {
	switch v.(type) {
	case jsonv1.Number, float64:
		f, err := numberValue(v)
		if err != nil {
			return nil
		}
		return &f
	}
	return nil
}

type preV2Repo struct {
	path             string
	workdir          string
	hasWorkdir       bool
	exists           bool
	markerUnreadable bool
}

func preV2Repos(base string) ([]preV2Repo, error) {
	entries, err := os.ReadDir(base)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []preV2Repo
	for _, e := range entries {
		name := e.Name()
		full := filepath.Join(base, name)
		if info, err := os.Stat(full); err != nil || !info.IsDir() {
			continue
		}
		if name == storeDirName || strings.HasPrefix(name, legacyPrefix) || !exists(filepath.Join(full, "HEAD")) {
			continue
		}
		repo := preV2Repo{path: full}
		marker := filepath.Join(full, workdirMarker)
		if exists(marker) {
			text, err := fileutil.ReadText(marker)
			if err != nil {
				repo.markerUnreadable = true
			} else {
				repo.workdir = strings.TrimSpace(text)
				repo.hasWorkdir = true
			}
		}
		repo.exists = repo.workdir != "" && exists(repo.workdir)
		out = append(out, repo)
	}
	return out, nil
}

func walkEntries(root string, visit func(path string, d fs.DirEntry) bool) {
	if resolved, err := filepath.EvalSymlinks(root); err == nil {
		root = resolved
	}
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if p == root {
				return err
			}
			return nil
		}
		if p == root {
			return nil
		}
		if !visit(p, d) {
			return fs.SkipAll
		}
		return nil
	})
	if err != nil {
		slog.Debug("checkpoint tree walk stopped", "root", root, "error", err)
	}
}

func dirFileCount(root string) int {
	count := 0
	walkEntries(root, func(string, fs.DirEntry) bool {
		count++
		return count <= maxFiles
	})
	return count
}

func dirSize(root string) int64 {
	var total int64
	walkEntries(root, func(p string, d fs.DirEntry) bool {
		if d.IsDir() {
			return true
		}
		if info, err := os.Stat(p); err == nil && info.Mode().IsRegular() {
			total += info.Size()
		}
		return true
	})
	return total
}

func newestMtime(root string) float64 {
	newest := 0.0
	walkEntries(root, func(p string, _ fs.DirEntry) bool {
		if info, err := os.Stat(p); err == nil {
			newest = max(newest, float64(info.ModTime().UnixNano())/1e9)
		}
		return true
	})
	return newest
}

func loadLedger(l layout, hash string) map[string]any {
	ledger, err := readJSONObject(l.ledger(hash))
	if err != nil {
		return map[string]any{}
	}
	return ledger
}

func ledgerTS(entry any) float64 {
	obj, ok := entry.(map[string]any)
	if !ok {
		return 0
	}
	ts, err := numberValue(obj["ts"])
	if err != nil {
		return 0
	}
	return ts
}

func saveLedger(ctx context.Context, l layout, hash string, ledger map[string]any) error {
	if len(ledger) > ledgerMaxEntries {
		keys := make([]string, 0, len(ledger))
		for k := range ledger {
			keys = append(keys, k)
		}
		slices.SortFunc(keys, func(a, b string) int {
			if c := cmp.Compare(ledgerTS(ledger[b]), ledgerTS(ledger[a])); c != 0 {
				return c
			}
			return strings.Compare(a, b)
		})
		kept := make(map[string]any, ledgerMaxEntries)
		for _, k := range keys[:ledgerMaxEntries] {
			kept[k] = ledger[k]
		}
		ledger = kept
	}
	if err := os.MkdirAll(l.ledgers, 0o777); err != nil {
		return err
	}
	return writeJSONObject(ctx, l.ledger(hash), ledger)
}
