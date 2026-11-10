package updaterecord

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/pm"
)

const (
	HostObligationName       = "host-update-restart.json"
	ProfileMarkerName        = "fleet_restart_pending"
	obligationRecordVersion  = 1
	hostObligationNameStem   = "host-update-restart"
	obligationMutexWaitScale = float64(time.Second)
)

var (
	obligationLog                 = slog.Default
	obligationStderr    io.Writer = os.Stderr
	obligationInstall             = pm.RepoRoot
	obligationFsync               = func(f *os.File) error { return f.Sync() }
	obligationReplace             = os.Rename
	obligationNow                 = func() float64 { return float64(time.Now().UnixNano()) / 1e9 }
	obligationMutexWait           = time.Duration(MutexWaitSeconds * obligationMutexWaitScale)
)

// Host state dir the gateway rendezvous resolves, computed here without importing the gateway
func HostStateDir() string {
	if override := os.Getenv("ICEBATH_GATEWAY_LOCK_DIR"); override != "" {
		return override
	}
	stateHome := os.Getenv("XDG_STATE_HOME")
	if !pypath.IsAbs(stateHome) {
		stateHome = pypath.Join(pypath.Expanduser("~"), ".local", "state")
	}
	return pypath.Join(stateHome, "icebath", "gateway-locks")
}

// This install's host obligation record, keyed by the normcased install root
func HostObligationPath() string {
	sum := sha256.Sum256([]byte(pypath.Normcase(obligationInstall())))
	return pypath.PathJoin(HostStateDir(), hostObligationNameStem+"-"+hex.EncodeToString(sum[:])[:16]+".json")
}

// Record a reader honours, this install's own else an older release's unkeyed one
func owedPath() string {
	path := HostObligationPath()
	legacy := filepath.Join(filepath.Dir(path), HostObligationName)
	if _, err := os.Stat(path); err != nil {
		if _, err := os.Stat(legacy); err == nil {
			return legacy
		}
	}
	return path
}

func writeObligationRecord(path string, record *pyjson.Object) error {
	text, err := pyjson.Dumps(record, pyjson.Indent(2), pyjson.EnsureASCII(false))
	if err != nil {
		return err
	}
	return ReplaceBytes(path, []byte(text))
}

// Published obligation record, nil when absent, corrupt or another version
func ReadHostObligation() *pyjson.Object { return readObligationRecord(owedPath()) }

func readObligationRecord(path string) *pyjson.Object {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	text, err := pystr.DecodeUTF8Sig(raw)
	if err != nil {
		return nil
	}
	v, err := pyjson.Loads(text)
	if err != nil {
		return nil
	}
	record, ok := v.(*pyjson.Object)
	if !ok || !pyrepr.Equal(record.Vals["version"], obligationRecordVersion) {
		return nil
	}
	return record
}

// True when the record file exists, parseable or not
func HostObligationPresent() bool {
	st, err := os.Stat(owedPath())
	return err == nil && st.Mode().IsRegular()
}

// Merges fields into the armed record, never failing
func AmendHostObligation(ctx context.Context, fields *pyjson.Object) {
	updateObligationRecord(ctx, func(record *pyjson.Object) { record.Update(fields) }, "amend")
}

// Arms the host obligation, true when written, runtimes nil standing for no inventory
func WriteHostObligation(ctx context.Context, expectedSHA string, runtimes []any, profile, owner string) bool {
	path := HostObligationPath()
	written := false
	err := withObligationMutex(ctx, path, func() error {
		written = writeObligationLocked(ctx, path, expectedSHA, runtimes, profile, owner)
		return nil
	})
	if err != nil {
		obligationLog().Debug(fmt.Sprintf("Could not lock the host update-restart obligation: %s", pypath.OSErrorText(err)))
		return false
	}
	return written
}

// Arms the host record else the per home marker, false when the debt is hidden, an error for an unserializable inventory
func ArmHostObligation(ctx context.Context, marker, expectedSHA string, runtimes []any, profile, owner string) (bool, error) {
	if WriteHostObligation(ctx, expectedSHA, runtimes, profile, owner) {
		return true, nil
	}
	lines := []string{"started=" + pyrepr.Float(obligationNow()), fmt.Sprintf("pid=%d", os.Getpid())}
	if expectedSHA != "" {
		lines = append(lines, "expected_sha="+expectedSHA)
	}
	if runtimes != nil {
		inventory, err := pyjson.Dumps(pyjson.ObjectOf("version", 1, "runtimes", runtimes))
		if err != nil {
			return false, err
		}
		lines = append(lines, "inventory="+inventory)
	}
	hidden := ""
	if err := ReplaceBytes(marker, []byte(strings.Join(lines, "\n")+"\n")); err != nil {
		hidden = fmt.Sprintf(" or %s (%s)", marker, pypath.OSErrorText(err))
	}
	if hidden == "" {
		if exist, err := namedProfilesExist(); err != nil {
			hidden = fmt.Sprintf(" (%s may hide it from other profiles: the profile list is unreadable: %s)", marker, pypath.OSErrorText(err))
		} else if exist {
			hidden = fmt.Sprintf(" (%s hides it from the other profiles)", marker)
		}
	}
	if hidden == "" {
		obligationLog().Warn(fmt.Sprintf("Host update-restart obligation (%s) is unwritable; armed the per-home marker %s instead.",
			HostObligationPath(), marker))
		return true, nil
	}
	obligationLog().Error(fmt.Sprintf("Could not arm the update-restart obligation in %s%s; an interrupted update will not warn.",
		HostObligationPath(), hidden))
	fmt.Fprintln(obligationStderr, "  Warning: could not record the pending gateway-restart obligation where every profile sees it "+
		"(state dir not writable) - restart gateways with `icebath gateway restart` if this update is "+
		"interrupted.")
	return false, nil
}

// True when a live named profile beside default exists, an unreadable roster or identity raising
func namedProfilesExist() (bool, error) {
	entries, err := os.ReadDir(filepath.Join(home.Root(), "profiles"))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ENOTDIR) {
			return false, nil
		}
		return false, err
	}
	for _, entry := range entries {
		profileHome := filepath.Join(home.Root(), "profiles", entry.Name())
		if entry.Name() == "default" || !home.ProfileIDRe.MatchString(entry.Name()) {
			continue
		}
		mode, err := obligationMode(profileHome, os.Stat)
		if err != nil {
			return false, err
		}
		if !mode.IsDir() {
			continue
		}
		identity := false
		for _, name := range home.ProfileIdentityMarkers() {
			m, err := obligationMode(filepath.Join(profileHome, name), os.Lstat)
			if err != nil {
				return false, err
			}
			if m != 0 && !m.IsDir() {
				identity = true
				break
			}
		}
		if !identity {
			continue
		}
		tomb, err := obligationMode(home.TombstonePath(profileHome), os.Lstat)
		if err != nil {
			return false, err
		}
		if tomb == 0 {
			return true, nil
		}
	}
	return false, nil
}

// File mode of path through probe with a present marker bit, zero when absent, other errors raising
func obligationMode(path string, probe func(string) (os.FileInfo, error)) (fs.FileMode, error) {
	st, err := probe(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ENOTDIR) {
			return 0, nil
		}
		return 0, err
	}
	if st.Mode() == 0 {
		return fs.ModeIrregular, nil
	}
	return st.Mode(), nil
}

func writeObligationLocked(ctx context.Context, path, expectedSHA string, runtimes []any, profile, owner string) bool {
	existing := readObligationRecord(path)
	var found any
	if owner != "" {
		var err error
		if found, err = baselineFor(owner, existing, path); err != nil {
			obligationLog().Debug(fmt.Sprintf("Could not read the host update-restart obligation: %s", pypath.OSErrorText(err)))
			return false
		}
	}
	if existing != nil && expectedSHA != "" && pyrepr.Equal(existing.Vals["expected_sha"], expectedSHA) {
		fields := pyjson.NewObject()
		owners := obligationOwners(existing)
		if owner != "" && !containsString(owners, owner) {
			fields.Set("owners", stringList(append(append([]string(nil), owners...), owner)))
			if len(owners) == 0 {
				fields.Set("found", found)
			}
		}
		inventory := pyjson.ObjectOf("version", 1, "runtimes", runtimes)
		if runtimes != nil && !pyrepr.Equal(existing.Vals["inventory"], inventory) {
			fields.Set("inventory", inventory)
		}
		if fields.Len() == 0 {
			return true
		}
		merged := existing.Copy()
		merged.Update(fields)
		if err := writeObligationRecord(path, merged); err != nil {
			obligationLog().Debug(fmt.Sprintf("Could not amend host update-restart obligation: %s", pypath.OSErrorText(err)))
			return false
		}
		return true
	}
	payload := pyjson.ObjectOf("version", obligationRecordVersion, "started", obligationNow(), "pid", os.Getpid(),
		"armed_by_profile", profile, "expected_sha", expectedSHA)
	if owner != "" {
		payload.Set("owners", []any{owner})
		payload.Set("found", found)
	}
	if runtimes != nil {
		payload.Set("inventory", pyjson.ObjectOf("version", 1, "runtimes", runtimes))
	}
	if _, err := home.MakeUserDir(ctx, filepath.Dir(path), 0o777, true, true); err != nil {
		obligationLog().Debug(fmt.Sprintf("Could not write host update-restart obligation: %s", pypath.OSErrorText(err)))
		return false
	}
	if err := writeObligationRecord(path, payload); err != nil {
		obligationLog().Debug(fmt.Sprintf("Could not write host update-restart obligation: %s", pypath.OSErrorText(err)))
		return false
	}
	return true
}

// Runs fn under the shared record guard every install's arm, release and stamp takes
func withObligationMutex(ctx context.Context, path string, fn func() error) error {
	guard := filepath.Join(filepath.Dir(path), "."+hostObligationNameStem+".mutex")
	if _, err := home.MakeUserDir(ctx, guard, 0o777, true, true); err != nil {
		return err
	}
	_, release, err := MarkerMutex(context.Background(), filepath.Join(guard, "record"), obligationMutexWait)
	if err != nil {
		return err
	}
	defer release()
	return fn()
}

func obligationOwners(record *pyjson.Object) []string {
	if record == nil {
		return nil
	}
	list, ok := record.Vals["owners"].([]any)
	if !ok {
		return nil
	}
	out := make([]string, len(list))
	for i, o := range list {
		out[i] = pyrepr.Str(o)
	}
	return out
}

func containsString(values []string, s string) bool {
	for _, v := range values {
		if v == s {
			return true
		}
	}
	return false
}

// What owner's release puts back as base64, nil for absent, an unreadable record raising
func baselineFor(owner string, existing *pyjson.Object, path string) (any, error) {
	owners := obligationOwners(existing)
	if !containsString(owners, owner) {
		return foundField(path)
	}
	var others []string
	for _, o := range owners {
		if o != owner {
			others = append(others, o)
		}
	}
	if len(others) == 0 {
		return existing.Vals["found"], nil
	}
	shared := existing.Copy()
	shared.Set("owners", stringList(others))
	text, err := pyjson.Dumps(shared, pyjson.Indent(2), pyjson.EnsureASCII(false))
	if err != nil {
		return nil, err
	}
	return base64.StdEncoding.EncodeToString([]byte(text)), nil
}

// Current record bytes as base64, nil when absent
func foundField(path string) (any, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	return base64.StdEncoding.EncodeToString(raw), nil
}

// Hands back owner's stake, restoring what the first owner found once no other run owes through it
func ReleaseHostObligation(ctx context.Context, owner string) error {
	path := HostObligationPath()
	if owner == "" || !containsString(obligationOwners(readObligationRecord(path)), owner) {
		return nil
	}
	return withObligationMutex(ctx, path, func() error { return releaseObligationLocked(path, owner) })
}

func releaseObligationLocked(path, owner string) error {
	record := readObligationRecord(path)
	owners := obligationOwners(record)
	if !containsString(owners, owner) {
		return nil
	}
	var remaining []string
	for _, o := range owners {
		if o != owner {
			remaining = append(remaining, o)
		}
	}
	if len(remaining) > 0 {
		merged := record.Copy()
		merged.Set("owners", stringList(remaining))
		return writeObligationRecord(path, merged)
	}
	var found []byte
	if raw := record.Vals["found"]; raw != nil {
		text, ok := raw.(string)
		if !ok || strings.ContainsAny(text, "\r\n") {
			return nil
		}
		decoded, err := base64.StdEncoding.DecodeString(text)
		if err != nil {
			return nil
		}
		found = decoded
	} else {
		if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		return nil
	}
	return ReplaceBytes(path, found)
}

// Writes data as one durable record through a fresh 0600 temp file renamed over path
func ReplaceBytes(path string, data []byte) error {
	stem := filepath.Base(path)
	if ext := filepath.Ext(stem); ext != "" && ext != stem {
		stem = strings.TrimSuffix(stem, ext)
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+stem+"_*.tmp")
	if err != nil {
		return err
	}
	fail := func(err error) error {
		_ = tmp.Close()
		_ = os.Remove(tmp.Name())
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		return fail(err)
	}
	if err := obligationFsync(tmp); err != nil {
		return fail(err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmp.Name())
		return err
	}
	if err := obligationReplace(tmp.Name(), path); err != nil {
		_ = os.Remove(tmp.Name())
		return err
	}
	if runtime.GOOS == "windows" {
		return nil
	}
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer dir.Close()
	return obligationFsync(dir)
}

// Discharges the record this install's readers judged, never failing
func ClearHostObligation() {
	if err := os.Remove(owedPath()); err != nil && !errors.Is(err, fs.ErrNotExist) {
		obligationLog().Debug(fmt.Sprintf("Could not clear host update-restart obligation: %s", pypath.OSErrorText(err)))
	}
}

// Obligation in the legacy key value field shape, nil when unarmed
func ObligationFields() map[string]string {
	record := ReadHostObligation()
	if record == nil {
		return nil
	}
	fields := map[string]string{"expected_sha": ""}
	if sha := record.Vals["expected_sha"]; truthy(sha) {
		fields["expected_sha"] = pyrepr.Str(sha)
	}
	if inventory, present := record.Vals["inventory"]; present && inventory != nil {
		fields["inventory"], _ = pyjson.Dumps(inventory)
	}
	return fields
}

// Records that the host process restarted onto sha, never failing
func MarkHostRestartCompleted(ctx context.Context, sha string) {
	updateObligationRecord(ctx, func(record *pyjson.Object) {
		record.Set("restarted", pyjson.ObjectOf("sha", sha, "pid", os.Getpid(), "at", obligationNow()))
	}, "stamp restart completion on")
}

// Reads, changes and rewrites an armed record under the guard, no record meaning no write
func updateObligationRecord(ctx context.Context, change func(*pyjson.Object), what string) {
	path := owedPath()
	if readObligationRecord(path) == nil {
		return
	}
	err := withObligationMutex(ctx, path, func() error {
		record := readObligationRecord(path)
		if record == nil {
			return nil
		}
		change(record)
		return writeObligationRecord(path, record)
	})
	if err != nil {
		obligationLog().Debug(fmt.Sprintf("Could not %s host update-restart obligation: %s", what, pypath.OSErrorText(err)))
	}
}

// True when this host obligation already restarted onto sha
func HostRestartAlreadyCompleted(sha string) bool {
	record := ReadHostObligation()
	if record == nil || sha == "" {
		return false
	}
	restarted, ok := record.Vals["restarted"].(*pyjson.Object)
	if !ok {
		return false
	}
	got := ""
	if v := restarted.Vals["sha"]; truthy(v) {
		got = pyrepr.Str(v)
	}
	return got == sha
}

// Splits units into restarts and legacy units covered by another unit sharing their live main pid
func CollapseUnitsToHostProcesses(units []string, mainPID func(string) (int, error)) ([]string, map[string]string) {
	restart := []string{}
	covered := map[string]string{}
	ownerByPID := map[int]string{}
	for _, unit := range units {
		pid, err := mainPID(unit)
		if err != nil {
			pid = 0
		}
		if pid <= 0 {
			restart = append(restart, unit)
			continue
		}
		if owner, ok := ownerByPID[pid]; ok {
			covered[unit] = owner
			continue
		}
		ownerByPID[pid] = unit
		restart = append(restart, unit)
	}
	return restart, covered
}
