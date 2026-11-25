package activesessions

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"math/big"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/proc"
	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
)

// RegistryError reports an unreadable registry or an owner whose liveness cannot be proven
type RegistryError struct{ msg string }

// Message of the registry failure
func (e *RegistryError) Error() string { return e.msg }

// Lease age in whole minutes, hours past sixty minutes
func FormatAge(seconds float64) string {
	minutes := max(0, int(math.Floor(seconds/60)))
	if minutes < 60 {
		return fmt.Sprintf("%dm", minutes)
	}
	hours, rest := minutes/60, minutes%60
	if rest == 0 {
		return fmt.Sprintf("%dh", hours)
	}
	return fmt.Sprintf("%dh%dm", hours, rest)
}

// Details line every owner refusal message carries, the surface and age of the holding lease
func SessionOwnerDetails(sessionID string, entry *jsonutil.Object) string {
	surface := "another surface"
	if v := entry.ValueOr("surface", nil); anyutil.IsTrue(v) {
		surface = anyutil.String(v)
	}
	age := ""
	if started, ok := optionalFloat(entry.ValueOr("started_at", nil)); ok && started != 0 {
		age = " " + FormatAge(float64(time.Now().UnixNano())/1e9-started) + " ago"
	}
	return fmt.Sprintf("Details: session %s opened by %s%s.", sessionID, surface, age)
}

// Registry file and lock file under the runtime directory of the registry home, the active home when empty
func leasePaths(ctx context.Context, registryHome string) (string, string) {
	h := resolveRegistryHome(ctx, registryHome)
	return filepath.Join(h, "runtime", "active_sessions.json"), filepath.Join(h, "runtime", "active_sessions.lock")
}

// Registry home given, else the active icebath home
func resolveRegistryHome(ctx context.Context, registryHome string) string {
	if registryHome != "" {
		return registryHome
	}
	return home.Dir(ctx)
}

// Registry file of a registry home
func registryStatePath(ctx context.Context, registryHome string) string {
	return filepath.Join(resolveRegistryHome(ctx, registryHome), "runtime", "active_sessions.json")
}

// Registry lock file of a registry home
func registryLockPath(ctx context.Context, registryHome string) string {
	return filepath.Join(resolveRegistryHome(ctx, registryHome), "runtime", "active_sessions.lock")
}

// Registry entries, every malformed shape an error when strict and dropped otherwise
func readEntries(path string, strict bool) ([]*jsonutil.Object, error) {
	invalid := func(what string) error {
		return &RegistryError{msg: fmt.Sprintf("active session registry %s: %s", what, path)}
	}
	raw, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	var data any
	if err == nil {
		if !utf8.Valid(raw) {
			err = errors.New("invalid utf-8")
		} else {
			data, err = jsonutil.Decode(strings.TrimPrefix(string(raw), "\ufeff"))
		}
	}
	if err != nil {
		if strict {
			return nil, invalid("unreadable")
		}
		pkgLog().Warn(fmt.Sprintf("Ignoring corrupt active session registry at %s", path))
		return nil, nil
	}
	entries := data
	if obj, ok := data.(*jsonutil.Object); ok {
		entries = obj.ValueOr("entries", nil)
	}
	list, ok := entries.([]any)
	if !ok {
		if strict {
			return nil, invalid("has invalid shape")
		}
		return nil, nil
	}
	var valid []*jsonutil.Object
	for _, entry := range list {
		if obj, ok := entry.(*jsonutil.Object); ok {
			valid = append(valid, obj)
		}
	}
	if !strict {
		return withoutQueuedReleases(path, valid), nil
	}
	if len(valid) != len(list) {
		return nil, invalid("contains invalid entries")
	}
	seen := map[string]bool{}
	for _, entry := range valid {
		leaseID, _ := entry.ValueOr("lease_id", nil).(string)
		checks := []struct {
			bad  func() bool
			what string
		}{
			{func() bool { return !nonblankStr(entry.ValueOr("lease_id", nil)) }, "an invalid lease id"},
			{func() bool { return seen[leaseID] }, "a duplicate lease id"},
			{func() bool { return !nonblankStr(entry.ValueOr("session_id", nil)) }, "an invalid session id"},
			{func() bool { return registryPid(entry.ValueOr("pid", nil)) <= 0 }, "an invalid pid"},
			{func() bool { return !optionalString(entry.ValueOr("surface", nil)) }, "an invalid surface"},
			{func() bool { return !optionalBool(entry.ValueOr("track_liveness", nil)) }, "an invalid liveness marker"},
			{func() bool { return !optionalObject(entry.ValueOr("metadata", nil)) }, "invalid metadata"},
			{func() bool { return !validProcessStart(entry.ValueOr("process_start_time", nil)) }, "an invalid process start time"},
		}
		for _, check := range checks {
			if check.bad() {
				return nil, invalid("contains " + check.what)
			}
		}
		seen[leaseID] = true
	}
	return withoutQueuedReleases(path, valid), nil
}

// True for a string with a non whitespace character
func nonblankStr(v any) bool {
	s, ok := v.(string)
	return ok && strings.TrimSpace(s) != ""
}

// True for nil or a string
func optionalString(v any) bool {
	_, ok := v.(string)
	return v == nil || ok
}

// True for nil or a bool
func optionalBool(v any) bool {
	_, ok := v.(bool)
	return v == nil || ok
}

// True for nil or a JSON object
func optionalObject(v any) bool {
	_, ok := v.(*jsonutil.Object)
	return v == nil || ok
}

// Registry pid as an int, zero for bools, other types and unparseable text
func registryPid(pid any) int {
	switch v := pid.(type) {
	case int64:
		return int(v)
	case *big.Int:
		if v.IsInt64() {
			return int(v.Int64())
		}
		if v.Sign() > 0 {
			return math.MaxInt
		}
		return math.MinInt
	case string:
		n, ok := intText(v)
		if !ok {
			return 0
		}
		return n
	}
	return 0
}

// Python int() of text, surrounding whitespace and underscores between digits accepted
func intText(s string) (int, bool) {
	t := strings.TrimSpace(s)
	if t == "" || strings.HasPrefix(t, "_") || strings.HasSuffix(t, "_") || strings.Contains(t, "__") {
		return 0, false
	}
	digits := strings.TrimLeft(t, "+-")
	if len(t)-len(digits) > 1 || strings.HasPrefix(digits, "_") {
		return 0, false
	}
	n, ok := new(big.Int).SetString(strings.ReplaceAll(t, "_", ""), 10)
	if !ok {
		return 0, false
	}
	if n.IsInt64() {
		return int(n.Int64()), true
	}
	if n.Sign() > 0 {
		return math.MaxInt, true
	}
	return math.MinInt, true
}

// True for an absent start time or one that parses to a finite float
func validProcessStart(v any) bool {
	if v == nil || v == "" {
		return true
	}
	parsed, ok := optionalFloat(v)
	return ok && !math.IsInf(parsed, 0) && !math.IsNaN(parsed)
}

// Python float() of a registry value, false for nil, empty text and values float rejects
func optionalFloat(v any) (float64, bool) {
	switch x := v.(type) {
	case nil:
		return 0, false
	case bool:
		if x {
			return 1, true
		}
		return 0, true
	case int64:
		return float64(x), true
	case float64:
		return x, true
	case *big.Int:
		f, _ := new(big.Float).SetInt(x).Float64()
		if math.IsInf(f, 0) {
			return 0, false
		}
		return f, true
	case string:
		if x == "" {
			return 0, false
		}
		return floatText(x)
	}
	return 0, false
}

// Python float() of text, whitespace stripped and inf and nan spellings accepted
func floatText(s string) (float64, bool) {
	t := strings.TrimSpace(s)
	switch strings.ToLower(strings.TrimLeft(t, "+-")) {
	case "inf", "infinity", "nan":
		f, err := strconv.ParseFloat(t, 64)
		return f, err == nil
	}
	if t == "" || strings.ContainsAny(t, "xXpP") || strings.HasPrefix(t, "_") || strings.HasSuffix(t, "_") || strings.Contains(t, "__") {
		return 0, false
	}
	f, err := strconv.ParseFloat(strings.ReplaceAll(t, "_", ""), 64)
	if err != nil {
		var numErr *strconv.NumError
		if errors.As(err, &numErr) && numErr.Err == strconv.ErrRange {
			return f, true
		}
		return 0, false
	}
	return f, true
}

// Writes the entries list as compact sorted JSON through an atomic replace
var writeEntries = func(path string, entries []*jsonutil.Object) error {
	list := make([]any, len(entries))
	for i, entry := range entries {
		list[i] = entry
	}
	return config.AtomicJSONWriteWith(context.Background(), path, jsonutil.ObjectOf("entries", list), jsonutil.SortKeys(), jsonutil.Spaced())
}

// Creation time of a process in epoch seconds, false when it cannot be read
var processStartTime = func(pid int) (float64, bool) {
	start, err := proc.Birth(pid)
	if err != nil {
		return 0, false
	}
	return start, true
}

var (
	ownStartMu  sync.Mutex
	ownStartPid int
	ownStartAt  float64
	ownStartSet bool
)

// Creation time of this process read once per pid
func ownStartTime() (float64, bool) {
	ownStartMu.Lock()
	defer ownStartMu.Unlock()
	pid := os.Getpid()
	if !ownStartSet || ownStartPid != pid {
		start, ok := processStartTime(pid)
		if !ok {
			return 0, false
		}
		ownStartPid, ownStartAt, ownStartSet = pid, start, true
	}
	return ownStartAt, true
}

// Liveness verdicts of a lease owner
const (
	livenessUnknown = iota
	livenessDead
	livenessAlive
)

// Probes whether an owner pid is the process that took the lease, lenient never answering unknown
func pidLiveness(pid, processStart any, lenient bool) int {
	unknownDead := livenessUnknown
	if lenient {
		unknownDead = livenessDead
	}
	pidInt := pythonIntOrZero(pid)
	if pidInt <= 0 {
		return unknownDead
	}
	isSelf := pidInt == os.Getpid()
	if !isSelf {
		exists, err := pidExists(pidInt)
		if err != nil {
			return unknownDead
		}
		if !exists {
			return livenessDead
		}
	}
	expected, ok := optionalFloat(processStart)
	if !ok {
		return livenessAlive
	}
	var current float64
	if isSelf {
		current, ok = ownStartTime()
	} else {
		current, ok = processStartTime(pidInt)
	}
	if !ok {
		if lenient {
			return livenessAlive
		}
		return livenessUnknown
	}
	if math.Abs(current-expected) < 0.001 {
		return livenessAlive
	}
	return livenessDead
}

// Python int() of a pid field, zero when it raises
func pythonIntOrZero(v any) int {
	switch x := v.(type) {
	case bool:
		if x {
			return 1
		}
		return 0
	case float64:
		if math.IsInf(x, 0) || math.IsNaN(x) {
			return 0
		}
		return int(math.Trunc(x))
	}
	return registryPid(v)
}

// Process existence probe the liveness check uses, zombies counting as dead and a failed probe as an error
var pidExists = func(pid int) (bool, error) { return proc.Exists(pid), nil }

// Entries whose owner is alive, tracked or strict entries required to be provably alive
var pruneDead = func(entries []*jsonutil.Object, strict bool, targetSessionID *string, targetPid *int) ([]*jsonutil.Object, error) {
	targeted := targetSessionID != nil || targetPid != nil
	var live []*jsonutil.Object
	for _, entry := range entries {
		tracked := strict || anyutil.IsTrue(entry.ValueOr("track_liveness", nil))
		state := pidLiveness(entry.ValueOr("pid", nil), entry.ValueOr("process_start_time", nil), !tracked)
		if state == livenessUnknown {
			sessionMatch := targetSessionID != nil && anyutil.String(orEmpty(entry.ValueOr("session_id", nil))) == *targetSessionID
			pidMatch := targetPid != nil && anyutil.Equal(entry.ValueOr("pid", nil), int64(*targetPid))
			if !targeted || sessionMatch || pidMatch {
				return nil, &RegistryError{msg: "active session owner liveness is unknown"}
			}
			state = livenessAlive
		}
		if state == livenessAlive {
			live = append(live, entry)
		}
	}
	return live, nil
}

// Live leases of the registry, dead ones written out under a second lock, strict requiring provable owners
func RegistrySnapshot(ctx context.Context, registryHome string, strict bool) ([]*jsonutil.Object, error) {
	statePath, lockPath := leasePaths(ctx, registryHome)
	unlock, err := enterFileLock(lockPath)
	if err != nil {
		return nil, err
	}
	raw, err := readEntries(statePath, true)
	unlock()
	if err != nil {
		return nil, err
	}
	entries, err := pruneDead(raw, strict, nil, nil)
	if err != nil {
		return nil, err
	}
	if len(entries) == len(raw) {
		return entries, nil
	}
	live := map[string]bool{}
	for _, entry := range entries {
		live[leaseIDOf(entry)] = true
	}
	dead := map[string]bool{}
	for _, entry := range raw {
		if id := leaseIDOf(entry); !live[id] {
			dead[id] = true
		}
	}
	unlock, err = enterFileLock(lockPath)
	if err != nil {
		return nil, err
	}
	defer unlock()
	current, err := readEntries(statePath, true)
	if err != nil {
		return nil, err
	}
	var kept []*jsonutil.Object
	for _, entry := range current {
		if !dead[leaseIDOf(entry)] {
			kept = append(kept, entry)
		}
	}
	if len(kept) != len(current) {
		if err := writeEntries(statePath, kept); err != nil {
			return nil, err
		}
	}
	return entries, nil
}

// Lease id of an entry as text, empty when falsy
func leaseIDOf(entry *jsonutil.Object) string {
	return anyutil.String(orEmpty(entry.ValueOr("lease_id", nil)))
}

// Value itself when truthy, else the empty string
func orEmpty(v any) any {
	if anyutil.IsTrue(v) {
		return v
	}
	return ""
}
