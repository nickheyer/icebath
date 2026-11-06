package home

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"

	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pyshlex"
)

const (
	// Scratch file remembering entries this user cannot delete
	StuckStateName = ".prune_stuck.json"
	// Lock file serializing read merge write of the stuck state
	StuckLockName       = StuckStateName + ".lock"
	stuckRecheckSeconds = 7 * 24 * 3600
	maxStuckBlockers    = 16
)

// Stuck entry tracking needs POSIX uids, so Windows keeps retrying permission failures every pass
func stuckTrackingSupported() bool { return runtime.GOOS != "windows" }

// Whether err carries EACCES or EPERM
func isPermissionError(err error) bool { return errors.Is(err, fs.ErrPermission) }

// Stuck records keyed by entry name in file order
type stuckState struct {
	keys []string
	recs map[string]*pyjson.Object
}

func (s *stuckState) set(name string, rec *pyjson.Object) {
	if _, ok := s.recs[name]; !ok {
		s.keys = append(s.keys, name)
	}
	s.recs[name] = rec
}

// Stuck records of root, empty when the file is missing, torn or not an object
func readStuck(root string) *stuckState {
	state := &stuckState{recs: map[string]*pyjson.Object{}}
	data, err := os.ReadFile(filepath.Join(root, StuckStateName))
	if err != nil {
		return state
	}
	text, ok := utf8SigText(data)
	if !ok {
		return state
	}
	v, err := pyjson.Loads(text)
	obj, isObj := v.(*pyjson.Object)
	if err != nil || !isObj {
		return state
	}
	for _, k := range obj.Keys {
		if rec, ok := obj.Vals[k].(*pyjson.Object); ok {
			state.set(k, rec)
		}
	}
	return state
}

// Publishes state through a unique temp file and rename, removing the file when state is empty
func writeStuck(root string, state *stuckState) {
	path := filepath.Join(root, StuckStateName)
	if len(state.keys) == 0 {
		_ = os.Remove(path)
		return
	}
	obj := pyjson.NewObject()
	for _, k := range state.keys {
		obj.Keys = append(obj.Keys, k)
		obj.Vals[k] = state.recs[k]
	}
	text, err := pyjson.Dumps(obj, pyjson.Indent(1), pyjson.SortKeys(true))
	if err != nil {
		return
	}
	f, err := os.CreateTemp(root, StuckStateName+".*.tmp")
	if err != nil {
		return
	}
	_, werr := f.WriteString(text)
	cerr := f.Close()
	if werr != nil || cerr != nil || os.Rename(f.Name(), path) != nil {
		_ = os.Remove(f.Name())
	}
}

// Python float of a falsy-defaulted value, the conversion error raised like float()
func floatOrZero(v any) (float64, error) {
	if !pyrepr.Truthy(v) {
		return 0, nil
	}
	return pyint.Float(v)
}

// Whether a record still describes an entry this user cannot remove, by uid, age, fingerprint and blockers
func stillStuck(root, name string, rec *pyjson.Object, now float64) (bool, error) {
	if !numberEquals(rec.Vals["uid"], int64(os.Geteuid())) {
		return false, nil
	}
	since, err := floatOrZero(rec.Vals["since"])
	if err != nil {
		return false, err
	}
	if now-since >= stuckRecheckSeconds {
		return false, nil
	}
	fp, ok := entryFingerprint(filepath.Join(root, name))
	if !fingerprintEquals(rec.Vals["fp"], fp, ok) {
		return false, nil
	}
	blockers, ok := rec.Vals["blockers"].([]any)
	if !ok || len(blockers) == 0 {
		return false, nil
	}
	for _, b := range blockers {
		if s, ok := b.(string); ok && !accessibleNow(s) {
			return true, nil
		}
	}
	return false, nil
}

// Python list equality between a recorded fingerprint and the live one, None matching a missing entry
func fingerprintEquals(recorded any, fp []int64, ok bool) bool {
	if !ok {
		return recorded == nil
	}
	list, isList := recorded.([]any)
	if !isList || len(list) != len(fp) {
		return false
	}
	for i, v := range list {
		if !numberEquals(v, fp[i]) {
			return false
		}
	}
	return true
}

// Names to skip this pass, dropping records that no longer hold and persisting that unless persist is false
func LoadStuckSkips(root string, persist bool) (map[string]bool, error) {
	skips := map[string]bool{}
	if !stuckTrackingSupported() {
		return skips, nil
	}
	if _, err := os.Stat(filepath.Join(root, StuckStateName)); err != nil {
		return skips, nil
	}
	now := scratchNow()
	filter := func(state *stuckState) (*stuckState, error) {
		live := &stuckState{recs: map[string]*pyjson.Object{}}
		for _, k := range state.keys {
			stuck, err := stillStuck(root, k, state.recs[k], now)
			if err != nil {
				return nil, err
			}
			if stuck {
				live.set(k, state.recs[k])
			}
		}
		return live, nil
	}
	var live *stuckState
	var filterErr error
	if !persist {
		live, filterErr = filter(readStuck(root))
	} else if lockErr := lockStuck(root, func() {
		state := readStuck(root)
		if live, filterErr = filter(state); filterErr == nil && len(live.keys) != len(state.keys) {
			writeStuck(root, live)
		}
	}); lockErr != nil {
		return skips, nil
	}
	if filterErr != nil {
		return nil, filterErr
	}
	for _, k := range live.keys {
		skips[k] = true
	}
	return skips, nil
}

// Removes entry past every error, returning the directories that refused access with EACCES or EPERM
func rmtreeCollectingPermissionErrors(entry string) []string {
	var blockers []string
	onexc := func(blocker string, err error) {
		if isPermissionError(err) && !slices.Contains(blockers, blocker) {
			blockers = append(blockers, blocker)
		}
	}
	var walk func(dir string)
	walk = func(dir string) {
		children, err := readDirUnsorted(dir)
		if err != nil {
			onexc(dir, err)
		}
		for _, child := range children {
			path := filepath.Join(dir, child.Name())
			if child.Type().IsDir() {
				walk(path)
			} else if err := os.Remove(path); err != nil {
				onexc(dir, err)
			}
		}
		if err := os.Remove(dir); err != nil {
			onexc(filepath.Dir(dir), err)
		}
	}
	if _, err := os.Lstat(entry); err != nil {
		onexc(entry, err)
		return blockers
	}
	walk(entry)
	return blockers
}

// Command that clears entry, with a sudo free docker variant when docker is installed
func RemovalHint(entry string) string {
	hint := "sudo rm -rf " + pyshlex.Quote(entry)
	if _, err := exec.LookPath("docker"); err == nil {
		hint += fmt.Sprintf("  (or without sudo: docker run --rm -v %s alpine rm -rf %s)",
			pyshlex.Quote(filepath.Dir(entry)+":/s"), pyshlex.Quote("/s/"+filepath.Base(entry)))
	}
	return hint
}

// Remembers entry as not removable by this user and logs the one actionable line
func recordStuck(root, entry string, blockers []string, audit *pruneAudit) {
	fp, ok := entryFingerprint(entry)
	if !ok || len(blockers) == 0 {
		return
	}
	uid := os.Geteuid()
	kept := blockers[:min(len(blockers), maxStuckBlockers)]
	rec := pyjson.NewObject()
	rec.Keys = []string{"fp", "since", "uid", "blockers"}
	fpList := make([]any, len(fp))
	for i, n := range fp {
		fpList[i] = n
	}
	blockerList := make([]any, len(kept))
	for i, b := range kept {
		blockerList[i] = b
	}
	rec.Vals["fp"], rec.Vals["since"], rec.Vals["uid"], rec.Vals["blockers"] = fpList, scratchNow(), int64(uid), blockerList
	_ = lockStuck(root, func() {
		state := readStuck(root)
		state.set(filepath.Base(entry), rec)
		writeStuck(root, state)
	})
	audit.info(fmt.Sprintf("scratch prune: cannot remove %s: it holds files this user (uid %d) may not read or "+
		"delete, usually root-owned output of a Docker container; skipping it until that is "+
		"fixed (re-checked weekly). Remove it with: %s", pathRepr(entry), uid, RemovalHint(entry)))
}

// Entries the prune currently skips as not removable, for the doctor report
func StuckEntries(root string) ([]string, error) {
	skips, err := LoadStuckSkips(root, false)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(skips))
	for n := range skips {
		names = append(names, n)
	}
	slices.Sort(names)
	for i, n := range names {
		names[i] = filepath.Join(root, n)
	}
	return names, nil
}
