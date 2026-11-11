package dashboard

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pycodecs"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pyshutil"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

const (
	uiPrefsStoreName     = "ui-preferences.json"
	uiPrefsMaxKeyChars   = 256
	uiPrefsMaxValueChars = 256 * 1024
	uiPrefsMaxKeys       = 512
	uiPrefsMaxTotalChars = 2 * 1024 * 1024
	uiPrefsLockWait      = 3 * time.Second
	uiPrefsLockStale     = 10 * time.Second
	uiPrefsLockPoll      = 20 * time.Millisecond
)

var uiPrefsKeyPrefixes = []string{"hermes.desktop.", "hermes-desktop-", "hermes-onboarding-"}

var uiPrefsThreadLock sync.Mutex

// Write the store refuses for a bad key, value or size, answered 400
type preferencesError struct{ msg string }

func (e *preferencesError) Error() string      { return e.msg }
func (e *preferencesError) PyTypeName() string { return "PreferencesError" }

// Store lock held past the wait or an unreadable store, answered 503
type preferencesBusy struct{ msg string }

func (e *preferencesBusy) Error() string      { return e.msg }
func (e *preferencesBusy) PyTypeName() string { return "PreferencesBusy" }

// Machine preferences file beside the root home, a profile home naming its root and symlinks resolved
func uiPreferencesStorePath(ctx context.Context) (string, error) {
	h, err := filepath.Abs(home.Dir(ctx))
	if err != nil {
		return "", err
	}
	root := h
	if pystr.Lower(filepath.Base(filepath.Dir(h))) == "profiles" {
		root = filepath.Dir(filepath.Dir(h))
	}
	resolved, err := pypath.Resolve(root)
	if err != nil {
		return "", err
	}
	return filepath.Join(resolved, uiPrefsStoreName), nil
}

// String keys with string values of a stored values mapping
func sanitizeUIPreferences(values any) *pyjson.Object {
	out := pyjson.NewObject()
	m, ok := values.(*pyjson.Object)
	if !ok {
		return out
	}
	for _, k := range m.Keys {
		if v, ok := m.Vals[k].(string); ok {
			out.Set(k, v)
		}
	}
	return out
}

// Snapshot of the store and whether it was corrupt, busy for any read failure but a missing file
func readUIPreferencesStore(path string) (*pyjson.Object, bool, error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return pyjson.ObjectOf("exists", false, "revision", int64(0), "values", pyjson.NewObject()), false, nil
	}
	if err != nil {
		return nil, false, &preferencesBusy{"preferences store is unreadable: " + pypath.OSErrorText(err)}
	}
	text, err := pycodecs.Decode(raw, "utf-8-sig")
	if err != nil {
		return nil, false, err
	}
	doc, _ := pyjson.Loads(text)
	obj, ok := doc.(*pyjson.Object)
	if !ok {
		return pyjson.ObjectOf("exists", false, "revision", int64(0), "values", pyjson.NewObject()), true, nil
	}
	revision := any(int64(0))
	switch r := obj.GetOr("revision", nil).(type) {
	case int64:
		revision = r
	case bool:
		revision = r
	}
	generation, _ := obj.GetOr("generation", nil).(string)
	return pyjson.ObjectOf("exists", true, "revision", revision, "values", sanitizeUIPreferences(obj.GetOr("values", nil)),
		"generation", generation), false, nil
}

// Snapshot of the store at path, the machine store when path is empty
func readPreferences(ctx context.Context, path string) (*pyjson.Object, error) {
	if path == "" {
		p, err := uiPreferencesStorePath(ctx)
		if err != nil {
			return nil, err
		}
		path = p
	}
	snap, _, err := readUIPreferencesStore(path)
	return snap, err
}

// Refuses keys outside the Desktop prefixes and values that are not strings under the size cap
func validateUIPreferenceChanges(changes *pyjson.Object) error {
	for _, key := range changes.Keys {
		prefixed := false
		for _, p := range uiPrefsKeyPrefixes {
			if strings.HasPrefix(key, p) {
				prefixed = true
			}
		}
		if !prefixed || utf8.RuneCountInString(key) > uiPrefsMaxKeyChars {
			return &preferencesError{"unsupported preference key: " + pyrepr.Repr(key)}
		}
		value := changes.Vals[key]
		if value == nil {
			continue
		}
		s, ok := value.(string)
		if !ok || utf8.RuneCountInString(s) > uiPrefsMaxValueChars {
			return &preferencesError{"unsupported value for " + key}
		}
	}
	return nil
}

// Owner file naming the lock holder
func uiPrefsOwnerFile(lockDir string) string { return filepath.Join(lockDir, "owner") }

// True while the lock directory still names this token
func uiPrefsOwns(lockDir, token string) bool {
	raw, err := os.ReadFile(uiPrefsOwnerFile(lockDir))
	if err != nil {
		return false
	}
	return strings.TrimPrefix(string(raw), "\ufeff") == token
}

// Removes the lock only while it is still ours
func uiPrefsRelease(lockDir, token string) {
	if !uiPrefsOwns(lockDir, token) {
		return
	}
	if os.Remove(uiPrefsOwnerFile(lockDir)) == nil {
		_ = os.Remove(lockDir)
	}
}

// Breaks a lock directory older than the stale limit
func uiPrefsBreakIfStale(lockDir string) {
	st, err := os.Stat(lockDir)
	if err != nil {
		return
	}
	if time.Since(st.ModTime()) <= uiPrefsLockStale {
		return
	}
	if err := os.Remove(uiPrefsOwnerFile(lockDir)); err == nil || errors.Is(err, fs.ErrNotExist) {
		_ = os.Remove(lockDir)
	}
}

// Takes the cross process store lock shared with the Electron writer, returning its ownership check and release
func uiPrefsStoreLock(path string) (func() bool, func(), error) {
	lockDir := filepath.Join(filepath.Dir(path), filepath.Base(path)+".lock")
	tokenBytes := make([]byte, 8)
	_, _ = rand.Read(tokenBytes)
	token := fmt.Sprintf("%d-%s", os.Getpid(), hex.EncodeToString(tokenBytes))
	deadline := time.Now().Add(uiPrefsLockWait)
	uiPrefsThreadLock.Lock()
	for {
		err := os.Mkdir(lockDir, 0o777)
		if err == nil {
			break
		}
		if !errors.Is(err, fs.ErrExist) {
			uiPrefsThreadLock.Unlock()
			return nil, nil, err
		}
		uiPrefsBreakIfStale(lockDir)
		if time.Now().After(deadline) {
			uiPrefsThreadLock.Unlock()
			return nil, nil, &preferencesBusy{"preferences store is busy"}
		}
		time.Sleep(uiPrefsLockPoll)
	}
	release := func() {
		uiPrefsRelease(lockDir, token)
		uiPrefsThreadLock.Unlock()
	}
	if err := os.WriteFile(uiPrefsOwnerFile(lockDir), []byte(token), 0o666); err != nil {
		release()
		return nil, nil, err
	}
	return func() bool { return uiPrefsOwns(lockDir, token) }, release, nil
}

// Per key merge of the changes, a seed only adding keys the store lacks
func mergeUIPreferences(current, changes *pyjson.Object, seed bool) *pyjson.Object {
	values := current.Copy()
	for _, key := range changes.Keys {
		value := changes.Vals[key]
		switch {
		case seed:
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

// Merges the changes per key under the store lock and returns the resulting snapshot
func writePreferences(ctx context.Context, changes *pyjson.Object, seed bool, path string) (*pyjson.Object, error) {
	if err := validateUIPreferenceChanges(changes); err != nil {
		return nil, err
	}
	if path == "" {
		p, err := uiPreferencesStorePath(ctx)
		if err != nil {
			return nil, err
		}
		path = p
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o777); err != nil {
		return nil, err
	}
	stillOurs, release, err := uiPrefsStoreLock(path)
	if err != nil {
		return nil, err
	}
	defer release()
	current, corrupt, err := readUIPreferencesStore(path)
	if err != nil {
		return nil, err
	}
	currentValues := current.Vals["values"].(*pyjson.Object)
	values := mergeUIPreferences(currentValues, changes, seed)
	total := 0
	for _, k := range values.Keys {
		total += utf8.RuneCountInString(k) + utf8.RuneCountInString(values.Vals[k].(string))
	}
	if values.Len() > uiPrefsMaxKeys || total > uiPrefsMaxTotalChars {
		return nil, &preferencesError{"preferences store is full"}
	}
	if pyjson.Equal(values, currentValues) && (current.Vals["exists"] == true || values.Len() == 0) {
		return current, nil
	}
	revision := uiPrefsRevision(current.Vals["revision"]) + 1
	generation, _ := current.GetOr("generation", "").(string)
	if generation == "" {
		b := make([]byte, 8)
		_, _ = rand.Read(b)
		generation = hex.EncodeToString(b)
	}
	if !stillOurs() {
		return nil, &preferencesBusy{"preferences store lock was taken over"}
	}
	if corrupt {
		backup := filepath.Join(filepath.Dir(path), fmt.Sprintf("%s.corrupt-%d", filepath.Base(path), time.Now().UnixMilli()))
		if err := pyshutil.Copy2(path, backup); err != nil {
			return nil, err
		}
	}
	doc := pyjson.ObjectOf("version", int64(1), "generation", generation, "revision", revision, "values", values)
	if err := config.AtomicJSONWriteMode(ctx, path, doc, 0o600, pyjson.Indent(2)); err != nil {
		return nil, err
	}
	return pyjson.ObjectOf("exists", true, "revision", revision, "values", values, "generation", generation), nil
}

// Integer value of a stored revision, a bool counting as its int
func uiPrefsRevision(v any) int64 {
	switch r := v.(type) {
	case int64:
		return r
	case bool:
		if r {
			return 1
		}
	}
	return 0
}
