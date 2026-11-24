package selfupdate

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"

	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
	textutil "github.com/nickheyer/icebath/pkg/utils/text"
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
func writeDurableJSON(root, prefix, target string, payload *jsonutil.Object) error {
	text, err := jsonutil.Encode(payload, jsonutil.Indent(2), jsonutil.ASCII())
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

// Moves the installers' bootstrap receipt to the commit the stamp just published, only when one exists
func refreshBootstrapReceipt(root string, stamp *jsonutil.Object) error {
	receiptPath := filepath.Join(root, BootstrapReceipt)
	raw, err := os.ReadFile(receiptPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	previous := map[string]any{}
	if err == nil {
		if text, derr := textutil.DecodeUTF8(textutil.TrimBOM(raw)); derr == nil {
			var decoded any
			if json.Unmarshal([]byte(text), &decoded) == nil {
				if m, ok := decoded.(map[string]any); ok {
					previous = m
				}
			}
		}
	}
	receipt := jsonutil.NewObject()
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
	text, err := jsonutil.Encode(receipt, jsonutil.Indent(2), jsonutil.ASCII())
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
