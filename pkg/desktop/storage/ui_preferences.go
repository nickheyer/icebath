package storage

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pycodecs"

	"github.com/nickheyer/icebath/pkg/desktop/hardening"
)

const (
	// Machine preferences store file name beside the root home
	UIPreferencesFile   = "ui-preferences.json"
	uiPrefsMaxKeyChars  = 256
	uiPrefsMaxValue     = 256 * 1024
	uiPrefsMaxKeys      = 512
	uiPrefsMaxTotal     = 2 * 1024 * 1024
	uiPrefsLockWait     = 3 * time.Second
	uiPrefsLockStale    = 10 * time.Second
	uiPrefsLockPoll     = 20 * time.Millisecond
	uiPrefsWatchEvery   = 1500 * time.Millisecond
	uiPrefsChangedEvent = "ui-preferences:changed"
)

var uiPrefsKeyPrefixes = []string{"icebath.desktop.", "icebath-desktop-", "icebath-onboarding-", "hermes.desktop.", "hermes-desktop-", "hermes-onboarding-"}

// Icebath name of a key a hermes Desktop stored, unchanged for any other key
func uiPreferenceKeyName(key string) string {
	for _, legacy := range []string{"hermes.desktop.", "hermes-desktop-", "hermes-onboarding-"} {
		if strings.HasPrefix(key, legacy) {
			return "icebath" + strings.TrimPrefix(key, "hermes")
		}
	}
	return key
}

// Store state as the renderer sees it, generation absent when no store exists
type UIPreferencesSnapshot struct {
	Exists     bool
	Generation *string
	Revision   float64
	Values     *Object
}

// Snapshot JSON in the key order the Electron store answered with
func (s UIPreferencesSnapshot) MarshalJSON() ([]byte, error) {
	return []byte(jsStringify(s.object(), "")), nil
}

func (s UIPreferencesSnapshot) object() *Object {
	o := NewObject()
	o.Set("exists", s.Exists)
	if s.Generation != nil {
		o.Set("generation", *s.Generation)
	}
	o.Set("revision", s.Revision)
	values := s.Values
	if values == nil {
		values = NewObject()
	}
	o.Set("values", values)
	return o
}

// Validated write of the renderer, a seed only adding keys the store lacks
type UIPreferencesWrite struct {
	Seed   bool
	Values *Object
}

// Store file for a root home
func UIPreferencesPath(hermesHome string) string { return filepath.Join(hermesHome, UIPreferencesFile) }

// Store directory for the root home with symlinks resolved, the absolute path when resolution fails
func UIPreferencesRoot(hermesHome string) string {
	if real, err := filepath.EvalSymlinks(hermesHome); err == nil {
		if abs, err := filepath.Abs(real); err == nil {
			return abs
		}
	}
	abs, _ := filepath.Abs(hermesHome)
	return abs
}

// String entries of a stored values object, hermes Desktop keys read under their icebath names
func sanitizeUIPreferences(values any) *Object {
	out := NewObject()
	m, ok := values.(*Object)
	if !ok {
		return out
	}
	keys := m.Keys()
	for _, k := range keys {
		if v, ok := m.vals[k].(string); ok && uiPreferenceKeyName(k) == k {
			out.Set(k, v)
		}
	}
	for _, k := range keys {
		if v, ok := m.vals[k].(string); ok && uiPreferenceKeyName(k) != k {
			if _, kept := out.Get(uiPreferenceKeyName(k)); !kept {
				out.Set(uiPreferenceKeyName(k), v)
			}
		}
	}
	return out
}

type uiPrefsStoreRead struct {
	corrupt  bool
	snapshot UIPreferencesSnapshot
}

func missingUIPreferences() UIPreferencesSnapshot {
	return UIPreferencesSnapshot{Values: NewObject()}
}

// Reads the store, only a missing file counting as no store and a corrupt one marked for keeping aside
func readUIPreferencesStore(file string) (uiPrefsStoreRead, error) {
	raw, err := os.ReadFile(file)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return uiPrefsStoreRead{snapshot: missingUIPreferences()}, nil
		}
		return uiPrefsStoreRead{}, errors.New("preferences store is unreadable: " + hardening.NodeErr(err).Error())
	}
	text, _ := pycodecs.DecodeReplace(raw, "utf-8")
	doc, perr := jsParse(strings.TrimPrefix(text, "\ufeff"))
	obj, ok := doc.(*Object)
	if perr != nil || !ok {
		return uiPrefsStoreRead{corrupt: true, snapshot: missingUIPreferences()}, nil
	}
	generation := ""
	if g, ok := obj.vals["generation"].(string); ok {
		generation = g
	}
	revision := 0.0
	if r, ok := obj.vals["revision"].(float64); ok && isJSInteger(r) {
		revision = r
	}
	return uiPrefsStoreRead{snapshot: UIPreferencesSnapshot{
		Exists:     true,
		Generation: &generation,
		Revision:   revision,
		Values:     sanitizeUIPreferences(obj.vals["values"]),
	}}, nil
}

// True for a finite number without a fractional part as Number.isInteger answers
func isJSInteger(f float64) bool { return !math.IsInf(f, 0) && !math.IsNaN(f) && math.Trunc(f) == f }

// Snapshot of the store, failing when the file exists but cannot be read
func ReadUIPreferences(file string) (UIPreferencesSnapshot, error) {
	r, err := readUIPreferencesStore(file)
	return r.snapshot, err
}

// Snapshot of the store, nil when it cannot be read yet
func tryReadUIPreferences(file string) *UIPreferencesSnapshot {
	s, err := ReadUIPreferences(file)
	if err != nil {
		return nil
	}
	return &s
}

// Applies a write to a copy of the stored values, a seed never overwriting or deleting
func mergeUIPreferences(current *Object, write UIPreferencesWrite) *Object {
	values := current.Copy()
	for _, key := range write.Values.Keys() {
		value := write.Values.vals[key]
		switch {
		case write.Seed:
			if _, has := values.Get(key); value != nil && !has {
				values.Set(key, value)
			}
		case value == nil:
			values.Delete(key)
		default:
			values.Set(key, value)
		}
	}
	return values
}

// Validates the renderer payload, refusing it whole for any unsupported key or value
func ParseUIPreferencesWrite(payload json.RawMessage) (UIPreferencesWrite, error) {
	var seed, values any
	if len(payload) > 0 {
		if doc, err := jsParse(string(payload)); err == nil {
			if obj, ok := doc.(*Object); ok {
				seed, values = obj.vals["seed"], obj.vals["values"]
			}
		}
	}
	vobj, ok := values.(*Object)
	if !ok {
		return UIPreferencesWrite{}, errors.New("ui preferences write needs a values object")
	}
	for _, key := range vobj.Keys() {
		prefixed := false
		for _, p := range uiPrefsKeyPrefixes {
			if strings.HasPrefix(key, p) {
				prefixed = true
				break
			}
		}
		if !prefixed || jsLength(key) > uiPrefsMaxKeyChars {
			return UIPreferencesWrite{}, errors.New("unsupported preference key: " + key)
		}
		value := vobj.vals[key]
		if value == nil {
			continue
		}
		if s, ok := value.(string); !ok || jsLength(s) > uiPrefsMaxValue {
			return UIPreferencesWrite{}, errors.New("unsupported value for " + key)
		}
	}
	return UIPreferencesWrite{Seed: seed == true, Values: vobj}, nil
}

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func uiPrefsOwnerFile(lockDir string) string { return filepath.Join(lockDir, "owner") }

// True while the lock directory names this token as its holder
func uiPrefsOwns(lockDir, token string) bool {
	raw, err := os.ReadFile(uiPrefsOwnerFile(lockDir))
	if err != nil {
		return false
	}
	text, _ := pycodecs.DecodeReplace(raw, "utf-8")
	return text == token
}

// Removes a lock directory a dead writer left older than the stale limit
func uiPrefsBreakIfStale(lockDir string) {
	st, err := os.Stat(lockDir)
	if err != nil {
		return
	}
	if time.Since(st.ModTime()) > uiPrefsLockStale {
		_ = os.RemoveAll(lockDir)
	}
}

// Runs fn under the cross process store lock shared with the Webapp writer, releasing it only while still ours
func withUIPrefsStoreLock(file string, fn func(stillOurs func() bool) (UIPreferencesSnapshot, error)) (UIPreferencesSnapshot, error) {
	lockDir := file + ".lock"
	token := fmt.Sprintf("%d-%s", os.Getpid(), randomHex(8))
	deadline := time.Now().Add(uiPrefsLockWait)
	for {
		err := os.Mkdir(lockDir, 0o777)
		if err == nil {
			break
		}
		if !errors.Is(err, fs.ErrExist) {
			return UIPreferencesSnapshot{}, hardening.NodeErr(err)
		}
		uiPrefsBreakIfStale(lockDir)
		if time.Now().After(deadline) {
			return UIPreferencesSnapshot{}, errors.New("preferences store is busy")
		}
		time.Sleep(uiPrefsLockPoll)
	}
	defer func() {
		if uiPrefsOwns(lockDir, token) {
			_ = os.RemoveAll(lockDir)
		}
	}()
	if err := os.WriteFile(uiPrefsOwnerFile(lockDir), []byte(token), 0o666); err != nil {
		return UIPreferencesSnapshot{}, hardening.NodeErr(err)
	}
	return fn(func() bool { return uiPrefsOwns(lockDir, token) })
}

// Writes body to a temporary sibling and renames it over file, removing the temporary on failure
func replaceAtomically(file, body string) error {
	temp := filepath.Join(filepath.Dir(file), fmt.Sprintf(".%s.%d.%d.tmp", filepath.Base(file), os.Getpid(), time.Now().UnixMilli()))
	err := os.WriteFile(temp, []byte(body), 0o600)
	if err == nil {
		err = os.Rename(temp, file)
	}
	if err != nil {
		_ = os.Remove(temp)
		return hardening.NodeErr(err)
	}
	return nil
}

// Copies src to dst with the mode of src, failing with the Node copyfile message
func copyFileNode(src, dst string) error {
	fail := func(err error) error {
		var pe *fs.PathError
		if errors.As(err, &pe) {
			err = pe.Err
		}
		return hardening.NodeErr(&os.LinkError{Op: "copyfile", Old: src, New: dst, Err: err})
	}
	data, err := os.ReadFile(src)
	if err != nil {
		return fail(err)
	}
	st, err := os.Stat(src)
	if err != nil {
		return fail(err)
	}
	if err := os.WriteFile(dst, data, st.Mode().Perm()); err != nil {
		return fail(err)
	}
	if err := os.Chmod(dst, st.Mode().Perm()); err != nil {
		return fail(err)
	}
	return nil
}

var uiPrefsWriteChain sync.Mutex

// Merges a write into the store under the store lock and returns the resulting snapshot
func WriteUIPreferences(file string, write UIPreferencesWrite) (UIPreferencesSnapshot, error) {
	uiPrefsWriteChain.Lock()
	defer uiPrefsWriteChain.Unlock()
	if err := os.MkdirAll(filepath.Dir(file), 0o777); err != nil {
		return UIPreferencesSnapshot{}, hardening.NodeErr(err)
	}
	return withUIPrefsStoreLock(file, func(stillOurs func() bool) (UIPreferencesSnapshot, error) {
		read, err := readUIPreferencesStore(file)
		if err != nil {
			return UIPreferencesSnapshot{}, err
		}
		current := read.snapshot
		values := mergeUIPreferences(current.Values, write)
		total := 0
		for _, k := range values.Keys() {
			total += jsLength(k) + jsLength(values.vals[k].(string))
		}
		if values.Len() > uiPrefsMaxKeys || total > uiPrefsMaxTotal {
			return UIPreferencesSnapshot{}, errors.New("preferences store is full")
		}
		if jsStringify(values, "") == jsStringify(current.Values, "") && (current.Exists || values.Len() == 0) {
			return current, nil
		}
		revision := current.Revision + 1
		generation := ""
		if current.Generation != nil {
			generation = *current.Generation
		}
		if generation == "" {
			generation = randomHex(8)
		}
		if !stillOurs() {
			return UIPreferencesSnapshot{}, errors.New("preferences store is busy")
		}
		if read.corrupt {
			if err := copyFileNode(file, fmt.Sprintf("%s.corrupt-%d", file, time.Now().UnixMilli())); err != nil {
				return UIPreferencesSnapshot{}, err
			}
		}
		doc := NewObject()
		doc.Set("version", 1.0)
		doc.Set("generation", generation)
		doc.Set("revision", revision)
		doc.Set("values", values)
		if err := replaceAtomically(file, jsStringify(doc, "  ")+"\n"); err != nil {
			return UIPreferencesSnapshot{}, err
		}
		return UIPreferencesSnapshot{Exists: true, Generation: &generation, Revision: revision, Values: values}, nil
	})
}

// Machine preferences store of a root home, pushing every change to the app windows
type UIPreferences struct {
	file      string
	broadcast func(UIPreferencesSnapshot)
	mu        sync.Mutex
	last      *string
	stop      chan struct{}
	stopOnce  sync.Once
}

func uiPrefsStamp(s *UIPreferencesSnapshot) *string {
	if s == nil {
		return nil
	}
	gen := ""
	if s.Generation != nil {
		gen = *s.Generation
	}
	stamp := fmt.Sprintf("%s:%s:%t", gen, jsNumber(s.Revision), s.Exists)
	return &stamp
}

// Opens the store of a root home and polls it so writes by the Webapp reach every window
func NewUIPreferences(hermesHome string, broadcast func(UIPreferencesSnapshot)) *UIPreferences {
	u := &UIPreferences{file: UIPreferencesPath(UIPreferencesRoot(hermesHome)), broadcast: broadcast, stop: make(chan struct{})}
	u.last = uiPrefsStamp(tryReadUIPreferences(u.file))
	go u.watch()
	return u
}

// Store file this instance serves
func (u *UIPreferences) File() string { return u.file }

// Broadcasts a snapshot whose generation, revision or existence differs from the last one seen
func (u *UIPreferences) publish(s *UIPreferencesSnapshot) {
	u.mu.Lock()
	next := uiPrefsStamp(s)
	changed := s != nil && (u.last == nil || *next != *u.last)
	if changed {
		u.last = next
	}
	u.mu.Unlock()
	if changed {
		u.broadcast(*s)
	}
}

// Pre paint snapshot, nil when the store cannot be read yet
func (u *UIPreferences) Snapshot() *UIPreferencesSnapshot { return tryReadUIPreferences(u.file) }

// Current snapshot, failing when the store exists but cannot be read
func (u *UIPreferences) Read() (UIPreferencesSnapshot, error) { return ReadUIPreferences(u.file) }

// Validates and merges a renderer write, then pushes the result to every window
func (u *UIPreferences) Write(payload json.RawMessage) (UIPreferencesSnapshot, error) {
	write, err := ParseUIPreferencesWrite(payload)
	if err != nil {
		return UIPreferencesSnapshot{}, err
	}
	snap, err := WriteUIPreferences(u.file, write)
	if err != nil {
		return UIPreferencesSnapshot{}, err
	}
	u.publish(&snap)
	return snap, nil
}

// Stops polling the store file
func (u *UIPreferences) Close() { u.stopOnce.Do(func() { close(u.stop) }) }

// Polls the store's stat every watch interval and publishes a fresh read when it changes
func (u *UIPreferences) watch() {
	ticker := time.NewTicker(uiPrefsWatchEvery)
	defer ticker.Stop()
	prev := statKeyOf(u.file)
	for {
		select {
		case <-u.stop:
			return
		case <-ticker.C:
			next := statKeyOf(u.file)
			if !sameStat(prev, next) {
				prev = next
				u.publish(tryReadUIPreferences(u.file))
			}
		}
	}
}
