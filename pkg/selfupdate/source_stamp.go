package selfupdate

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/selfupdate/updaterecord"
)

// Installers' receipt file a published checkout identity moves with it
const BootstrapReceipt = ".icebath-bootstrap-complete"

// Nil for an absent optional value, the value otherwise
func optionalValue[T any](v *T) any {
	if v == nil {
		return nil
	}
	return *v
}

// Writes a JSON document durably through a mkstemp sibling, the directory synced after the rename
func writeDurableJSON(root, prefix, target string, payload *pyjson.Object) error {
	text, err := pyjson.Dumps(payload, pyjson.Indent(2))
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(root, prefix+"*.tmp")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if _, err := tmp.WriteString(text + "\n"); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(name, target); err != nil {
		return err
	}
	if dir, err := os.Open(root); err == nil {
		dir.Sync()
		dir.Close()
	}
	return nil
}

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

// Moves the installers' bootstrap receipt to the commit the stamp just published, only when one exists
func refreshBootstrapReceipt(root string, stamp *pyjson.Object) error {
	receiptPath := filepath.Join(root, BootstrapReceipt)
	raw, err := os.ReadFile(receiptPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	previous := map[string]any{}
	if err == nil {
		if text, derr := pystr.DecodeUTF8Sig(raw); derr == nil {
			var decoded any
			if json.Unmarshal([]byte(text), &decoded) == nil {
				if m, ok := decoded.(map[string]any); ok {
					previous = m
				}
			}
		}
	}
	receipt := pyjson.NewObject()
	for k, v := range previous {
		receipt.Set(k, v)
	}
	commit, _ := stamp.Get("commit")
	branch, _ := stamp.Get("branch")
	if branch == nil {
		branch = previous["pinnedBranch"]
	}
	now := time.Now().UTC()
	receipt.Set("schemaVersion", 1)
	receipt.Set("pinnedCommit", commit)
	receipt.Set("pinnedBranch", branch)
	receipt.Set("completedAt", now.Format("2006-01-02T15:04:05.000")+"Z")
	text, err := pyjson.Dumps(receipt, pyjson.Indent(2))
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(root, BootstrapReceipt+".*.tmp")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if _, err := tmp.WriteString(text + "\n"); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(name, receiptPath)
}
