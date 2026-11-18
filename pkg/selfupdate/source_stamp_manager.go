//go:build !icebath_core && !icebath_noextras

package selfupdate

import (
	"errors"
	"os"
	"path/filepath"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/selfupdate/updaterecord"
)

// Replaces install-stamp.json with identity read from the checkout itself, nil when git cannot identify it
func WriteSourceStamp(root string, adopted bool) (*pyjson.Object, error) {
	resolved, err := pypath.Resolve(root)
	if err != nil {
		return nil, err
	}
	root = resolved
	info := updaterecord.GitVersionInfo(root, true)
	stampPath := filepath.Join(root, "install-stamp.json")
	if info.Commit == nil {
		if err := os.Remove(stampPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
		updaterecord.ResetVersionInfoCache()
		return nil, nil
	}
	builtAt := pyISONowUTC()
	stamp := pyjson.ObjectOf(
		"schemaVersion", 2,
		"commit", *info.Commit,
		"commitDate", optionalValue(info.CommitDate),
		"branch", optionalValue(info.Branch),
		"builtAt", builtAt,
		"dirty", info.Dirty,
		"source", "git",
		"distribution", nil,
		"updateMechanism", "self",
		"baseVersion", info.BaseVersion,
		"displayVersion", info.DerivedVersion,
		"distance", optionalValue(info.Distance),
		"payload", "bootstrap",
		"tag", nil,
	)
	if adopted {
		stamp.Set("adoptedAt", builtAt)
	}
	if err := writeDurableJSON(root, ".install-stamp.", stampPath, stamp); err != nil {
		return nil, err
	}
	updaterecord.ResetVersionInfoCache()
	if err := refreshBootstrapReceipt(root, stamp); err != nil {
		return nil, err
	}
	return stamp, nil
}
