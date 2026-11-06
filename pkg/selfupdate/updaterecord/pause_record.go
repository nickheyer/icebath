package updaterecord

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/pm"
)

const (
	pauseRelaunchRetryBase = 60.0
	pauseRelaunchRetryMax  = 3600.0
)

var pauseCarried = []string{"pause_id", "baselines", "identities", "stop_requested", "stop_sent", "stopper_pid",
	"stop_markers", "absorbed"}

// PlannedStopProbe carries the gateway status rules a stop request is judged by
type PlannedStopProbe struct {
	TTL       int
	IsStale   func(writtenAt any, ttl int) bool
	StartTime func(pid int) any
}

var (
	plannedStopProbe     PlannedStopProbe
	pauseGatewayResumer  func(ctx context.Context, token map[string]any, stdout io.Writer) error
	pauseStdout          io.Writer = os.Stdout
	pauseStderr          io.Writer = os.Stderr
	pauseNow                       = func() float64 { return float64(time.Now().UnixNano()) / 1e9 }
	pauseLiveElsewhere             = liveUpdateElsewhere
	pauseClaimSuffix               = func() string { return randomHex8() }
	pauseRetireRedundant           = RetirePauseRedundant
	pauseClaim                     = ClaimPause
	pauseTransferHook              = func(point string) {}
)

// Installs the gateway planned stop marker rules, called by the gateway package
func SetPlannedStopProbe(probe PlannedStopProbe) { plannedStopProbe = probe }

// Installs the Windows gateway resume a recovering launch runs, called by the update command package
func SetPauseGatewayResumer(resume func(ctx context.Context, token map[string]any, stdout io.Writer) error) {
	pauseGatewayResumer = resume
}

// Owner identity of a record nobody holds
func PauseUnowned() map[string]any { return map[string]any{"pid": 0, "ct": nil} }

func randomHex8() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])[:8]
}

// Path with its last suffix replaced the way Path.with_suffix does
func pyWithSuffix(path, suffix string) string {
	base := filepath.Base(path)
	stem := base
	if i := strings.LastIndex(base, "."); i > 0 && i < len(base)-1 {
		stem = base[:i]
	}
	return filepath.Join(filepath.Dir(path), stem+suffix)
}

func pauseTokenOf(body map[string]any) map[string]any {
	token, _ := body["token"].(map[string]any)
	return token
}

func shallowCopy(m map[string]any) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

func strSet(v any) map[string]bool {
	set := map[string]bool{}
	for _, item := range listItems(orEmptyList(v)) {
		set[pyrepr.Str(item)] = true
	}
	return set
}

func sortedSet(set map[string]bool) []any {
	keys := make([]string, 0, len(set))
	for k := range set {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return stringList(keys)
}

func mapOf(v any) map[string]any {
	m, _ := v.(map[string]any)
	return m
}

func listContains(list []any, v any) bool {
	for _, x := range list {
		if pyrepr.Equal(x, v) {
			return true
		}
	}
	return false
}

// Appends each item missing from the growing list, the list.extend of a filtering generator
func extendMissing(list []any, items []any) []any {
	for _, item := range items {
		if !listContains(list, item) {
			list = append(list, item)
		}
	}
	return list
}

// Existing list under key or a new empty one stored there, the setdefault of a list field
func setdefaultList(token map[string]any, key string) ([]any, error) {
	v, ok := token[key]
	if !ok {
		token[key] = []any{}
		return []any{}, nil
	}
	list, isList := v.([]any)
	if !isList {
		return nil, fmt.Errorf("'%s' object has no attribute 'extend'", pyrepr.TypeName(v))
	}
	return list, nil
}

// Existing mapping under key or a new empty one stored there, the setdefault of a dict field
func setdefaultMap(token map[string]any, key string) (map[string]any, error) {
	v, ok := token[key]
	if !ok {
		m := map[string]any{}
		token[key] = m
		return m, nil
	}
	m, isMap := v.(map[string]any)
	if !isMap {
		return nil, fmt.Errorf("'%s' object has no attribute 'setdefault'", pyrepr.TypeName(v))
	}
	return m, nil
}

func pauseWarn(w io.Writer, format string, args ...any) { fmt.Fprintf(w, format+"\n", args...) }

// Persists token keeping this pause's owner, merging a dead pause's orphaned set and refusing a live one
func WritePause(ctx context.Context, token, owner map[string]any, path string) error {
	if path == "" {
		path = PauseRecordPath()
	}
	_, release, err := PauseMutex(ctx, PauseMutexWaitSeconds*time.Second)
	if err != nil {
		return err
	}
	defer release()
	existing, err := ReadPauseRecord(path)
	if err != nil {
		return err
	}
	same := existing != nil && pyrepr.Equal(pauseTokenOf(existing)["pause_id"], token["pause_id"])
	if existing != nil && !same {
		if !pyrepr.Equal(existing["install_root"], PauseInstallRoot()) {
			return &PauseRecordError{Kind: "RecordConflict", Msg: fmt.Sprintf("%s holds gateways paused for another checkout (%s)", path, pyrepr.Str(existing["install_root"]))}
		}
		if PauseIdentityIsLive(mapOf(existing["owner"])) {
			return &PauseRecordError{Kind: "RecordConflict", Msg: fmt.Sprintf("%s holds gateways paused by live process %s", path, pyrepr.Str(mapOf(existing["owner"])["pid"]))}
		}
		dropped, err := DropNeverStopped(shallowCopy(pauseTokenOf(existing)))
		if err != nil {
			return err
		}
		if _, err := MergePauseInto(token, dropped); err != nil {
			return err
		}
	}
	if owner == nil {
		if same {
			owner = mapOf(existing["owner"])
		} else {
			owner = PauseIdentity(0)
		}
	}
	return pauseAtomicWrite(path, map[string]any{"schema": 1, "owner": owner, "install_root": PauseInstallRoot(), "token": token})
}

// Persists the intended stops and each mapped gateway's marker path before the first stop request
func MarkStopRequested(ctx context.Context, token map[string]any, pids []int, markers map[int]string) error {
	extra := make([]string, len(pids))
	for i, p := range pids {
		extra[i] = fmt.Sprint(p)
	}
	requested, err := sortedStrings(token["stop_requested"], extra...)
	if err != nil {
		return err
	}
	sent, err := sortedStrings(token["stop_sent"])
	if err != nil {
		return err
	}
	token["stop_requested"], token["stop_sent"], token["stopper_pid"] = requested, sent, os.Getpid()
	merged := map[string]any{}
	for k, v := range mapOf(token["stop_markers"]) {
		merged[k] = v
	}
	for p, m := range markers {
		merged[fmt.Sprint(p)] = m
	}
	token["stop_markers"] = merged
	return WritePause(ctx, token, PauseIdentity(0), "")
}

// Persists that pid's stop request was issued, an unwritable record warning instead of failing
func MarkStopSent(ctx context.Context, token map[string]any, pid int) error {
	sent, err := sortedStrings(token["stop_sent"], fmt.Sprint(pid))
	if err != nil {
		return err
	}
	token["stop_sent"] = sent
	if err := WritePause(ctx, token, PauseIdentity(0), ""); err != nil {
		if !isOSError(err) {
			return err
		}
		pauseWarn(pauseStdout, "  ⚠ Could not record the stop request for gateway %d: %s", pid, pauseErrText(err))
	}
	return nil
}

func pauseErrText(err error) string {
	var pe *PauseRecordError
	if errors.As(err, &pe) {
		return pe.Msg
	}
	return pypath.OSErrorText(err)
}

// Removes the record only while it still names this pause, decided under the mutex
func DischargePause(ctx context.Context, token map[string]any, path string) error {
	if path == "" {
		path = PauseRecordPath()
	}
	_, release, err := PauseMutex(ctx, PauseMutexWaitSeconds*time.Second)
	if err != nil {
		return err
	}
	defer release()
	existing, err := ReadPauseRecord(path)
	if err != nil {
		return err
	}
	if existing != nil && pyrepr.Equal(pauseTokenOf(existing)["pause_id"], token["pause_id"]) {
		return retirePause(path, []PauseFile{{path, existing}})
	}
	return nil
}

// Mirrors the token after a resume attempt, deleting a finished record and rewriting an owed one
func SyncPause(ctx context.Context, token map[string]any) error {
	if !truthy(token["pause_id"]) || truthy(token["recovery"]) {
		return nil
	}
	var err error
	if truthy(token["resume_needed"]) || truthy(token["resume_deferred"]) {
		owed := shallowCopy(token)
		owed["resume_needed"] = true
		delete(owed, "resume_deferred")
		err = WritePause(ctx, owed, nil, "")
	} else {
		err = DischargePause(ctx, token, "")
	}
	if err != nil {
		if !isOSError(err) {
			return err
		}
		pauseWarn(pauseStdout, "  ⚠ Could not update the paused-gateway record %s: %s", PauseRecordPath(), pauseErrText(err))
	}
	return nil
}

// A live update other than the update tree holding this install's checkout lock
func liveUpdateElsewhere(ctx context.Context) (bool, error) {
	root := PauseInstallRoot()
	held, err := HoldsCheckoutLock(root)
	if err != nil || held {
		return false, err
	}
	return UpdateInProgress(ctx, root)
}

func pauseRetiredPath(path string) string { return pyWithSuffix(path, ".retired") }

// Completed obligation ids, an unreadable list raising RetiredUnknown and a malformed one quarantined
func pauseRetired(path string) (map[string]bool, error) {
	target := pauseRetiredPath(path)
	raw, err := os.ReadFile(target)
	if errors.Is(err, fs.ErrNotExist) {
		return map[string]bool{}, nil
	}
	if err != nil {
		return nil, &PauseRecordError{Kind: "RetiredUnknown", Msg: fmt.Sprintf("cannot read the retired paused-gateway list %s: %s", target, pypath.OSErrorText(err))}
	}
	var ids any
	if v, err := pyjson.Loads(pystr.DecodeUTF8Replace(bytes.TrimPrefix(raw, []byte("\xef\xbb\xbf")))); err == nil {
		if obj, ok := v.(*pyjson.Object); ok {
			ids = obj.Vals["ids"]
		}
	}
	list, ok := ids.([]any)
	if !ok {
		return quarantineRetired(path, target)
	}
	set := map[string]bool{}
	for _, i := range list {
		set[pyrepr.Str(pyjson.Plain(i))] = true
	}
	return set, nil
}

// Sets aside a malformed retired list once no record or claim of this install remains
func quarantineRetired(path, target string) (map[string]bool, error) {
	if _, err := os.Stat(path); err == nil || len(pauseClaims(path)) > 0 {
		return nil, &PauseRecordError{Kind: "RetiredUnknown", Msg: fmt.Sprintf("the retired paused-gateway list %s is malformed", target)}
	}
	corrupt := pyWithSuffix(target, ".corrupt")
	if err := os.Rename(target, corrupt); err != nil {
		return nil, &PauseRecordError{Kind: "RetiredUnknown", Msg: fmt.Sprintf("cannot set aside the malformed retired paused-gateway list %s: %s", target, pypath.OSErrorText(err))}
	}
	pauseWarn(pauseStderr, "  ⚠ Set aside a malformed paused-gateway history (no paused set refers to it): %s", corrupt)
	return map[string]bool{}, nil
}

// Retires the carriers' obligation ids durably first, then deletes their files
func retirePause(path string, carriers []PauseFile) error {
	ids := map[string]bool{}
	for _, c := range carriers {
		token := pauseTokenOf(c.Body)
		for _, i := range append([]any{token["pause_id"]}, listItems(orEmptyList(token["absorbed"]))...) {
			if truthy(i) {
				ids[pyrepr.Str(i)] = true
			}
		}
	}
	retired, err := pauseRetired(path)
	if err != nil {
		return err
	}
	subset := true
	for id := range ids {
		if !retired[id] {
			subset = false
		}
	}
	if !subset {
		union := map[string]bool{}
		for id := range retired {
			union[id] = true
		}
		for id := range ids {
			union[id] = true
		}
		if err := pauseAtomicWrite(pauseRetiredPath(path), map[string]any{"schema": 1, "ids": sortedSet(union)}); err != nil {
			return err
		}
	}
	for _, c := range carriers {
		_ = os.Remove(c.Src)
	}
	return nil
}

// Who holds a file, a record's owner or a claim's claimer
func pauseHolder(src string, body map[string]any) map[string]any {
	key := "claimer"
	if filepath.Ext(src) == ".json" {
		key = "owner"
	}
	if held, ok := body[key].(map[string]any); ok {
		return held
	}
	return PauseUnowned()
}

// Files that only copy an obligation another file carries or that was completed, never a held one
func pauseRedundant(found []PauseFile, held, retired map[string]bool) map[string]bool {
	carriers := map[string]string{}
	for _, f := range found {
		for _, oid := range listItems(orEmptyList(pauseTokenOf(f.Body)["absorbed"])) {
			if _, ok := carriers[pyrepr.Str(oid)]; !ok {
				carriers[pyrepr.Str(oid)] = f.Src
			}
		}
	}
	redundant := map[string]bool{}
	byID := map[string][]string{}
	var order []string
	for _, f := range found {
		raw := pauseTokenOf(f.Body)["pause_id"]
		oid := ""
		if truthy(raw) {
			oid = pyrepr.Str(raw)
		}
		if oid == "" {
			continue
		}
		if carrier, ok := carriers[oid]; retired[oid] || (ok && carrier != f.Src) {
			redundant[f.Src] = true
		} else {
			if _, seen := byID[oid]; !seen {
				order = append(order, oid)
			}
			byID[oid] = append(byID[oid], f.Src)
		}
	}
	rev := map[string]int64{}
	for _, f := range found {
		if truthy(f.Body["rev"]) {
			rev[f.Src], _ = pyIntValue(f.Body["rev"])
		}
	}
	for _, oid := range order {
		copies := byID[oid]
		keep := ""
		for _, s := range copies {
			if held[s] {
				keep = s
				break
			}
		}
		if keep == "" {
			keep = copies[0]
			for _, s := range copies[1:] {
				if rev[s] > rev[keep] {
					keep = s
				}
			}
		}
		for _, s := range copies {
			if s != keep {
				redundant[s] = true
			}
		}
	}
	for s := range held {
		delete(redundant, s)
	}
	return redundant
}

func pauseSurvey(path string) ([]PauseFile, map[string]bool, map[string]bool, error) {
	found, err := pauseFiles(path)
	if err != nil {
		return nil, nil, nil, err
	}
	held := map[string]bool{}
	for _, f := range found {
		if PauseIdentityIsLive(pauseHolder(f.Src, f.Body)) {
			held[f.Src] = true
		}
	}
	retired, err := pauseRetired(path)
	if err != nil {
		return nil, nil, nil, err
	}
	return found, held, pauseRedundant(found, held, retired), nil
}

func pathExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// Every obligation of this install whose holder is dead, empty while another update is live
func PauseOrphans(ctx context.Context, path string) ([]PauseFile, error) {
	if path == "" {
		path = PauseRecordPath()
	}
	if !pathExists(path) && len(pauseClaims(path)) == 0 {
		return nil, nil
	}
	_, release, err := PauseMutex(ctx, PauseMutexWaitSeconds*time.Second)
	if err != nil {
		return nil, err
	}
	found, held, redundant, err := pauseSurvey(path)
	release()
	if err != nil {
		return nil, err
	}
	var out []PauseFile
	for _, f := range found {
		if !held[f.Src] && !redundant[f.Src] {
			out = append(out, f)
		}
	}
	if len(out) == 0 {
		return nil, nil
	}
	elsewhere, err := pauseLiveElsewhere(ctx)
	if err != nil {
		return nil, err
	}
	if elsewhere {
		return nil, nil
	}
	return out, nil
}

// Deletes copies of obligations another file already carries, a busy mutex skipping the sweep
func RetirePauseRedundant(ctx context.Context, path string) error {
	if path == "" {
		path = PauseRecordPath()
	}
	if !pathExists(path) && len(pauseClaims(path)) == 0 && !pathExists(pauseRetiredPath(path)) {
		return nil
	}
	_, release, err := PauseMutex(ctx, PauseMutexWaitSeconds*time.Second)
	if err != nil {
		if IsPauseRecordError(err, "RecordBusy") {
			return nil
		}
		return err
	}
	defer release()
	_, _, redundant, err := pauseSurvey(path)
	if err == nil {
		for src := range redundant {
			_ = os.Remove(src)
		}
		err = prunePauseRetired(path)
	}
	if IsPauseRecordError(err, "RecordBusy") {
		return nil
	}
	return err
}

// Forgets retired ids no file carries any more
func prunePauseRetired(path string) error {
	retired, err := pauseRetired(path)
	if err != nil {
		return err
	}
	var bodies []map[string]any
	for _, src := range append([]string{path}, pauseClaims(path)...) {
		body, err := ReadPauseRecord(src)
		if err != nil {
			return err
		}
		if body != nil {
			bodies = append(bodies, body)
		}
	}
	if len(retired) == 0 {
		return nil
	}
	carried := map[string]bool{}
	for _, b := range bodies {
		carried[pyrepr.Str(pauseTokenOf(b)["pause_id"])] = true
	}
	keep := map[string]bool{}
	for id := range retired {
		if carried[id] {
			keep[id] = true
		}
	}
	if len(keep) > 0 {
		_ = pauseAtomicWrite(pauseRetiredPath(path), map[string]any{"schema": 1, "ids": sortedSet(keep)})
	} else {
		_ = os.Remove(pauseRetiredPath(path))
	}
	return nil
}

// Takes an orphaned record or a dead claimer's claim for this process, false when another launch won it
func ClaimPause(ctx context.Context, src string) (string, map[string]any, bool, error) {
	path := PauseRecordPath()
	mine := filepath.Join(filepath.Dir(path), fmt.Sprintf("%s.%d.%s.claim", filepath.Base(path), os.Getpid(), pauseClaimSuffix()))
	body, err := claimLocked(ctx, path, src, mine)
	if err != nil {
		if isOSError(err) {
			return "", nil, false, nil
		}
		return "", nil, false, err
	}
	if body == nil {
		return "", nil, false, nil
	}
	return mine, body, true, nil
}

func claimLocked(ctx context.Context, path, src, mine string) (map[string]any, error) {
	_, release, err := PauseMutex(ctx, PauseMutexWaitSeconds*time.Second)
	if err != nil {
		return nil, err
	}
	defer release()
	body, err := ReadPauseRecord(src)
	if err != nil {
		return nil, err
	}
	if body == nil || !pyrepr.Equal(body["install_root"], PauseInstallRoot()) {
		return nil, nil
	}
	found, held, redundant, err := pauseSurvey(path)
	if err != nil {
		return nil, err
	}
	listed := false
	for _, f := range found {
		listed = listed || f.Src == src
	}
	if held[src] || redundant[src] || !listed {
		return nil, nil
	}
	body["claimer"] = PauseIdentity(0)
	var rev int64
	if truthy(body["rev"]) {
		if rev, err = pyIntConvert(body["rev"]); err != nil {
			return nil, err
		}
	}
	body["rev"] = rev + 1
	if err := pauseAtomicWrite(mine, body); err != nil {
		return nil, err
	}
	pauseTransferHook("claim")
	_ = os.Remove(src)
	return body, nil
}

// Claims every orphaned pause for the update command and merges their sets
func AdoptPauseOrphans(ctx context.Context) (map[string]any, []string, error) {
	if err := pauseRetireRedundant(ctx, ""); err != nil {
		return nil, nil, err
	}
	found, err := PauseOrphans(ctx, "")
	if err != nil {
		return nil, nil, err
	}
	var adopted map[string]any
	var claims []string
	for _, f := range found {
		won, body, ok, err := pauseClaim(ctx, f.Src)
		if err != nil {
			return nil, nil, err
		}
		if !ok {
			continue
		}
		claims = append(claims, won)
		_, release, err := PauseMutex(ctx, PauseMutexWaitSeconds*time.Second)
		if err != nil {
			return nil, nil, err
		}
		current, err := ReadPauseRecord(won)
		if err == nil {
			if current == nil {
				current = body
			}
			var dropped map[string]any
			if dropped, err = DropNeverStopped(shallowCopy(pauseTokenOf(current))); err == nil {
				adopted, err = MergePauseInto(adopted, dropped)
			}
		}
		release()
		if err != nil {
			return nil, nil, err
		}
	}
	return adopted, claims, nil
}

// Deletes the claims a merged record now carries, a busy mutex leaving them for the redundancy sweep
func ReleasePauseClaims(ctx context.Context, claims []string) error {
	_, release, err := PauseMutex(ctx, PauseMutexWaitSeconds*time.Second)
	if err != nil {
		if IsPauseRecordError(err, "RecordBusy") {
			return nil
		}
		return err
	}
	defer release()
	for _, c := range claims {
		_ = os.Remove(c)
	}
	return nil
}

// Carries the recorded pause identity onto the final token and mirrors it to disk
func FinishPause(ctx context.Context, token, intended, adopted map[string]any) (map[string]any, error) {
	if adopted != nil {
		if _, err := MergePauseInto(token, adopted); err != nil {
			return nil, err
		}
	}
	for _, key := range pauseCarried {
		if v, ok := intended[key]; ok {
			token[key] = v
		}
	}
	if err := SyncPause(ctx, token); err != nil {
		return nil, err
	}
	return token, nil
}

// Puts only an adopted orphan set back on disk unowned after this run rolled its own stops back
func AbandonPause(ctx context.Context, intended, adopted map[string]any) error {
	if adopted == nil {
		return DischargePause(ctx, intended, "")
	}
	merged := shallowCopy(adopted)
	for _, key := range []string{"pause_id", "baselines", "absorbed"} {
		if v, ok := intended[key]; ok {
			merged[key] = v
		}
	}
	return WritePause(ctx, merged, PauseUnowned(), "")
}

func pauseLivePIDs(token map[string]any) map[string]bool {
	live := map[string]bool{}
	for pid, ct := range mapOf(token["identities"]) {
		if PauseIdentityIsLive(map[string]any{"pid": pid, "ct": ct}) {
			live[pid] = true
		}
	}
	return live
}

func pauseWithout(token map[string]any, pids map[string]bool) map[string]any {
	profiles := map[string]any{}
	for p, pid := range mapOf(token["profiles"]) {
		if !pids[pyrepr.Str(pid)] {
			profiles[p] = pid
		}
	}
	token["profiles"] = profiles
	token["unmapped"] = filterByPID(token["unmapped"], pids, false)
	if _, ok := token["posix_units"]; ok {
		token["posix_units"] = filterByPID(token["posix_units"], pids, false)
	}
	return token
}

// Entries whose pid is in or out of pids, keeping their order
func filterByPID(v any, pids map[string]bool, inside bool) []any {
	out := []any{}
	for _, u := range listItems(orEmptyList(v)) {
		if pids[pyrepr.Str(mapOf(u)["pid"])] == inside {
			out = append(out, u)
		}
	}
	return out
}

// True when this update's stop request for pid is still on disk, nil when its evidence cannot be read
func requestOnDisk(token map[string]any, pid string) (*bool, error) {
	raw := mapOf(token["stop_markers"])[pid]
	if !truthy(raw) {
		return boolPtr(false), nil
	}
	path := pyrepr.Str(raw)
	ttl := plannedStopProbe.TTL
	first, err := namesRequest(token, pid, path, &ttl)
	if err != nil {
		return nil, err
	}
	second, err := namesRequest(token, pid, PauseAcceptedPath(path), nil)
	if err != nil {
		return nil, err
	}
	switch {
	case (first != nil && *first) || (second != nil && *second):
		return boolPtr(true), nil
	case first == nil || second == nil:
		return nil, nil
	}
	return boolPtr(false), nil
}

// Whether path holds this stopper's request for pid's incarnation, nil when refused or malformed
func namesRequest(token map[string]any, pid, path string, ttl *int) (*bool, error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return boolPtr(false), nil
	}
	if err != nil {
		return nil, nil
	}
	text, err := pystr.DecodeUTF8Sig(raw)
	if err != nil {
		return nil, nil
	}
	v, err := pyjson.Loads(text)
	if err != nil {
		return nil, nil
	}
	marker, ok := v.(*pyjson.Object)
	if !ok {
		return nil, nil
	}
	rawTarget, okTarget := marker.Get("target_pid")
	rawStopper, okStopper := marker.Get("stopper_pid")
	if !okTarget || !okStopper {
		return nil, nil
	}
	target, err := pyIntConvert(rawTarget)
	if err != nil {
		return nil, nil
	}
	stopper, err := pyIntConvert(rawStopper)
	if err != nil {
		return nil, nil
	}
	wantPID, err := pyIntConvert(pid)
	if err != nil {
		return nil, err
	}
	var wantStopper int64
	if truthy(token["stopper_pid"]) {
		if wantStopper, err = pyIntConvert(token["stopper_pid"]); err != nil {
			return nil, err
		}
	}
	accepted, _ := marker.Get("accepted")
	writtenAt, _ := marker.Get("written_at")
	if !truthy(writtenAt) {
		writtenAt = ""
	}
	if target != wantPID || stopper != wantStopper ||
		(ttl != nil && !truthy(accepted) && plannedStopProbe.IsStale(writtenAt, *ttl)) {
		return boolPtr(false), nil
	}
	expected, _ := marker.Get("target_start_time")
	actual := plannedStopProbe.StartTime(int(wantPID))
	return boolPtr(expected == nil || actual == nil || pyrepr.Equal(expected, actual)), nil
}

// Pids this update asked to stop and pids whose request evidence cannot be read
func pauseAsked(token map[string]any) (map[string]bool, map[string]bool, error) {
	sent := strSet(token["stop_sent"])
	asked, unknown := map[string]bool{}, map[string]bool{}
	for p := range sent {
		asked[p] = true
	}
	for _, raw := range listItems(orEmptyList(token["stop_requested"])) {
		p := pyrepr.Str(raw)
		if sent[p] {
			continue
		}
		verdict, err := requestOnDisk(token, p)
		if err != nil {
			return nil, nil, err
		}
		if verdict == nil {
			unknown[p] = true
		} else if *verdict {
			asked[p] = true
		}
	}
	return asked, unknown, nil
}

// Drops entries still served by the same live incarnation that this update never asked to stop
func DropNeverStopped(token map[string]any) (map[string]any, error) {
	if token["stop_requested"] == nil {
		return token, nil
	}
	asked, unknown, err := pauseAsked(token)
	if err != nil {
		return nil, err
	}
	token["stop_sent"] = sortedSet(asked)
	drop := map[string]bool{}
	for pid := range pauseLivePIDs(token) {
		if !asked[pid] && !unknown[pid] {
			drop[pid] = true
		}
	}
	return pauseWithout(token, drop), nil
}

// Removes and returns the profiles, unmapped and supervised entries whose process is still draining
func SplitDraining(token map[string]any) map[string]any {
	live := pauseLivePIDs(token)
	profiles := map[string]any{}
	for p, pid := range mapOf(token["profiles"]) {
		if live[pyrepr.Str(pid)] {
			profiles[p] = pid
		}
	}
	draining := map[string]any{"profiles": profiles, "unmapped": filterByPID(token["unmapped"], live, true),
		"posix_units": filterByPID(token["posix_units"], live, true)}
	pauseWithout(token, live)
	return draining
}

// Folds an orphaned pause into this update's token so one resume brings both sets back
func MergePauseInto(token, adopted map[string]any) (map[string]any, error) {
	if token == nil {
		token = map[string]any{"resume_needed": true, "profiles": map[string]any{}, "unmapped_pids": []any{}, "unmapped": []any{}}
	}
	token["resume_needed"] = true
	extend := func(key string, items []any) error {
		list, err := setdefaultList(token, key)
		if err != nil {
			return err
		}
		token[key] = extendMissing(list, items)
		return nil
	}
	setdefaults := func(key string) error {
		m, err := setdefaultMap(token, key)
		if err != nil {
			return err
		}
		for k, v := range mapOf(adopted[key]) {
			if _, ok := m[k]; !ok {
				m[k] = v
			}
		}
		return nil
	}
	if err := extend("baselines", listItems(orEmptyList(adopted["baselines"]))); err != nil {
		return nil, err
	}
	var ids []any
	for _, i := range append([]any{adopted["pause_id"]}, listItems(orEmptyList(adopted["absorbed"]))...) {
		if truthy(i) {
			ids = append(ids, i)
		}
	}
	if err := extend("absorbed", ids); err != nil {
		return nil, err
	}
	if err := setdefaults("profiles"); err != nil {
		return nil, err
	}
	if err := extend("unmapped", listItems(orEmptyList(adopted["unmapped"]))); err != nil {
		return nil, err
	}
	if err := setdefaults("identities"); err != nil {
		return nil, err
	}
	_, adoptedHas := adopted["stop_requested"]
	_, tokenHas := token["stop_requested"]
	if adoptedHas || tokenHas {
		requested, sent := adopted["stop_requested"], adopted["stop_sent"]
		if requested == nil {
			keys := make([]string, 0)
			for k := range mapOf(adopted["identities"]) {
				keys = append(keys, k)
			}
			requested = stringList(keys)
			sent = requested
		}
		req := strSet(token["stop_requested"])
		for k := range strSet(requested) {
			req[k] = true
		}
		got := strSet(token["stop_sent"])
		for k := range strSet(sent) {
			got[k] = true
		}
		token["stop_requested"], token["stop_sent"] = sortedSet(req), sortedSet(got)
	}
	if err := setdefaults("cold_start_profiles"); err != nil {
		return nil, err
	}
	if truthy(adopted["cold_start_if_installed"]) {
		token["cold_start_if_installed"] = true
		if v, ok := adopted["attested_generation"]; ok {
			if _, has := token["attested_generation"]; !has {
				token["attested_generation"] = v
			}
		}
	}
	if truthy(adopted["platform"]) {
		token["platform"] = adopted["platform"]
		if err := extend("posix_units", listItems(orEmptyList(adopted["posix_units"]))); err != nil {
			return nil, err
		}
	}
	if err := extend("services", listItems(orEmptyList(adopted["services"]))); err != nil {
		return nil, err
	}
	if services := token["services"].([]any); len(services) > 0 {
		if err := extend("expected_services", services); err != nil {
			return nil, err
		}
		if _, err := setdefaultList(token, "restarted_services"); err != nil {
			return nil, err
		}
		m, err := setdefaultMap(token, "service_profiles")
		if err != nil {
			return nil, err
		}
		for k, v := range mapOf(adopted["service_profiles"]) {
			m[k] = v
		}
	}
	return token, nil
}

func pauseHasWork(token map[string]any) bool {
	for _, u := range listItems(orEmptyList(token["unmapped"])) {
		if truthy(mapOf(u)["argv"]) {
			return true
		}
	}
	return truthy(token["profiles"]) || truthy(token["services"]) || truthy(token["posix_units"]) ||
		truthy(token["cold_start_if_installed"]) || truthy(token["cold_start_profiles"])
}

// Resumes a claimed set under the checkout lock, handing it back untouched when an update holds the checkout
func resumeClaimed(ctx context.Context, claimPath string, body map[string]any) error {
	fence := NewUpdateLock("", "", false)
	acquired, err := fence.AcquireCheckout(PauseInstallRoot())
	if err != nil {
		return err
	}
	if !acquired {
		if _, release, err := PauseMutex(ctx, PauseMutexWaitSeconds*time.Second); err == nil {
			current, rerr := ReadPauseRecord(claimPath)
			if rerr == nil {
				if current == nil {
					current = body
				}
				next := shallowCopy(current)
				next["claimer"] = PauseUnowned()
				_ = pauseAtomicWrite(claimPath, next)
			}
			release()
		} else if !isOSError(err) {
			return err
		}
		return nil
	}
	defer fence.Release(ctx)
	return resumeFenced(ctx, claimPath, body)
}

func resumeFenced(ctx context.Context, claimPath string, body map[string]any) error {
	_, release, err := PauseMutex(ctx, PauseMutexWaitSeconds*time.Second)
	if err != nil {
		return err
	}
	current, err := ReadPauseRecord(claimPath)
	var token, draining map[string]any
	if err == nil {
		if current != nil {
			body = current
		}
		if token, err = DropNeverStopped(shallowCopy(pauseTokenOf(body))); err == nil {
			draining = SplitDraining(token)
		}
	}
	release()
	if err != nil {
		return err
	}
	holdBackingOff(token, draining)
	attempted := relaunchKeys(token)
	token["resume_needed"], token["recovery"] = true, true
	defer func() {
		backOffUnready(token, attempted, draining)
		handBack(ctx, claimPath, body, token, draining)
	}()
	if failure := runPauseResume(ctx, token); failure != "" {
		pauseWarn(pauseStderr, "  ⚠ Could not restart every paused gateway: %s. Run `icebath update` or `icebath gateway start`.", failure)
	}
	return nil
}

// Runs the gateway resume with its output on stderr, any failure or panic returned as its text
func runPauseResume(ctx context.Context, token map[string]any) (failure string) {
	defer func() {
		if r := recover(); r != nil {
			failure = fmt.Sprint(r)
		}
	}()
	if !pauseHasWork(token) {
		token["resume_needed"] = false
		return ""
	}
	pauseWarn(pauseStderr, "→ Restarting gateway(s) paused by an interrupted `icebath update`...")
	if pauseGatewayResumer == nil {
		return "No module named 'hermes_cli.update_cmd_windows'"
	}
	if err := pauseGatewayResumer(ctx, token, pauseStderr); err != nil {
		return err.Error()
	}
	return ""
}

func relaunchKeys(token map[string]any) map[string]bool {
	keys := map[string]bool{}
	for name := range mapOf(token["profiles"]) {
		keys["profile:"+name] = true
	}
	for _, u := range listItems(orEmptyList(token["unmapped"])) {
		if truthy(mapOf(u)["argv"]) {
			keys["unmapped:"+pyrepr.Str(mapOf(u)["pid"])] = true
		}
	}
	for _, u := range listItems(orEmptyList(token["posix_units"])) {
		keys["unit:"+unitKey(mapOf(u))] = true
	}
	return keys
}

func unitKey(u map[string]any) string {
	if truthy(u["unit"]) {
		return pyrepr.Str(u["unit"])
	}
	return pyrepr.Str(u["label"])
}

func pyFloatOf(v any) float64 {
	switch t := v.(type) {
	case float64:
		return t
	case nil:
		return 0
	}
	if n, ok := pyIntValue(v); ok {
		return float64(n)
	}
	if s, ok := v.(string); ok {
		var f float64
		if _, err := fmt.Sscan(strings.TrimSpace(s), &f); err == nil {
			return f
		}
	}
	return 0
}

// Moves entries still inside their relaunch backoff from token into held, owed but not relaunched now
func holdBackingOff(token, held map[string]any) {
	now := pauseNow()
	waiting := map[string]bool{}
	for key, state := range mapOf(token["relaunch_retry"]) {
		if now < pyFloatOf(mapOf(state)["next_at"]) {
			waiting[key] = true
		}
	}
	heldProfiles := mapOf(held["profiles"])
	profiles := map[string]any{}
	for n, p := range mapOf(token["profiles"]) {
		if waiting["profile:"+n] {
			heldProfiles[n] = p
		} else {
			profiles[n] = p
		}
	}
	heldUnmapped := listItems(held["unmapped"])
	for _, u := range listItems(token["unmapped"]) {
		if truthy(mapOf(u)["argv"]) && waiting["unmapped:"+pyrepr.Str(mapOf(u)["pid"])] {
			heldUnmapped = append(heldUnmapped, u)
		}
	}
	held["unmapped"] = heldUnmapped
	token["profiles"] = profiles
	var rest []any
	for _, u := range listItems(token["unmapped"]) {
		if !listContains(heldUnmapped, u) {
			rest = append(rest, u)
		}
	}
	token["unmapped"] = append([]any{}, rest...)
	if units := listItems(token["posix_units"]); truthy(token["posix_units"]) {
		heldUnits := listItems(held["posix_units"])
		for _, u := range units {
			if waiting["unit:"+unitKey(mapOf(u))] {
				heldUnits = append(heldUnits, u)
			}
		}
		held["posix_units"] = heldUnits
		var keep []any
		for _, u := range units {
			if !listContains(heldUnits, u) {
				keep = append(keep, u)
			}
		}
		token["posix_units"] = append([]any{}, keep...)
	}
}

// Starts or extends the backoff of each attempted entry the resume left owed, forgetting recovered ones
func backOffUnready(token map[string]any, attempted map[string]bool, held map[string]any) {
	retry := shallowCopy(mapOf(token["relaunch_retry"]))
	if !truthy(token["resume_deferred"]) {
		unready := relaunchKeys(token)
		for key := range attempted {
			if !unready[key] {
				delete(retry, key)
				continue
			}
			var attempts int64
			if state := mapOf(retry[key]); truthy(state["attempts"]) {
				attempts, _ = pyIntValue(state["attempts"])
			}
			attempts++
			delay := math.Min(pauseRelaunchRetryBase*math.Pow(2, float64(min(attempts-1, 6))), pauseRelaunchRetryMax)
			retry[key] = map[string]any{"attempts": attempts, "next_at": pauseNow() + delay}
		}
	}
	owed := relaunchKeys(token)
	for k := range relaunchKeys(held) {
		owed[k] = true
	}
	kept := map[string]any{}
	for key, state := range retry {
		if owed[key] {
			kept[key] = state
		}
	}
	token["relaunch_retry"] = kept
}

// Retires the claim when nothing is owed, else hands it back unowned for the next launch
func handBack(ctx context.Context, claimPath string, body, token, draining map[string]any) {
	profiles := shallowCopy(mapOf(token["profiles"]))
	for k, v := range mapOf(draining["profiles"]) {
		profiles[k] = v
	}
	token["profiles"] = profiles
	token["unmapped"] = append(append([]any{}, listItems(orEmptyList(token["unmapped"]))...), listItems(draining["unmapped"])...)
	if truthy(draining["posix_units"]) {
		token["posix_units"] = append(append([]any{}, listItems(orEmptyList(token["posix_units"]))...), listItems(draining["posix_units"])...)
	}
	owed := truthy(token["resume_needed"]) || truthy(token["resume_deferred"]) || truthy(draining["profiles"]) ||
		truthy(draining["unmapped"]) || truthy(draining["posix_units"])
	_, release, err := PauseMutex(ctx, PauseMutexWaitSeconds*time.Second)
	if err != nil {
		return
	}
	defer release()
	if owed {
		kept := map[string]any{}
		for k, v := range token {
			if k != "recovery" && k != "resume_deferred" {
				kept[k] = v
			}
		}
		kept["resume_needed"] = true
		next := shallowCopy(body)
		next["token"], next["claimer"] = kept, PauseUnowned()
		_ = pauseAtomicWrite(claimPath, next)
		return
	}
	current, err := ReadPauseRecord(claimPath)
	if err != nil {
		return
	}
	if current == nil {
		current = body
	}
	_ = retirePause(PauseRecordPath(), []PauseFile{{claimPath, current}})
}

// Start of run recovery resuming every orphaned pause, the update command and a booting gateway excepted
func RecoverPauses(ctx context.Context, argv []string) error {
	if argv == nil {
		argv = os.Args[1:]
	}
	command, err := pm.CommandArgv(append([]string{}, argv...))
	if err != nil {
		return err
	}
	if (len(command) >= 1 && command[0] == "update") || (len(command) >= 2 && command[0] == "gateway" && command[1] == "run") {
		return nil
	}
	if failure := recoverPausesGuarded(ctx); failure != "" {
		pauseWarn(pauseStderr, "  ⚠ Paused-gateway recovery skipped: %s", failure)
	}
	return nil
}

func recoverPausesGuarded(ctx context.Context) (failure string) {
	defer func() {
		if r := recover(); r != nil {
			failure = fmt.Sprint(r)
		}
	}()
	if err := pauseRetireRedundant(ctx, ""); err != nil {
		return pauseErrText(err)
	}
	found, err := PauseOrphans(ctx, "")
	if err != nil {
		return pauseErrText(err)
	}
	for _, f := range found {
		won, body, ok, err := pauseClaim(ctx, f.Src)
		if err != nil {
			return pauseErrText(err)
		}
		if ok {
			if err := resumeClaimed(ctx, won, body); err != nil {
				return pauseErrText(err)
			}
		}
	}
	return ""
}
