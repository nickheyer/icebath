package checkpoint

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const gcPendingName = ".gc-pending"

type pruner struct {
	m     *Manager
	store string
	cwd   string
}

func (p pruner) git(ctx context.Context, args []string, o gitOpts) (string, error) {
	out, err := p.m.runGit(ctx, p.store, p.cwd, args, o)
	if err != nil {
		if cerr := ctx.Err(); cerr != nil {
			return "", cerr
		}
		return "", &PruneError{msg: fmt.Sprintf("checkpoint %s failed: %s", args[0], err), err: err}
	}
	return out, nil
}

func (p pruner) reclaim(ctx context.Context) error {
	if _, err := p.git(ctx, []string{"reflog", "expire", "--expire=now", "--all"}, gitOpts{}); err != nil {
		return err
	}
	if _, err := p.git(ctx, []string{"gc", "--prune=now", "--quiet"}, gitOpts{scale: 3}); err != nil {
		return err
	}
	for _, name := range []string{"refs/heads", "branches"} {
		if err := os.MkdirAll(filepath.Join(p.store, filepath.FromSlash(name)), 0o777); err != nil {
			return err
		}
	}
	if err := os.Remove(filepath.Join(p.store, gcPendingName)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		slog.Debug("could not clear the checkpoint gc marker", "error", err)
	}
	return nil
}

func (p pruner) gcPending() bool {
	return exists(filepath.Join(p.store, gcPendingName))
}

func (p pruner) markGCPending() {
	path := filepath.Join(p.store, gcPendingName)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, 0o666)
	if err != nil {
		slog.Debug("could not mark the checkpoint store for gc", "error", err)
		return
	}
	if err := f.Close(); err != nil {
		slog.Debug("could not mark the checkpoint store for gc", "error", err)
	}
	now := time.Now()
	if err := os.Chtimes(path, now, now); err != nil {
		slog.Debug("could not mark the checkpoint store for gc", "error", err)
	}
}

func (p pruner) commits(ctx context.Context, ref string) ([]string, error) {
	out, err := p.git(ctx, []string{"rev-list", "--reverse", ref}, gitOpts{})
	if err != nil {
		return nil, err
	}
	commits := strings.Split(out, "\n")
	if out == "" {
		return nil, &PruneError{msg: "checkpoint ref has no readable history: " + ref}
	}
	return commits, nil
}

func (p pruner) rewrite(ctx context.Context, ref string, original, keep []string, gc bool) error {
	parent := ""
	for _, sha := range keep {
		metadata, err := p.git(ctx, []string{"log", "-1", "--format=%T%x00%aI%x00%cI%x00%s", sha}, gitOpts{})
		if err != nil {
			return err
		}
		fields := strings.SplitN(metadata, "\x00", 4)
		if len(fields) != 4 {
			return &PruneError{msg: "invalid checkpoint metadata: " + sha}
		}
		args := []string{"commit-tree", fields[0], "--no-gpg-sign", "-m", fields[3]}
		if parent != "" {
			args = append(args, "-p", parent)
		}
		parent, err = p.git(ctx, args, gitOpts{extra: map[string]string{"GIT_AUTHOR_DATE": fields[1], "GIT_COMMITTER_DATE": fields[2]}})
		if err != nil {
			return err
		}
		if parent == "" {
			return &PruneError{msg: "commit-tree returned no checkpoint identity"}
		}
	}
	if parent == "" {
		return &PruneError{msg: "cannot remove a project's last snapshot"}
	}
	if _, err := p.git(ctx, []string{"update-ref", ref, parent, original[len(original)-1]}, gitOpts{}); err != nil {
		return err
	}
	if gc {
		return p.reclaim(ctx)
	}
	p.markGCPending()
	return nil
}

func (p pruner) trim(ctx context.Context, ref string, keepCount int) error {
	commits, err := p.commits(ctx, ref)
	if err != nil {
		return err
	}
	keepCount = max(1, keepCount)
	if len(commits) > keepCount {
		return p.rewrite(ctx, ref, commits, commits[len(commits)-keepCount:], false)
	}
	return nil
}

func (p pruner) refs(ctx context.Context) ([]string, error) {
	out, err := p.git(ctx, []string{"for-each-ref", "--format=%(refname)", refsPrefix}, gitOpts{})
	if err != nil {
		return nil, err
	}
	if out == "" {
		return nil, nil
	}
	return strings.Split(out, "\n"), nil
}

func (p pruner) dropOneRound(ctx context.Context, capBytes int64) (bool, error) {
	if capBytes <= 0 || dirSize(p.store) <= capBytes {
		return false, nil
	}
	refs, err := p.refs(ctx)
	if err != nil {
		return false, err
	}
	changed := false
	for _, ref := range refs {
		commits, err := p.commits(ctx, ref)
		if err != nil {
			return changed, err
		}
		if len(commits) > 1 {
			if err := p.rewrite(ctx, ref, commits, commits[1:], false); err != nil {
				return changed, err
			}
			changed = true
		}
	}
	return changed, nil
}

func (p pruner) enforceSize(ctx context.Context, capBytes int64) (bool, error) {
	if capBytes <= 0 || dirSize(p.store) <= capBytes {
		return true, nil
	}
	if err := p.reclaim(ctx); err != nil {
		return false, err
	}
	if dirSize(p.store) <= capBytes {
		return true, nil
	}
	refs, err := p.refs(ctx)
	if err != nil {
		return false, err
	}
	for range 20 {
		changed := false
		for _, ref := range refs {
			commits, err := p.commits(ctx, ref)
			if err != nil {
				return false, err
			}
			if len(commits) <= 1 {
				continue
			}
			if err := p.rewrite(ctx, ref, commits, commits[1:], true); err != nil {
				return false, err
			}
			changed = true
			if dirSize(p.store) <= capBytes {
				return true, nil
			}
		}
		if !changed {
			break
		}
	}
	return false, nil
}

func (m *Manager) budget(ctx context.Context, store, workdir, ref string) {
	p := pruner{m: m, store: store, cwd: workdir}
	if err := p.trim(ctx, ref, m.maxSnapshots); err != nil {
		slog.Warn("Checkpoint pruning stopped", "error", err)
		return
	}
	dropped, err := p.dropOneRound(ctx, int64(m.maxTotalSizeMB)*mib)
	if err != nil {
		slog.Warn("Checkpoint pruning stopped", "error", err)
		return
	}
	if dropped {
		slog.Info(fmt.Sprintf("Checkpoint store exceeded %d MB, dropped the oldest snapshot per project; space is reclaimed by the next prune", m.maxTotalSizeMB))
	}
}
