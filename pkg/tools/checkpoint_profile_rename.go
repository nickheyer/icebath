package tools

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

// Counts the checkpoint projects a profile rename scanned, rekeyed and failed
type CheckpointRenameResult struct {
	Scanned  int
	Migrated int
	Errors   int
}

// Moves absolute ledger keys under the old workdir to the matching path under the new one
func rebaseLedgerPaths(ledger map[string]any, oldWorkdir, newWorkdir string) map[string]any {
	rebased := make(map[string]any, len(ledger))
	for raw, entry := range ledger {
		if rel, ok := relativeTo(raw, oldWorkdir); ok {
			rebased[filepath.Join(newWorkdir, rel)] = entry
			continue
		}
		rebased[raw] = entry
	}
	return rebased
}

// Installs the project under its new hash, folds the ledger, then drops the old identity, refusing a foreign target history
func rekeyCheckpointProject(env *Env, store string, meta map[string]any, oldWorkdir, newWorkdir string) error {
	oldHash, _ := meta["_hash"].(string)
	newHash := checkpointProjectHash(newWorkdir)
	oldRef, newRef := checkpointRefName(oldHash), checkpointRefName(newHash)
	oldTip := checkpointRefTip(env, store, newWorkdir, oldRef)
	newTip := checkpointRefTip(env, store, newWorkdir, newRef)
	if oldTip != "" && newTip != "" {
		ok, _, _ := runCheckpointGit(env, []string{"merge-base", "--is-ancestor", oldTip, newTip}, store, newWorkdir,
			checkpointGitOpts{Allowed: map[int]bool{1: true}})
		if !ok {
			return fmt.Errorf("target identity already exists: %s does not descend from %s", newRef, oldRef)
		}
	}
	metaPath := checkpointProjectMetaPath(store, newHash)
	if _, err := os.Stat(metaPath); err != nil {
		newMeta := map[string]any{}
		for k, v := range meta {
			if k != "_hash" {
				newMeta[k] = v
			}
		}
		newMeta["workdir"] = newWorkdir
		for k, v := range checkpointVolumeEvidence(newWorkdir) {
			newMeta[k] = v
		}
		if err := os.MkdirAll(filepath.Dir(metaPath), 0o777); err != nil {
			return err
		}
		data, err := checkpointJSONEncode(newMeta)
		if err != nil {
			return err
		}
		if err := os.WriteFile(metaPath, []byte(data), 0o666); err != nil {
			return err
		}
	}
	oldLedgerPath := checkpointLedgerPath(store, oldHash)
	if _, err := os.Stat(oldLedgerPath); err == nil {
		ledger := rebaseLedgerPaths(loadCheckpointLedger(store, oldHash), oldWorkdir, newWorkdir)
		for k, v := range loadCheckpointLedger(store, newHash) {
			ledger[k] = v
		}
		if err := saveCheckpointLedger(env, store, newHash, ledger); err != nil {
			return err
		}
	}
	if oldTip != "" {
		if newTip == "" {
			ok, _, errText := runCheckpointGit(env, []string{"update-ref", newRef, oldTip}, store, newWorkdir, checkpointGitOpts{})
			if !ok {
				return fmt.Errorf("could not create %s: %s", newRef, errText)
			}
		}
		if !checkpointDeleteRef(env, store, oldRef) {
			return fmt.Errorf("could not delete %s", oldRef)
		}
	}
	checkpointUnlinkQuiet(checkpointProjectMetaPath(store, oldHash))
	checkpointUnlinkQuiet(oldLedgerPath)
	checkpointUnlinkQuiet(checkpointIndexPath(store, oldHash))
	return nil
}

// Rekeys checkpoint projects whose workdirs moved with a profile rename
func MigrateProfileCheckpointProjects(env *Env, oldProfileDir, newProfileDir string) CheckpointRenameResult {
	oldRoot := checkpointNormalizePath(oldProfileDir)
	newRoot := checkpointNormalizePath(newProfileDir)
	store := checkpointStorePath(filepath.Join(newRoot, "checkpoints"))
	var result CheckpointRenameResult
	if !checkpointStoreHasHead(store) {
		return result
	}
	if _, err := exec.LookPath("git"); err != nil {
		toolsLogger(env).Warn("Cannot migrate checkpoint projects after profile rename: git not found")
		result.Errors = 1
		return result
	}
	for _, meta := range listCheckpointProjects(store) {
		result.Scanned++
		raw := meta["workdir"]
		if !checkpointTruthy(raw) {
			continue
		}
		oldWorkdir := checkpointNormalizePath(pyStr(raw))
		rel, ok := relativeTo(oldWorkdir, oldRoot)
		if !ok {
			continue
		}
		newWorkdir := filepath.Join(newRoot, rel)
		if st, err := os.Stat(newWorkdir); err != nil || !st.IsDir() {
			continue
		}
		if err := rekeyCheckpointProject(env, store, meta, oldWorkdir, newWorkdir); err != nil {
			result.Errors++
			toolsLogger(env).Warn(fmt.Sprintf("Cannot migrate checkpoint project %s -> %s after profile rename: %v", oldWorkdir, newWorkdir, err))
			continue
		}
		result.Migrated++
	}
	return result
}
