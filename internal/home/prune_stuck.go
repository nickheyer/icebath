package home

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"time"

	"github.com/nickheyer/icebath/internal/fileutil"
	"github.com/nickheyer/icebath/internal/shell"
)

// Stuck entry state file, its lock, the weekly recheck and the blockers one record keeps
const (
	stuckStateName = ".prune_stuck.json"
	stuckLockName  = stuckStateName + ".lock"
	stuckRecheck   = 7 * 24 * time.Hour
	maxBlockers    = 16
)

// On-disk layout of the stuck state, one indented ASCII object with sorted keys
var stuckStyle = fileutil.JSONStyle{Lines: true, Indent: 1, Sorted: true, ASCII: true}

// Inode, modification and change time of an entry, the change time catching a chown that keeps the mtime
type fingerprint struct {
	ino   uint64
	mtime int64
	ctime int64
}

// Scratch entry this user could not delete, with its fingerprint, when it was recorded, by whom and what refused access
type stuckRecord struct {
	fp       fingerprint
	since    float64
	uid      int64
	blockers []string
}

// Whole number a decoded JSON value holds, false for anything else
func wholeNumber(v any, bits int, signed bool) (int64, uint64, bool) {
	n, ok := v.(json.Number)
	if !ok {
		return 0, 0, false
	}
	if signed {
		i, err := strconv.ParseInt(string(n), 10, bits)
		return i, 0, err == nil
	}
	u, err := strconv.ParseUint(string(n), 10, bits)
	return 0, u, err == nil
}

// Stuck record of a decoded value, false for anything not shaped as one
func parseStuckRecord(v any) (stuckRecord, bool) {
	m, ok := v.(map[string]any)
	if !ok {
		return stuckRecord{}, false
	}
	var rec stuckRecord
	fp, _ := m["fp"].([]any)
	if len(fp) != 3 {
		return stuckRecord{}, false
	}
	_, ino, okIno := wholeNumber(fp[0], 64, false)
	mtime, _, okM := wholeNumber(fp[1], 64, true)
	ctime, _, okC := wholeNumber(fp[2], 64, true)
	uid, _, okU := wholeNumber(m["uid"], 64, true)
	if !okIno || !okM || !okC || !okU {
		return stuckRecord{}, false
	}
	rec.fp, rec.uid = fingerprint{ino, mtime, ctime}, uid
	if s, present := m["since"]; present && s != nil {
		n, ok := s.(json.Number)
		if !ok {
			return stuckRecord{}, false
		}
		f, err := n.Float64()
		if err != nil {
			return stuckRecord{}, false
		}
		rec.since = f
	}
	list, _ := m["blockers"].([]any)
	for _, b := range list {
		if s, ok := b.(string); ok {
			rec.blockers = append(rec.blockers, s)
		}
	}
	return rec, true
}

// Value written for a record, sorted on disk by the style
func (r stuckRecord) encode() map[string]any {
	return map[string]any{"fp": []any{r.fp.ino, r.fp.mtime, r.fp.ctime}, "since": r.since, "uid": r.uid, "blockers": r.blockers}
}

// Records of the state file under root, empty when it is missing, torn or not an object
func readStuck(root string) map[string]stuckRecord {
	out := map[string]stuckRecord{}
	var doc any
	if fileutil.ReadJSON(filepath.Join(root, stuckStateName), &doc) != nil {
		return out
	}
	m, _ := doc.(map[string]any)
	for name, v := range m {
		if rec, ok := parseStuckRecord(v); ok {
			out[name] = rec
		}
	}
	return out
}

// Publishes state atomically under root, removing the file when no record is left
func writeStuck(root string, state map[string]stuckRecord) {
	path := filepath.Join(root, stuckStateName)
	if len(state) == 0 {
		_ = os.Remove(path)
		return
	}
	doc := make(map[string]any, len(state))
	for name, rec := range state {
		doc[name] = rec.encode()
	}
	_ = fileutil.WriteJSON(context.Background(), path, doc, stuckStyle,
		fileutil.TempPrefix(stuckStateName+"."), fileutil.Perm(0o600))
}

// Runs fn under the exclusive cross-process lock of the state file, false when the lock cannot be taken
func lockStuck(root string, fn func()) bool {
	l, err := fileutil.OpenLock(context.Background(), filepath.Join(root, stuckLockName), fileutil.LockPerm(0o600))
	if err != nil {
		return false
	}
	defer l.Release()
	fn()
	return true
}

// Seconds since the epoch of t as a float, the form a record stores
func epochSeconds(t time.Time) float64 { return float64(t.UnixNano()) / 1e9 }

// Reports whether a record still describes an entry this user cannot remove
func stillStuck(root, name string, rec stuckRecord, now time.Time) bool {
	if rec.uid != int64(os.Geteuid()) || epochSeconds(now)-rec.since >= stuckRecheck.Seconds() || len(rec.blockers) == 0 {
		return false
	}
	if fp, ok := entryFingerprint(filepath.Join(root, name)); !ok || fp != rec.fp {
		return false
	}
	for _, b := range rec.blockers {
		if !accessibleNow(b) {
			return true
		}
	}
	return false
}

// Entry names a pass skips, records that no longer hold dropped and persisted unless persist is false
func loadStuckSkips(root string, persist bool) map[string]bool {
	skips := map[string]bool{}
	if !stuckTrackingSupported || !exists(filepath.Join(root, stuckStateName)) {
		return skips
	}
	now := time.Now()
	filter := func(state map[string]stuckRecord) map[string]stuckRecord {
		live := map[string]stuckRecord{}
		for name, rec := range state {
			if stillStuck(root, name, rec, now) {
				live[name] = rec
			}
		}
		return live
	}
	var live map[string]stuckRecord
	if !persist {
		live = filter(readStuck(root))
	} else if !lockStuck(root, func() {
		state := readStuck(root)
		if live = filter(state); len(live) != len(state) {
			writeStuck(root, live)
		}
	}) {
		return skips
	}
	for name := range live {
		skips[name] = true
	}
	return skips
}

// Command that clears entry, with a variant through docker without sudo when docker is installed
func RemovalHint(entry string) string {
	hint := "sudo rm -rf " + shell.Quote(entry)
	if _, err := exec.LookPath("docker"); err == nil {
		hint += fmt.Sprintf("  (or without sudo: docker run --rm -v %s alpine rm -rf %s)",
			shell.Quote(filepath.Dir(entry)+":/s"), shell.Quote("/s/"+filepath.Base(entry)))
	}
	return hint
}

// Remembers entry as not removable by this user and records the one actionable line
func recordStuck(root, entry string, blockers []string, audit *pruneAudit) {
	fp, ok := entryFingerprint(entry)
	if !ok || len(blockers) == 0 {
		return
	}
	uid := int64(os.Geteuid())
	rec := stuckRecord{fp: fp, since: epochSeconds(time.Now()), uid: uid, blockers: blockers[:min(len(blockers), maxBlockers)]}
	lockStuck(root, func() {
		state := readStuck(root)
		state[filepath.Base(entry)] = rec
		writeStuck(root, state)
	})
	audit.info(fmt.Sprintf("scratch prune: cannot remove %s: it holds files this user (uid %d) may not read or "+
		"delete, usually root-owned output of a Docker container; skipping it until that is "+
		"fixed (re-checked weekly). Remove it with: %s", reprQuote(entry), uid, RemovalHint(entry)))
}
