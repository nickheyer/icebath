package selfupdate

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// Journal in the install root naming every entry a ZIP swap stages or renames
const ZipSwapJournal = ".icebath-update-zip-swap"

const (
	zipStagingSuffix = ".icebath-update-staging"
	zipOldSuffix     = ".icebath-update-old"
)

var zipGenRE = regexp.MustCompile(`^[0-9a-f]{12}$`)

// Verdict of the ZIP swap owner lock, owned only while this process holds it
type ZipSwapLock struct {
	Owned  bool
	Reason string
}

// Takes the ZIP swap owner lock of root, waiting at most wait for a live owner, the release func nil when not owned
func zipSwapOwnerLock(root string, wait time.Duration) (ZipSwapLock, func()) {
	lockPath := filepath.Join(root, ZipSwapJournal+".lock")
	f, err := os.OpenFile(lockPath, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return ZipSwapLock{Reason: fmt.Sprintf("cannot open the ZIP swap lock %s: %s", lockPath, pypath.OSErrorText(err))}, nil
	}
	deadline := time.Now().Add(wait)
	for {
		held, lockErr := tryZipSwapLock(f)
		if lockErr != nil {
			f.Close()
			return ZipSwapLock{Reason: fmt.Sprintf("cannot lock the ZIP swap lock %s: %s", lockPath, pypath.OSErrorText(lockErr))}, nil
		}
		if !held {
			break
		}
		if !time.Now().Before(deadline) {
			f.Close()
			return ZipSwapLock{Reason: "busy"}, nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	return ZipSwapLock{Owned: true}, func() {
		lockClaimFile(f, false)
		f.Close()
	}
}

// Publishes the swap journal with its phase, entries, generation tag and backup temps
func WriteZipSwapJournal(root, phase string, entries []any, gen string, temps map[string]string) error {
	tempObj := pyjson.NewObject()
	keys := make([]string, 0, len(temps))
	for k := range temps {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		tempObj.Set(k, temps[k])
	}
	text, err := pyjson.Dumps(pyjson.ObjectOf("pid", os.Getpid(), "gen", gen, "phase", phase, "entries", entries, "temps", tempObj))
	if err != nil {
		return err
	}
	return WriteDurableText(filepath.Join(root, ZipSwapJournal), text)
}

// The entry's own identity from lstat, empty when absent or unidentifiable, size and mtime added for a settled file
func ZipEntryIdentity(path string, filling bool) string {
	info, err := os.Lstat(path)
	if err != nil {
		return ""
	}
	dev, ino, ok := fileIdentity(path, info)
	if !ok || ino == 0 {
		return ""
	}
	base := fmt.Sprintf("%d:%d:%o", dev, ino, uint32(info.Mode().Type()>>12&0xf)<<12)
	if filling || info.IsDir() {
		return base
	}
	return fmt.Sprintf("%s:%d:%d", base, info.Size(), mtimeNanos(info))
}

// Removes a staged tree or entry, a read only staged directory made writable and retried
func dropPath(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	if info.IsDir() && info.Mode()&os.ModeSymlink == 0 {
		if err := os.RemoveAll(path); err == nil {
			return nil
		}
		rwx := os.FileMode(0o700)
		if st, err := os.Stat(path); err == nil {
			os.Chmod(path, st.Mode().Perm()|rwx)
		}
		filepath.Walk(path, func(child string, fi os.FileInfo, err error) error {
			if err != nil || child == path {
				return nil
			}
			if fi.IsDir() && fi.Mode()&os.ModeSymlink == 0 {
				os.Chmod(child, fi.Mode().Perm()|rwx)
			} else if runtime.GOOS == "windows" && fi.Mode()&os.ModeSymlink == 0 && linkCountOf(fi) == 1 {
				os.Chmod(child, fi.Mode().Perm()|0o200)
			}
			return nil
		})
		return os.RemoveAll(path)
	}
	return os.Remove(path)
}

// One journaled entry, its name, prior existence and the staged and live identities
type zipJournalEntry struct {
	name    string
	existed bool
	staged  string
	live    string
}

// Parsed journal this code wrote, false for another writer's or a malformed one
func parseZipSwapJournal(raw string) (phase, gen string, entries []zipJournalEntry, temps map[string]string, ok bool) {
	var data map[string]any
	if err := json.Unmarshal([]byte(raw), &data); err != nil {
		return "", "", nil, nil, false
	}
	phase, _ = data["phase"].(string)
	if phase != "staging" && phase != "swapping" && phase != "committed" {
		return "", "", nil, nil, false
	}
	rawEntries, isList := data["entries"].([]any)
	gen, isGen := data["gen"].(string)
	if !isList || !isGen || !zipGenRE.MatchString(gen) {
		return "", "", nil, nil, false
	}
	for _, item := range rawEntries {
		row, isRow := item.([]any)
		if !isRow || len(row) != 4 {
			return "", "", nil, nil, false
		}
		name, okName := row[0].(string)
		existed, okExisted := row[1].(bool)
		staged, okStaged := row[2].(string)
		live, okLive := row[3].(string)
		if !okName || !okExisted || !okStaged || !okLive || name == "" || name == "." || name == ".." || strings.ContainsAny(name, `/\`) {
			return "", "", nil, nil, false
		}
		entries = append(entries, zipJournalEntry{name, existed, staged, live})
	}
	temps = map[string]string{}
	if rawTemps, present := data["temps"]; present {
		table, isMap := rawTemps.(map[string]any)
		if !isMap {
			return "", "", nil, nil, false
		}
		for k, v := range table {
			s, isText := v.(string)
			if !isText {
				return "", "", nil, nil, false
			}
			temps[k] = s
		}
	}
	return phase, gen, entries, temps, true
}

// Deletes path only when it is provably the entry the swap recorded, anything else kept aside
func discardUnlessOwned(path, identity string, kept *[]string) error {
	if _, err := os.Lstat(path); err != nil {
		return nil
	}
	if identity != "" && ZipEntryIdentity(path, false) == identity {
		return dropPath(path)
	}
	aside, err := keepAside(path)
	if err != nil {
		return err
	}
	*kept = append(*kept, aside)
	return nil
}

func lexists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

func isRegularNoFollow(path string) bool {
	info, err := os.Lstat(path)
	return err == nil && info.Mode().IsRegular()
}

func isDirFollow(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

// Finishes or rolls back one journaled entry, true when the live tree changed
func settleZipEntry(root, phase, gen string, entry zipJournalEntry, temps map[string]string, kept *[]string) (bool, error) {
	dst := filepath.Join(root, entry.name)
	staging, old := dst+zipStagingSuffix, dst+zipOldSuffix
	changed := false
	existed := entry.existed
	if phase == "swapping" {
		existed = existed || lexists(old)
		if existed && lexists(old) && !(entry.live != "" && ZipEntryIdentity(dst, false) == entry.live) {
			if isRegularNoFollow(old) && entry.staged != "" && ZipEntryIdentity(dst, false) == entry.staged && !isDirFollow(dst) {
				if err := os.Rename(old, dst); err != nil {
					return changed, err
				}
			} else {
				if err := discardUnlessOwned(dst, entry.staged, kept); err != nil {
					return changed, err
				}
				if err := os.Rename(old, dst); err != nil {
					return changed, err
				}
			}
			changed = true
		} else if !existed && lexists(dst) && !lexists(staging) {
			if err := discardUnlessOwned(dst, entry.staged, kept); err != nil {
				return changed, err
			}
			changed = true
		}
	} else if existed && !lexists(dst) && lexists(old) {
		if err := os.Rename(old, dst); err != nil {
			return changed, err
		}
		changed = true
	}
	for _, leftover := range [][2]string{{staging, entry.staged}, {old, entry.live}} {
		if err := discardUnlessOwned(leftover[0], leftover[1], kept); err != nil {
			return changed, err
		}
	}
	tmp := fmt.Sprintf("%s.%s.tmp", old, gen)
	if isKilledBackupCopy(tmp, dst, temps[filepath.Base(tmp)]) {
		if err := dropPath(tmp); err != nil {
			return changed, err
		}
	} else if lexists(tmp) {
		aside, err := keepAside(tmp)
		if err != nil {
			return changed, err
		}
		*kept = append(*kept, aside)
	}
	return changed, nil
}

// Whether tmp is the backup copy the swap recorded and its bytes start the live file
func isKilledBackupCopy(tmp, dst, identity string) bool {
	if identity == "" || ZipEntryIdentity(tmp, true) != identity || !isRegularNoFollow(dst) {
		return false
	}
	live, err := os.ReadFile(dst)
	if err != nil {
		return false
	}
	partial, err := os.ReadFile(tmp)
	if err != nil {
		return false
	}
	return bytes.HasPrefix(live, partial)
}

// Finishes or rolls back a ZIP swap whose owner died, true when the tree changed and a relaunch is due
func RestoreInterruptedZipSwap(root string, stream io.Writer) bool {
	if stream == nil {
		stream = os.Stderr
	}
	journal := filepath.Join(root, ZipSwapJournal)
	if !isRegularNoFollow(journal) || testOwnsLiveCheckout(root) {
		return false
	}
	owned, release := zipSwapOwnerLock(root, 0)
	if !owned.Owned && owned.Reason != "busy" {
		fmt.Fprintf(stream, "⚠ An interrupted ZIP update cannot be settled: %s.\n", owned.Reason)
	}
	if !owned.Owned {
		return false
	}
	defer release()
	if !isRegularNoFollow(journal) {
		return false
	}
	rawBytes, err := os.ReadFile(journal)
	if err != nil {
		fmt.Fprintf(stream, "⚠ Could not read the interrupted ZIP update's journal (%s); the next launch retries.\n", pypath.OSErrorText(err))
		return false
	}
	raw, _ := pystr.DecodeUTF8Sig(rawBytes)
	phase, gen, entries, temps, ok := parseZipSwapJournal(raw)
	if !ok {
		fmt.Fprintf(stream, "⚠ The interrupted ZIP update's journal %s is unreadable or from another version; "+
			"it and every `*%s` backup were kept. Put back what each backup replaced "+
			"(or reinstall), then delete the journal.\n", journal, zipOldSuffix)
		return false
	}
	changed, failed := false, false
	kept := []string{}
	for i := len(entries) - 1; i >= 0; i-- {
		entryChanged, err := settleZipEntry(root, phase, gen, entries[i], temps, &kept)
		changed = changed || entryChanged
		if err != nil {
			failed = true
			fmt.Fprintf(stream, "⚠ Could not settle %s after an interrupted ZIP update: %s\n", entries[i].name, pypath.OSErrorText(err))
		}
	}
	if len(kept) > 0 {
		fmt.Fprintf(stream, "⚠ An interrupted ZIP update's recovery found entries it could not prove were its own and "+
			"kept them aside instead of deleting them: %s. Delete each once you know it is not yours.\n", strings.Join(kept, ", "))
	}
	siblings := []string{zipStagingSuffix, zipOldSuffix, fmt.Sprintf("%s.%s.tmp", zipOldSuffix, gen)}
	unsettled := []string{}
	for _, e := range entries {
		for _, suffix := range siblings {
			if lexists(filepath.Join(root, e.name) + suffix) {
				unsettled = append(unsettled, e.name)
				break
			}
		}
	}
	if failed || len(unsettled) > 0 {
		if len(unsettled) > 0 && !failed {
			fmt.Fprintf(stream, "⚠ The interrupted ZIP update left %s unsettled; its journal %s stays and the next launch retries.\n",
				strings.Join(unsettled, ", "), journal)
		}
		return changed
	}
	if err := os.Remove(journal); err != nil && !errors.Is(err, os.ErrNotExist) {
		return changed
	}
	if changed {
		fmt.Fprintln(stream, "⚠ A previous ZIP `icebath update` was interrupted mid-swap; the old install was put back. "+
			"`icebath update` updates it again.")
	}
	return changed
}
