package config

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math/big"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

const (
	UIPreferencesStoreName  = "ui-preferences.json"
	UIPreferencesMaxKey     = 256
	UIPreferencesMaxValue   = 256 * 1024
	UIPreferencesMaxKeys    = 512
	UIPreferencesMaxTotal   = 2 * 1024 * 1024
	uiPreferencesLockWait   = 3 * time.Second
	uiPreferencesLockStale  = 10 * time.Second
	uiPreferencesLockPoll   = 20 * time.Millisecond
	uiPreferencesDocVersion = 1
)

var (
	UIPreferenceKeyPrefixes = []string{"icebath.desktop.", "icebath-desktop-", "icebath-onboarding-"}
	uiPreferencesThreadLock sync.Mutex
)

// Write the store refuses for a bad key, value or size, answered 400
type PreferencesError struct{ Message string }

func (e *PreferencesError) Error() string { return e.Message }

// Store lock held past the wait or an unreadable store, answered 503 and retried by the client
type PreferencesBusy struct{ Message string }

func (e *PreferencesBusy) Error() string { return e.Message }

// One key change of a preferences write, a nil value deleting the key
type UIPreferenceChange struct {
	Key   string
	Value any
}

// Snapshot of the store with values in file order, generation set only for an existing store
type UIPreferences struct {
	Exists        bool
	Revision      any
	Values        *pyjson.Object
	Generation    string
	HasGeneration bool
}

// Snapshot as the route serializes it, exists then revision then values then generation
func (p UIPreferences) Dict() *pyjson.Object {
	out := pyjson.NewObject()
	out.Set("exists", p.Exists)
	out.Set("revision", p.Revision)
	out.Set("values", p.Values)
	if p.HasGeneration {
		out.Set("generation", p.Generation)
	}
	return out
}

// Preferences file of the root home, a profile home naming its root, resolved through symlinks
func UIPreferencesStorePath(ctx context.Context, root string) string {
	if root == "" {
		h, _ := filepath.Abs(home.Dir(ctx))
		if strings.ToLower(filepath.Base(filepath.Dir(h))) == "profiles" {
			h = filepath.Dir(filepath.Dir(h))
		}
		root = resolveNonStrict(h)
	}
	return filepath.Join(root, UIPreferencesStoreName)
}

// Absolute path with every existing prefix resolved through symlinks like a non strict resolve
func resolveNonStrict(p string) string {
	p, _ = filepath.Abs(p)
	if resolved, err := filepath.EvalSymlinks(p); err == nil {
		return resolved
	}
	parent := filepath.Dir(p)
	if parent == p {
		return p
	}
	return filepath.Join(resolveNonStrict(parent), filepath.Base(p))
}

func emptyPreferences() UIPreferences {
	return UIPreferences{Revision: int64(0), Values: pyjson.NewObject()}
}

// String keyed string values of a decoded values field in file order
func sanitizePreferenceValues(values any) *pyjson.Object {
	out := pyjson.NewObject()
	obj, ok := values.(*pyjson.Object)
	if !ok {
		return out
	}
	for _, k := range obj.Keys {
		if v, isStr := obj.Vals[k].(string); isStr {
			out.Set(k, v)
		}
	}
	return out
}

// Python int revision of the store, bools and big ints included, zero for any other type
func preferenceRevision(v any) any {
	switch n := v.(type) {
	case int64, *big.Int, bool:
		return n
	}
	return int64(0)
}

// Revision plus one with Python int arithmetic
func nextRevision(v any) any {
	switch n := v.(type) {
	case bool:
		if n {
			return int64(2)
		}
		return int64(1)
	case int64:
		if n < 1<<63-1 {
			return n + 1
		}
		return new(big.Int).Add(big.NewInt(n), big.NewInt(1))
	case *big.Int:
		return new(big.Int).Add(n, big.NewInt(1))
	}
	return int64(1)
}

// Snapshot and whether the file was corrupt, a missing file no store, any other read failure busy
func readPreferencesFile(path string) (UIPreferences, bool, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return emptyPreferences(), false, nil
	}
	if err != nil {
		return UIPreferences{}, false, &PreferencesBusy{Message: "preferences store is unreadable: " + pyOSErrorString(err)}
	}
	data = []byte(strings.TrimPrefix(string(data), "\uFEFF"))
	if !utf8.Valid(data) {
		return UIPreferences{}, false, fmt.Errorf("'utf-8' codec can't decode the preferences store %s", path)
	}
	doc, err := pyjson.Loads(string(data))
	obj, isObj := doc.(*pyjson.Object)
	if err != nil || !isObj {
		return emptyPreferences(), true, nil
	}
	revision, _ := obj.Get("revision")
	values, _ := obj.Get("values")
	generation, _ := obj.Get("generation")
	gen, _ := generation.(string)
	return UIPreferences{Exists: true, Revision: preferenceRevision(revision), Values: sanitizePreferenceValues(values),
		Generation: gen, HasGeneration: true}, false, nil
}

// Snapshot of the store at path, the machine store when empty, busy when it exists but cannot be read
func ReadPreferences(ctx context.Context, path string) (UIPreferences, error) {
	if path == "" {
		path = UIPreferencesStorePath(ctx, "")
	}
	snap, _, err := readPreferencesFile(path)
	return snap, err
}

func validatePreferenceChanges(changes []UIPreferenceChange) error {
	for _, c := range changes {
		if !hasAnyPrefix(c.Key, UIPreferenceKeyPrefixes) || utf8.RuneCountInString(c.Key) > UIPreferencesMaxKey {
			return &PreferencesError{Message: "unsupported preference key: " + PyRepr(c.Key)}
		}
		if c.Value == nil {
			continue
		}
		if s, ok := c.Value.(string); !ok || utf8.RuneCountInString(s) > UIPreferencesMaxValue {
			return &PreferencesError{Message: "unsupported value for " + c.Key}
		}
	}
	return nil
}

func hasAnyPrefix(s string, prefixes []string) bool {
	for _, p := range prefixes {
		if strings.HasPrefix(s, p) {
			return true
		}
	}
	return false
}

func tokenHex8() string {
	b := make([]byte, 8)
	if _, err := io.ReadFull(rand.Reader, b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

// Reports whether the lock directory owner file still names token
func ownsPreferencesLock(lockDir, token string) bool {
	data, err := os.ReadFile(filepath.Join(lockDir, "owner"))
	return err == nil && strings.TrimPrefix(string(data), "\uFEFF") == token
}

// Removes a lock whose directory is older than the stale limit so a dead writer never wedges writes
func breakPreferencesLockIfStale(lockDir string) {
	fi, err := os.Stat(lockDir)
	if err != nil || time.Since(fi.ModTime()) <= uiPreferencesLockStale {
		return
	}
	os.Remove(filepath.Join(lockDir, "owner"))
	os.Remove(lockDir)
}

// Takes the store lock directory shared with the desktop writer, returning the still ours check and release
func lockPreferencesStore(path string) (func() bool, func(), error) {
	lockDir := path + ".lock"
	token := strconv.Itoa(os.Getpid()) + "-" + tokenHex8()
	deadline := time.Now().Add(uiPreferencesLockWait)
	uiPreferencesThreadLock.Lock()
	for {
		err := os.Mkdir(lockDir, 0o777)
		if err == nil {
			break
		}
		if !errors.Is(err, fs.ErrExist) {
			uiPreferencesThreadLock.Unlock()
			return nil, nil, err
		}
		breakPreferencesLockIfStale(lockDir)
		if time.Now().After(deadline) {
			uiPreferencesThreadLock.Unlock()
			return nil, nil, &PreferencesBusy{Message: "preferences store is busy"}
		}
		time.Sleep(uiPreferencesLockPoll)
	}
	release := func() {
		if ownsPreferencesLock(lockDir, token) {
			if os.Remove(filepath.Join(lockDir, "owner")) == nil {
				os.Remove(lockDir)
			}
		}
		uiPreferencesThreadLock.Unlock()
	}
	if err := os.WriteFile(filepath.Join(lockDir, "owner"), []byte(token), 0o666); err != nil {
		release()
		return nil, nil, err
	}
	return func() bool { return ownsPreferencesLock(lockDir, token) }, release, nil
}

// Values after changes, a seed only adding keys the store lacks
func mergePreferences(current *pyjson.Object, changes []UIPreferenceChange, seed bool) *pyjson.Object {
	values := pyjson.NewObject()
	for _, k := range current.Keys {
		values.Set(k, current.Vals[k])
	}
	for _, c := range changes {
		switch {
		case seed:
			if _, have := values.Get(c.Key); c.Value != nil && !have {
				values.Set(c.Key, c.Value)
			}
		case c.Value == nil:
			values.Delete(c.Key)
		default:
			values.Set(c.Key, c.Value)
		}
	}
	return values
}

// Copies a corrupt store aside with its mode and times like shutil copy2
func copyPreferencesAside(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	fi, err := os.Stat(path)
	if err != nil {
		return err
	}
	dest := fmt.Sprintf("%s.corrupt-%d", path, time.Now().UnixMilli())
	if err := os.WriteFile(dest, data, fi.Mode().Perm()); err != nil {
		return err
	}
	if err := os.Chmod(dest, fi.Mode().Perm()); err != nil {
		return err
	}
	return os.Chtimes(dest, fi.ModTime(), fi.ModTime())
}

// Python json dump with two space indents, ASCII escapes only when the text holds lone surrogates
func dumpPreferencesDoc(doc *pyjson.Object) ([]byte, error) {
	text, err := pyjson.Dumps(doc, pyjson.Indent(2), pyjson.EnsureASCII(false))
	if err != nil {
		return nil, err
	}
	if !utf8.ValidString(text) {
		if text, err = pyjson.Dumps(doc, pyjson.Indent(2), pyjson.EnsureASCII(true)); err != nil {
			return nil, err
		}
	}
	return []byte(text), nil
}

// Merges changes per key under the store lock and returns the resulting snapshot
func WritePreferences(ctx context.Context, changes []UIPreferenceChange, seed bool, path string) (UIPreferences, error) {
	if err := validatePreferenceChanges(changes); err != nil {
		return UIPreferences{}, err
	}
	if path == "" {
		path = UIPreferencesStorePath(ctx, "")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o777); err != nil {
		return UIPreferences{}, err
	}
	stillOurs, release, err := lockPreferencesStore(path)
	if err != nil {
		return UIPreferences{}, err
	}
	defer release()
	current, corrupt, err := readPreferencesFile(path)
	if err != nil {
		return UIPreferences{}, err
	}
	values := mergePreferences(current.Values, changes, seed)
	total := 0
	for _, k := range values.Keys {
		total += utf8.RuneCountInString(k) + utf8.RuneCountInString(values.Vals[k].(string))
	}
	if values.Len() > UIPreferencesMaxKeys || total > UIPreferencesMaxTotal {
		return UIPreferences{}, &PreferencesError{Message: "preferences store is full"}
	}
	if reflect.DeepEqual(values.Vals, current.Values.Vals) && (current.Exists || values.Len() == 0) {
		return current, nil
	}
	revision := nextRevision(current.Revision)
	generation := current.Generation
	if generation == "" {
		generation = tokenHex8()
	}
	if !stillOurs() {
		return UIPreferences{}, &PreferencesBusy{Message: "preferences store lock was taken over"}
	}
	if corrupt {
		if err := copyPreferencesAside(path); err != nil {
			return UIPreferences{}, err
		}
	}
	doc := pyjson.NewObject()
	doc.Set("version", uiPreferencesDocVersion)
	doc.Set("generation", generation)
	doc.Set("revision", revision)
	doc.Set("values", values)
	data, err := dumpPreferencesDoc(doc)
	if err != nil {
		return UIPreferences{}, err
	}
	stem := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	if err := atomicWriteMode(path, data, "."+stem+"_", 0o600, true); err != nil {
		return UIPreferences{}, err
	}
	return UIPreferences{Exists: true, Revision: revision, Values: values, Generation: generation, HasGeneration: true}, nil
}
