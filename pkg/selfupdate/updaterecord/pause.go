package updaterecord

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/pm"
)

const (
	PauseRecordStem       = ".icebath-update-paused-gateways"
	PauseMutexName        = PauseRecordStem + ".lock"
	PauseMutexWaitSeconds = 10.0
)

// Pause record failure that callers treat as Python's OSError family
type PauseRecordError struct {
	Kind string
	Msg  string
}

func (e *PauseRecordError) Error() string { return e.Msg }

// Marks the error as an OSError subclass for callers that catch that family
func (e *PauseRecordError) OSError() bool { return true }

// Whether err is the pause record failure of the given kind (RecordConflict, RecordBusy, RetiredUnknown, RecordUnknown)
func IsPauseRecordError(err error, kind string) bool {
	var p *PauseRecordError
	return errors.As(err, &p) && p.Kind == kind
}

// Install root the pause record is keyed by, the resolved directory of the running binary
var pauseInstall = pm.RepoRoot

func PauseInstallRoot() string { return pauseInstall() }

// First twelve hex digits of the sha256 of the normcased install root
func pauseInstallKey(root string) string {
	if root == "" {
		root = PauseInstallRoot()
	}
	sum := sha256.Sum256([]byte(pypath.Normcase(root)))
	return hex.EncodeToString(sum[:])[:12]
}

// Paused gateway record of this install in the profile tree root home
func PauseRecordPath() string {
	return filepath.Join(home.Root(), fmt.Sprintf("%s.%s.json", PauseRecordStem, pauseInstallKey("")))
}

// Pid and ct line identity of a process, this process when pid is zero
func PauseIdentity(pid int) map[string]any {
	if pid == 0 {
		pid = os.Getpid()
	}
	ident := map[string]any{"pid": pid, "ct": nil}
	if created := ProcessCreateTime(pid); created != nil {
		ident["ct"] = fmt.Sprintf("ct:%.3f", *created)
	}
	return ident
}

// Update marker incarnation rule for a recorded identity, unprovable counting as live
func PauseIdentityIsLive(ident map[string]any) bool {
	var raw any = 0
	if truthy(ident["pid"]) {
		raw = ident["pid"]
	}
	pid, err := pyIntConvert(raw)
	if err != nil {
		return false
	}
	var ct any
	if truthy(ident["ct"]) {
		ct = ident["ct"]
	}
	live := IncarnationLive(int(pid), ct)
	return live == nil || *live
}

type pauseMutexKey struct{}

// Exclusive kernel lock on the record sidecar, reentrant along ctx, RecordBusy after wait
func PauseMutex(ctx context.Context, wait time.Duration) (context.Context, func(), error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if held, _ := ctx.Value(pauseMutexKey{}).(bool); held {
		return ctx, func() {}, nil
	}
	path := filepath.Join(filepath.Dir(PauseRecordPath()), PauseMutexName)
	if err := os.MkdirAll(filepath.Dir(path), 0o777); err != nil {
		return ctx, nil, err
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return ctx, nil, err
	}
	deadline := time.Now().Add(wait)
	for {
		ok, err := tryLock(f)
		if err != nil {
			f.Close()
			return ctx, nil, err
		}
		if ok {
			break
		}
		if time.Now().After(deadline) {
			f.Close()
			return ctx, nil, &PauseRecordError{Kind: "RecordBusy", Msg: path + " is held by another process or thread"}
		}
		time.Sleep(50 * time.Millisecond)
	}
	release := func() {
		unlockBytes(f, windowsLockOffset, 1)
		f.Close()
	}
	return context.WithValue(ctx, pauseMutexKey{}, true), release, nil
}

// Publishes body as sorted one space indented JSON through a fsynced pid tmp sibling
func pauseAtomicWrite(path string, body any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o777); err != nil {
		return err
	}
	text, err := pyjson.Dumps(body, pyjson.Indent(1), pyjson.SortKeys(true))
	if err != nil {
		return err
	}
	tmp := filepath.Join(filepath.Dir(path), fmt.Sprintf("%s.%d.tmp", filepath.Base(path), os.Getpid()))
	if err := writeSynced(tmp, []byte(text), os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o666); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := pauseReplace(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

var pauseReplace = os.Rename

// Saved pause body, nil only when absent, RecordUnknown when it cannot be read or parsed
func ReadPauseRecord(path string) (map[string]any, error) {
	if path == "" {
		path = PauseRecordPath()
	}
	raw, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, &PauseRecordError{Kind: "RecordUnknown", Msg: fmt.Sprintf("cannot read the paused-gateway record %s: %s", path, pypath.OSErrorText(err))}
	}
	text, err := pystr.DecodeUTF8Sig(raw)
	if err != nil {
		return nil, err
	}
	var body map[string]any
	if v, err := pyjson.Loads(text); err == nil {
		body, _ = pyjson.Plain(v).(map[string]any)
	}
	if _, ok := body["token"].(map[string]any); body == nil || !ok {
		return nil, &PauseRecordError{Kind: "RecordUnknown", Msg: fmt.Sprintf("the paused-gateway record %s is malformed; fix or remove it to resume", path)}
	}
	return body, nil
}

// Recovery claims beside a record, sorted
func pauseClaims(path string) []string {
	paths, err := globDir(filepath.Dir(path), globEscape(filepath.Base(path))+".*.claim")
	if err != nil {
		return nil
	}
	sort.Strings(paths)
	return paths
}

// glob.escape of a literal name, wrapping each wildcard character in brackets
func globEscape(name string) string {
	return strings.NewReplacer("[", "[[]", "*", "[*]", "?", "[?]").Replace(name)
}

// PauseFile is one record or claim of this install with its parsed body
type PauseFile struct {
	Src  string
	Body map[string]any
}

// This install's record and claims, RecordUnknown when one cannot be read
func pauseFiles(path string) ([]PauseFile, error) {
	var found []PauseFile
	for _, src := range append([]string{path}, pauseClaims(path)...) {
		body, err := ReadPauseRecord(src)
		if err != nil {
			return nil, err
		}
		if body != nil && pyrepr.Equal(body["install_root"], PauseInstallRoot()) {
			found = append(found, PauseFile{src, body})
		}
	}
	return found, nil
}

// Sorted unique string forms of an iterable value merged with extra entries
func sortedStrings(v any, extra ...string) ([]any, error) {
	items, err := pyList(orEmptyList(v))
	if err != nil {
		return nil, err
	}
	set := map[string]bool{}
	for _, item := range items {
		set[pyrepr.Str(item)] = true
	}
	for _, e := range extra {
		set[e] = true
	}
	keys := make([]string, 0, len(set))
	for k := range set {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return stringList(keys), nil
}

// Checkpoints a validated gateway stop consume into every matching record before its marker disappears
func MarkStopConsumed(path string, marker *pyjson.Object, sameHome func(left, right string) (bool, error)) error {
	rawPID, ok := marker.Get("target_pid")
	if !ok {
		return &KeyErrorText{Key: "target_pid"}
	}
	pid := pyrepr.Str(rawPID)
	stopper, _ := marker.Get("stopper_pid")
	files, err := pauseFiles(PauseRecordPath())
	if err != nil {
		return err
	}
	for _, f := range files {
		token := f.Body["token"].(map[string]any)
		markers, ok := orEmptyMap(token["stop_markers"])
		if !ok {
			return fmt.Errorf("'%s' object has no attribute 'get'", pyrepr.TypeName(token["stop_markers"]))
		}
		recorded := markers[pid]
		requested, err := pyContains(orEmptyList(token["stop_requested"]), pid)
		if err != nil {
			return err
		}
		if pyrepr.Str(token["stopper_pid"]) != pyrepr.Str(stopper) || !requested || !truthy(recorded) {
			continue
		}
		same, err := sameHome(pyrepr.Str(recorded), path)
		if err != nil {
			return err
		}
		if !same {
			continue
		}
		sent, err := sortedStrings(token["stop_sent"], pid)
		if err != nil {
			return err
		}
		token["stop_sent"] = sent
		if err := pauseAtomicWrite(f.Src, f.Body); err != nil {
			return err
		}
	}
	return nil
}

// Python membership test of a string in a decoded JSON container
func pyContains(container any, s string) (bool, error) {
	switch t := container.(type) {
	case string:
		return strings.Contains(t, s), nil
	case map[string]any:
		_, ok := t[s]
		return ok, nil
	case []any:
		for _, item := range t {
			if pyrepr.Equal(item, s) {
				return true, nil
			}
		}
		return false, nil
	}
	return false, fmt.Errorf("argument of type '%s' is not iterable", pyrepr.TypeName(container))
}

// Python KeyError text of a missing mapping key
type KeyErrorText struct{ Key string }

func (e *KeyErrorText) Error() string { return pyrepr.StrRepr(e.Key) }

func orEmptyMap(v any) (map[string]any, bool) {
	if !truthy(v) {
		return map[string]any{}, true
	}
	m, ok := v.(map[string]any)
	return m, ok
}

// Accepted receipt written beside a planned stop marker
func PauseAcceptedPath(markerPath string) string {
	return filepath.Join(filepath.Dir(markerPath), filepath.Base(markerPath)+".accepted")
}

// Receipt that this incarnation accepted the stop, beside the marker or stamped into it in place
func MarkStopAccepted(path string, marker *pyjson.Object) error {
	refused := pauseAtomicWrite(PauseAcceptedPath(path), marker)
	if refused == nil {
		return nil
	}
	stamped := pyjson.NewObject()
	for _, k := range marker.Keys {
		v, _ := marker.Get(k)
		stamped.Set(k, v)
	}
	stamped.Set("accepted", true)
	err := func() error {
		f, err := os.OpenFile(path, os.O_RDWR, 0)
		if err != nil {
			return err
		}
		defer f.Close()
		text, err := pyjson.Dumps(stamped)
		if err != nil {
			return err
		}
		if _, err := f.WriteAt([]byte(text), 0); err != nil {
			return err
		}
		if err := f.Truncate(int64(len(text))); err != nil {
			return err
		}
		return f.Sync()
	}()
	if err != nil {
		fmt.Fprintf(os.Stderr, "  ⚠ Could not record the accepted stop request %s: %s; %s\n", path, pypath.OSErrorText(refused), pypath.OSErrorText(err))
	}
	return nil
}
