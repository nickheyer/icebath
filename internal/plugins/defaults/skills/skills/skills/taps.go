package skills

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"slices"

	"github.com/nickheyer/icebath/internal/skills"
	"github.com/nickheyer/icebath/pkg/fsx"
)

// GitHub repositories the user added to the ones the hub searches, in the order added
func (r *run) taps() ([]tap, error) {
	data, err := os.ReadFile(r.tapsPath())
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var file struct {
		Taps []tap `json:"taps"`
	}
	if err := json.Unmarshal(bytes.TrimPrefix(data, []byte("\uFEFF")), &file); err != nil {
		return nil, fmt.Errorf("The skill sources file %s can't be read: %w", r.tapsPath(), err)
	}
	return slices.DeleteFunc(file.Taps, func(t tap) bool { return t.Repo == "" }), nil
}

// Changes the user's taps while holding their file, written when change says they changed
func (r *run) updateTaps(ctx context.Context, change func(taps []tap) ([]tap, bool)) (bool, error) {
	release, err := skills.Hold(ctx, r.tapsPath()+".lock")
	if err != nil {
		return false, err
	}
	defer release()
	taps, err := r.taps()
	if err != nil {
		return false, err
	}
	taps, changed := change(taps)
	if !changed {
		return false, nil
	}
	if taps == nil {
		taps = []tap{}
	}
	data, err := json.MarshalIndent(map[string]any{"taps": taps}, "", "  ")
	if err != nil {
		return false, err
	}
	return true, fsx.Replace(r.tapsPath(), append(data, '\n'), 0o644)
}

// Adds a repository to the taps, false when it is there already
func (r *run) addTap(ctx context.Context, repo, path string) (bool, error) {
	return r.updateTaps(ctx, func(taps []tap) ([]tap, bool) {
		if slices.ContainsFunc(taps, func(t tap) bool { return t.Repo == repo }) {
			return taps, false
		}
		return append(taps, tap{Repo: repo, Path: path}), true
	})
}

// Removes a repository from the taps, false when it was not there
func (r *run) removeTap(ctx context.Context, repo string) (bool, error) {
	return r.updateTaps(ctx, func(taps []tap) ([]tap, bool) {
		kept := slices.DeleteFunc(slices.Clone(taps), func(t tap) bool { return t.Repo == repo })
		return kept, len(kept) != len(taps)
	})
}
