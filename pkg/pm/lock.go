package pm

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"runtime"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

// LockSchema is the schema number of lock.json and facts.json
const LockSchema = 1

// StoreToken stands for the store root inside recorded env values
const StoreToken = "{{store}}"

//go:embed lock.json
var embeddedLock []byte

// Checked in lockfile beside this package's source, the shipped binary reading its embedded copy
var lockfilePath = func() string {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		return filepath.Join(RepoRoot(), "pm", "lock.json")
	}
	return filepath.Join(filepath.Dir(file), "lock.json")
}

// Path of the lockfile pm lock writes and every install reads
func LockfilePath() string { return lockfilePath() }

// Replaces the lockfile path every install reads and returns the previous lookup
func SwapLockfilePath(fn func() string) func() string {
	prev := lockfilePath
	lockfilePath = fn
	return prev
}

// Empty document of either state file
func emptyLockDocument() map[string]any {
	return map[string]any{"schema": jsonInt(LockSchema), "packages": map[string]any{}}
}

// Reads a lock or facts document, tolerating damage unless strict and preserving unparsable bytes
func readLockDocument(path string, strict bool) (map[string]any, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		if resolvePath(path) == resolvePath(LockfilePath()) {
			data, err = embeddedLock, nil
		} else {
			return emptyLockDocument(), nil
		}
	}
	if err != nil {
		if strict {
			return nil, err
		}
		return emptyLockDocument(), nil
	}
	text := trimBOM(data)
	if v, perr := loadJSON(text); perr == nil {
		if doc, ok := v.(map[string]any); ok && jsonEquals(doc["schema"], LockSchema) {
			if _, ok := doc["packages"].(map[string]any); ok {
				return doc, nil
			}
		}
	}
	if strict {
		return nil, fmt.Errorf("cannot read recorded package state: %s", path)
	}
	if len(bytes.TrimSpace(text)) > 0 {
		preserveCorruptState(path, text)
	}
	return emptyLockDocument(), nil
}

// Keeps an unparsable state file's bytes beside it, or warns when it ships inside a sealed payload
func preserveCorruptState(path string, text []byte) {
	if store, err := StoreRoot(contextBackground()); err == nil {
		if st, serr := os.Stat(filepath.Join(filepath.Dir(store), "manifest.json")); LongRoot(filepath.Dir(path)) == LongRoot(store) && serr == nil && st.Mode().IsRegular() {
			slog.Warn("invalid shipped state file", "path", path)
			return
		}
	}
	_ = os.WriteFile(strings.TrimSuffix(path, filepath.Ext(path))+".corrupt", text, 0o666)
}

// Writes a state document as sorted, indented JSON durably
func writeLockDocument(path string, data any) error {
	body, err := dumpsJSON(data, true)
	if err != nil {
		return err
	}
	return DurableWriteBytes(path, append(body, '\n'))
}

// StaleLockRowError reports a row a concurrent editor changed before this writer published it
type StaleLockRowError struct{ Message string }

func (e *StaleLockRowError) Error() string { return e.Message }

// Lockfile is a snapshot of lock.json with conflict checked publication of changed pins
type Lockfile struct {
	Path     string
	packages map[string]any
	base     map[string]any
	touched  map[string]bool
}

// Reads the lockfile at path, a damaged or missing file reading as no pins
func NewLockfile(path string) *Lockfile {
	resolved := resolvePath(path)
	doc, _ := readLockDocument(resolved, false)
	packages := doc["packages"].(map[string]any)
	return &Lockfile{Path: resolved, packages: packages, base: deepCopyJSON(packages).(map[string]any), touched: map[string]bool{}}
}

// Pinned version of a package, ok false when unpinned
func (l *Lockfile) Version(name string) (string, bool) {
	row, _ := l.packages[name].(map[string]any)
	v, ok := row["version"].(string)
	return v, ok
}

// Every archive a target needs in extraction order, the any target covering target independent pins
func (l *Lockfile) Artifacts(name, target string) []map[string]any {
	row, _ := l.packages[name].(map[string]any)
	artifacts, _ := row["artifacts"].(map[string]any)
	found, ok := artifacts[target]
	if !ok || found == nil {
		found, ok = artifacts["any"]
	}
	if !ok || found == nil {
		return nil
	}
	if list, isList := found.([]any); isList {
		out := make([]map[string]any, 0, len(list))
		for _, item := range list {
			m, _ := item.(map[string]any)
			out = append(out, m)
		}
		return out
	}
	m, _ := found.(map[string]any)
	return []map[string]any{m}
}

// Complete pin table of a package including targets without discovery
func (l *Lockfile) PinnedArtifacts(name string) map[string]any {
	row, _ := l.packages[name].(map[string]any)
	artifacts, _ := row["artifacts"].(map[string]any)
	if artifacts == nil {
		return map[string]any{}
	}
	return deepCopyJSON(artifacts).(map[string]any)
}

// Sorted pinned package names
func (l *Lockfile) Names() []string {
	names := make([]string, 0, len(l.packages))
	for name := range l.packages {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// Replaces a package's pin in the snapshot and marks it for publication
func (l *Lockfile) SetPin(name, version string, artifacts map[string]any) {
	l.setPinValue(name, version, artifacts)
}

// Replaces a pin keeping the version exactly as the row held it, a missing one staying null
func (l *Lockfile) setPinValue(name string, version any, artifacts map[string]any) {
	l.packages[name] = map[string]any{"version": version, "artifacts": deepCopyJSON(artifacts)}
	l.touched[name] = true
}

// Raw version value of a package's row, nil when the row or its version is absent
func (l *Lockfile) versionValue(name string) any {
	row, _ := l.packages[name].(map[string]any)
	return row["version"]
}

// Merges touched rows under the file's lock, refusing every row a concurrent editor changed
func (l *Lockfile) Save() error {
	if err := os.MkdirAll(filepath.Dir(l.Path), 0o777); err != nil {
		return err
	}
	lock, err := os.OpenFile(filepath.Join(filepath.Dir(l.Path), "."+filepath.Base(l.Path)+".lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer lock.Close()
	if _, err := LockFD(lock, true, WaitForever); err != nil {
		return err
	}
	doc, err := readLockDocument(l.Path, true)
	if err != nil {
		return err
	}
	current := doc["packages"].(map[string]any)
	updated := make(map[string]any, len(current))
	for k, v := range current {
		updated[k] = v
	}
	for _, name := range sortedKeys(l.touched) {
		intended := l.packages[name]
		if !jsonDeepEqual(current[name], l.base[name]) && !jsonDeepEqual(current[name], intended) {
			return &StaleLockRowError{Message: fmt.Sprintf("%s changed in %s; read the lockfile again and retry", name, l.Path)}
		}
		updated[name] = intended
	}
	_, statErr := os.Stat(l.Path)
	if !jsonDeepEqual(updated, current) || statErr != nil {
		if err := writeLockDocument(l.Path, map[string]any{"schema": jsonInt(LockSchema), "packages": updated}); err != nil {
			return err
		}
	}
	l.packages = updated
	l.base = deepCopyJSON(updated).(map[string]any)
	l.touched = map[string]bool{}
	return nil
}

// Pinned termux-docker image digest read once for every consumer
func TermuxDockerDigest() string {
	v, _ := NewLockfile(LockfilePath()).Version("termux-docker")
	return v
}

// Facts is the installed state file, written only by pm
type Facts struct {
	Path     string
	packages map[string]any
}

// Identity is the target and artifact digests a lock currently pins for a package
type Identity struct {
	Target  string
	SHA256s []string
}

// Reads the installed state file, strict refusing a damaged or unreadable one
func NewFacts(path string, strict bool) (*Facts, error) {
	doc, err := readLockDocument(path, strict)
	if err != nil {
		return nil, err
	}
	return &Facts{Path: path, packages: doc["packages"].(map[string]any)}, nil
}

// Rereads the file strictly
func (f *Facts) Reload() error {
	doc, err := readLockDocument(f.Path, true)
	if err != nil {
		return err
	}
	f.packages = doc["packages"].(map[string]any)
	return nil
}

// Recorded fact of a package
func (f *Facts) Get(name string) (map[string]any, bool) {
	fact, ok := f.packages[name].(map[string]any)
	return fact, ok
}

var sha256Hex = regexp.MustCompile(`^[a-f0-9]{64}$`)

// Publishes every tool entry's tree digest once packaging stops changing its bytes, returning the count
func (f *Facts) RefreshDigests(storeRoot string) (int, error) {
	doc, err := readLockDocument(f.Path, true)
	if err != nil {
		return 0, err
	}
	packages := doc["packages"].(map[string]any)
	root := LongRoot(resolvePath(storeRoot))
	count := 0
	for _, name := range sortedKeys(packages) {
		fact, ok := packages[name].(map[string]any)
		if !ok {
			return 0, fmt.Errorf("invalid payload fact: %s", name)
		}
		_, hasEntry := fact["entry"]
		if _, hasStamp := fact["stamp"]; !hasEntry && hasStamp {
			continue
		}
		entryName, entryOK := fact["entry"].(string)
		version, versionOK := fact["version"].(string)
		target, targetOK := fact["target"].(string)
		artifacts, artifactsOK := fact["artifacts"].([]any)
		bad := !entryOK || entryName == "" || entryName == "." || entryName == ".." || strings.ContainsAny(entryName, `/\:`) ||
			!versionOK || version == "" || !targetOK || target == "" || !artifactsOK || len(artifacts) == 0
		for _, sha := range artifacts {
			if s, isStr := sha.(string); !isStr || !sha256Hex.MatchString(s) {
				bad = true
			}
		}
		if bad {
			return 0, fmt.Errorf("incomplete payload tool: %s", name)
		}
		entry := filepath.Join(root, entryName)
		if st, err := os.Stat(entry); err != nil || !st.IsDir() || !isRelativeTo(LongRoot(resolvePath(entry)), root) {
			return 0, fmt.Errorf("incomplete payload tool: %s", name)
		}
		digest, err := TreeDigest(entry)
		if err != nil {
			return 0, err
		}
		fact["digest"] = digest
		count++
	}
	if count == 0 {
		return 0, fmt.Errorf("payload facts carry no tool entries: %s", f.Path)
	}
	if err := writeLockDocument(f.Path, map[string]any{"schema": jsonInt(LockSchema), "packages": packages}); err != nil {
		return 0, err
	}
	f.packages = packages
	return count, nil
}

// Reports whether a tool fact matches the expected version and identity and its entry exists
func (f *Facts) Installed(name string, expectedVersion *string, storeRoot string, expectedIdentity *Identity) bool {
	fact, _ := f.packages[name].(map[string]any)
	if len(fact) == 0 {
		return false
	}
	if _, ok := fact["entry"]; !ok {
		return false
	}
	if expectedVersion != nil && !jsonEquals(fact["version"], *expectedVersion) {
		return false
	}
	if expectedIdentity != nil {
		target, hasTarget := fact["target"]
		artifacts, hasArtifacts := fact["artifacts"]
		if !hasTarget || !hasArtifacts {
			return false
		}
		want := make([]any, len(expectedIdentity.SHA256s))
		for i, s := range expectedIdentity.SHA256s {
			want[i] = s
		}
		if !jsonEquals(target, expectedIdentity.Target) || !jsonDeepEqual(artifacts, want) {
			return false
		}
	}
	_, err := os.Stat(filepath.Join(storeRoot, pyrepr.Str(fact["entry"])))
	return err == nil
}

// Recorded environment diff of a package with the store token resolved
func (f *Facts) EnvFor(name, storeRoot string) EnvDiff {
	fact, _ := f.packages[name].(map[string]any)
	env, _ := fact["env"].(map[string]any)
	return resolveStoreToken(env, storeRoot)
}

// Rereads disk and merges so concurrent installs of different packages never clobber each other
func (f *Facts) mergeAndWrite(name string, fact map[string]any) error {
	doc, err := readLockDocument(f.Path, true)
	if err != nil {
		return err
	}
	onDisk := doc["packages"].(map[string]any)
	for key, value := range f.packages {
		if _, ok := onDisk[key]; !ok {
			onDisk[key] = value
		}
	}
	f.packages = onDisk
	f.packages[name] = fact
	return writeLockDocument(f.Path, map[string]any{"schema": jsonInt(LockSchema), "packages": f.packages})
}

// FactRecord is the identity and realized digest an install records beside its entry and env
type FactRecord struct {
	Target    *string
	Artifacts []string
	Digest    *string
}

// Records an installed tool entry, its templated env and the identity it came from
func (f *Facts) Record(name, version, entry string, env EnvDiff, storeRoot string, extra FactRecord) error {
	fact := map[string]any{"entry": entry, "version": version, "env": templatizeStore(env, storeRoot)}
	if extra.Target != nil {
		fact["target"] = *extra.Target
	}
	if extra.Artifacts != nil {
		list := make([]any, len(extra.Artifacts))
		for i, a := range extra.Artifacts {
			list[i] = a
		}
		fact["artifacts"] = list
	}
	if extra.Digest != nil {
		fact["digest"] = *extra.Digest
	}
	return f.mergeAndWrite(name, fact)
}

// Commits a verified state package with its selected environment and resolved lock
func (f *Facts) RecordState(name, stamp string, extras []string, environment, resolvedLock string) error {
	list := make([]any, len(extras))
	for i, e := range extras {
		list[i] = e
	}
	fact := map[string]any{"stamp": stamp, "extras": list}
	if environment != "" {
		fact["environment"] = resolvePath(environment)
	}
	if resolvedLock != "" {
		fact["resolved_lock"] = resolvePath(resolvedLock)
	}
	return f.mergeAndWrite(name, fact)
}

// Drops unselected package facts from an exclusively owned staged store
func (f *Facts) Retain(names map[string]bool) error {
	doc, err := readLockDocument(f.Path, true)
	if err != nil {
		return err
	}
	packages := doc["packages"].(map[string]any)
	retained := map[string]any{}
	for name, fact := range packages {
		if names[name] {
			retained[name] = fact
		}
	}
	if !jsonDeepEqual(retained, packages) {
		if err := writeLockDocument(f.Path, map[string]any{"schema": jsonInt(LockSchema), "packages": retained}); err != nil {
			return err
		}
	}
	f.packages = retained
	return nil
}

// Drops the facts of packages pm no longer defines so gc reclaims their entries
func (f *Facts) Forget(names []string) error {
	doc, err := readLockDocument(f.Path, true)
	if err != nil {
		return err
	}
	keep := map[string]bool{}
	for name := range doc["packages"].(map[string]any) {
		if !slices.Contains(names, name) {
			keep[name] = true
		}
	}
	return f.Retain(keep)
}

// Store entry names recorded by tool facts
func (f *Facts) EntriesInUse() map[string]bool {
	out := map[string]bool{}
	for _, v := range f.packages {
		if fact, ok := v.(map[string]any); ok {
			if entry, has := fact["entry"]; has {
				out[pyrepr.Str(entry)] = true
			}
		}
	}
	return out
}

// Replaces the store root with the store token in every env value
func templatizeStore(env EnvDiff, storeRoot string) map[string]any {
	out := map[string]any{}
	for key, value := range env {
		if list, ok := envList(value); ok {
			items := make([]any, len(list))
			for i, v := range list {
				items[i] = strings.ReplaceAll(v, storeRoot, StoreToken)
			}
			out[key] = items
			continue
		}
		out[key] = strings.ReplaceAll(pyrepr.Str(value), storeRoot, StoreToken)
	}
	return out
}

// Replaces the store token with the store root in every recorded env value
func resolveStoreToken(env map[string]any, storeRoot string) EnvDiff {
	out := EnvDiff{}
	for key, value := range env {
		if list, ok := envList(value); ok {
			items := make([]string, len(list))
			for i, v := range list {
				items[i] = strings.ReplaceAll(v, StoreToken, storeRoot)
			}
			out[key] = items
			continue
		}
		out[key] = strings.ReplaceAll(pyrepr.Str(value), StoreToken, storeRoot)
	}
	return out
}

// String items of a list valued env entry, ok false for a scalar
func envList(value any) ([]string, bool) {
	switch t := value.(type) {
	case []string:
		return t, true
	case []any:
		out := make([]string, len(t))
		for i, v := range t {
			out[i] = pyrepr.Str(v)
		}
		return out, true
	}
	return nil, false
}

// Deep copy of a decoded JSON value
func deepCopyJSON(v any) any {
	switch t := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, x := range t {
			out[k] = deepCopyJSON(x)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, x := range t {
			out[i] = deepCopyJSON(x)
		}
		return out
	case []string:
		return slices.Clone(t)
	}
	return v
}

// Normalizes Go values into decoded JSON shapes so equality matches Python dict comparison
func normalizeJSON(v any) any {
	switch t := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, x := range t {
			out[k] = normalizeJSON(x)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, x := range t {
			out[i] = normalizeJSON(x)
		}
		return out
	case []string:
		out := make([]any, len(t))
		for i, x := range t {
			out[i] = x
		}
		return out
	case json.Number:
		f, err := strconv.ParseFloat(t.String(), 64)
		if err != nil {
			return t.String()
		}
		return pyNumber{f}
	case int:
		return pyNumber{float64(t)}
	case int64:
		return pyNumber{float64(t)}
	case float64:
		return pyNumber{t}
	}
	return v
}

// Numeric value compared across int, float and decoded JSON spellings
type pyNumber struct{ f float64 }

// Decoded JSON spelling of an integer
func jsonInt(n int) json.Number { return json.Number(strconv.Itoa(n)) }

// Background context for layout reads made outside any caller context
func contextBackground() context.Context { return context.Background() }

// Python equality of two decoded JSON trees
func jsonDeepEqual(a, b any) bool {
	return reflect.DeepEqual(normalizeJSON(a), normalizeJSON(b))
}

// Python equality of a decoded JSON scalar against a Go value
func jsonEquals(v any, want any) bool {
	return jsonDeepEqual(v, want)
}

// Keys of a map in sorted order
func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
