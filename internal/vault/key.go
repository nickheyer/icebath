package vault

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json/jsontext"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/nickheyer/icebath/internal/fileutil"
	"github.com/nickheyer/icebath/internal/home"
)

// Keychain service every vault key entry is filed under
const keychainService = "icebath vault key"

// Keychain coordinates of a vault key, the non-secret pointer vault.keyref holds beside the ciphertext
type KeyRef struct {
	Service  string
	Account  string
	VaultDir string
}

// Where a vault key lives, never the key itself
type KeyLocation struct {
	Storage     string
	Path        string
	Service     string
	Account     string
	VaultDir    string
	LeftAccount string
}

// Pointer file layout, written with two space indentation
type keyRefFile struct {
	Storage  string `json:"storage"`
	Service  string `json:"service"`
	Account  string `json:"account"`
	VaultDir string `json:"vault_dir"`
}

// Absolute path with every existing symlink followed and a missing remainder kept as written
func resolvedPath(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	rest := ""
	for cur := abs; ; {
		if real, err := filepath.EvalSymlinks(cur); err == nil {
			return filepath.Join(real, rest), nil
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return filepath.Join(cur, rest), nil
		}
		rest = filepath.Join(filepath.Base(cur), rest)
		cur = parent
	}
}

// Reports whether something is at path
func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// Deletes path, a missing file counting as deleted
func removeFile(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// Bytes of the key file without surrounding ASCII whitespace
func (s *Store) readKeyFile() ([]byte, error) {
	raw, err := os.ReadFile(s.keyFile)
	if err != nil {
		return nil, err
	}
	return bytes.Trim(raw, " \t\n\r\v\f"), nil
}

// Coordinates of a new entry owned by dir under a random account no path can predict
func newKeyRef(dir string) (KeyRef, error) {
	u, err := uuid.NewRandom()
	if err != nil {
		return KeyRef{}, err
	}
	resolved, err := resolvedPath(dir)
	if err != nil {
		return KeyRef{}, err
	}
	return KeyRef{Service: keychainService, Account: "vault-" + hex.EncodeToString(u[:]), VaultDir: resolved}, nil
}

// Reports whether this vault directory created the entry ref names, false for a pointer copied from another profile
func (s *Store) owns(ref KeyRef) bool {
	resolved, err := resolvedPath(s.dir)
	return err == nil && ref.VaultDir != "" && ref.VaultDir == resolved
}

// Error for a pointer file that cannot be read or parsed
func refUnreadable(reason string) error {
	return failure(ErrKeyRef, "vault.keyref is unreadable (%s); see `icebath vault key`", reason)
}

// Pointer saved beside the vault, nil when there is none
func (s *Store) readRef() (*KeyRef, error) {
	raw, err := os.ReadFile(s.keyRef)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	var pe *fs.PathError
	if errors.As(err, &pe) {
		return nil, refUnreadable(pe.Err.Error())
	}
	if err != nil {
		return nil, refUnreadable(err.Error())
	}
	raw = bytes.TrimPrefix(raw, []byte("\xef\xbb\xbf"))
	if !utf8.Valid(raw) {
		return nil, refUnreadable("not UTF-8 text")
	}
	doc, err := parseValue(raw)
	if err != nil {
		return nil, refUnreadable("invalid JSON")
	}
	ms, ok := objectMembers(doc)
	storage, account := lookup(ms, "storage"), lookup(ms, "account")
	if !ok || storage.Kind() != jsontext.KindString || valueText(storage) != "keychain" || !truthy(account) {
		return nil, failure(ErrKeyRef, "vault.keyref does not describe a keychain entry; see `icebath vault key`")
	}
	ref := KeyRef{Service: keychainService, Account: valueText(account)}
	if service := lookup(ms, "service"); truthy(service) {
		ref.Service = valueText(service)
	}
	if dir := lookup(ms, "vault_dir"); dir.Kind() == jsontext.KindString {
		ref.VaultDir = valueText(dir)
	}
	return &ref, nil
}

// Keychain name the entry is filed under, the vault key default when the pointer gives none
func (r KeyRef) service() string {
	if r.Service == "" {
		return keychainService
	}
	return r.Service
}

// Atomically saves ref as the pointer file
func (s *Store) writeRef(ctx context.Context, ref KeyRef) error {
	data, err := fileutil.EncodeJSON(keyRefFile{Storage: "keychain", Service: ref.Service, Account: ref.Account, VaultDir: ref.VaultDir},
		fileutil.JSONStyle{Lines: true, Indent: 2, ASCII: true})
	if err != nil {
		return err
	}
	return s.writePrivate(ctx, s.keyRef, data)
}

// Saves the pointer to an entry just created, deleting the entry unless the pointer landed before the failure
func (s *Store) saveNewRef(ctx context.Context, ref KeyRef) error {
	err := s.writeRef(ctx, ref)
	if err == nil {
		return nil
	}
	if saved, rerr := s.readRef(); rerr == nil && saved != nil && saved.Account == ref.Account {
		return err
	}
	s.deleteKey(ctx, ref)
	return err
}

// Keychain failure the key layer reports in place of a key it could not read or write
type keychainFailure struct {
	msg string
	err error
}

// Message naming what the keychain did
func (e *keychainFailure) Error() string { return e.msg }

// Keychain error behind the failure
func (e *keychainFailure) Unwrap() error { return e.err }

// Failure wrapping err under a formatted message
func keychainFailed(err error, format string, args ...any) error {
	return &keychainFailure{msg: fmt.Sprintf(format, args...), err: err}
}

// Reports whether err came from the keychain rather than from the vault files
func isKeychainFailure(err error) bool {
	var kf *keychainFailure
	return errors.As(err, &kf)
}

// Key the entry holds, nil when the entry is gone, a keychain failure when the keychain cannot answer
func (s *Store) loadKey(ctx context.Context, ref KeyRef) ([]byte, error) {
	if err := s.keychain.Check(ctx); err != nil {
		return nil, keychainFailed(err, "%s", err)
	}
	value, err := s.keychain.Get(ctx, ref.service(), ref.Account)
	if errors.Is(err, ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, keychainFailed(err, "the OS keychain did not answer (%s)", err)
	}
	if value == "" {
		return nil, nil
	}
	return []byte(value), nil
}

// Writes the key into the entry and reads it back, a store returning anything else counting as unusable
func (s *Store) storeKey(ctx context.Context, ref KeyRef, key []byte) error {
	if err := s.keychain.Check(ctx); err != nil {
		return keychainFailed(err, "%s", err)
	}
	if err := s.keychain.Set(ctx, ref.service(), ref.Account, string(key)); err != nil {
		return keychainFailed(err, "the OS keychain refused the key (%s)", err)
	}
	back, err := s.keychain.Get(ctx, ref.service(), ref.Account)
	if err != nil && !errors.Is(err, ErrNotFound) {
		return keychainFailed(err, "the OS keychain refused the key (%s)", err)
	}
	if err != nil || back != string(key) {
		return keychainFailed(nil, "the OS keychain did not return the key it was given")
	}
	return nil
}

// Reports whether the entry is gone afterwards, an entry an earlier cleanup removed included
func (s *Store) deleteKey(ctx context.Context, ref KeyRef) bool {
	if s.keychain.Check(ctx) != nil {
		return false
	}
	_ = s.keychain.Delete(ctx, ref.service(), ref.Account)
	_, err := s.keychain.Get(ctx, ref.service(), ref.Account)
	return errors.Is(err, ErrNotFound)
}

// Deletes the entry an unpublished profile clone was given, reporting whether it is gone
func (v *Vault) DeleteKey(ctx context.Context, ref KeyRef) bool {
	s := &Store{keychain: v.keychain}
	return s.deleteKey(ctx, ref)
}

// Reports whether key opens the vault, true for any key while the vault has no ciphertext
func (s *Store) decrypts(key []byte) (bool, error) {
	if len(key) == 0 {
		return false, nil
	}
	if !s.hasCiphertext() {
		return true, nil
	}
	k, err := parseFernetKey(key)
	if err != nil {
		return false, nil
	}
	blob, err := os.ReadFile(s.data)
	if err != nil {
		return false, err
	}
	_, err = k.decrypt(blob)
	return err == nil, nil
}

// Secret the vault is encrypted under, the file winning over the keychain entry, minted only without ciphertext
func (s *Store) key(ctx context.Context) ([]byte, error) {
	if err := s.ensureDir(ctx); err != nil {
		return nil, err
	}
	if exists(s.keyFile) {
		_ = os.Chmod(s.keyFile, 0o600)
		return s.readKeyFile()
	}
	ref, err := s.readRef()
	if err != nil {
		return nil, err
	}
	why := filepath.Base(s.keyFile) + " is missing"
	if ref != nil {
		adopted, err := s.adopt(ctx, *ref)
		var key []byte
		if err == nil {
			key, err = s.loadKey(ctx, adopted)
		}
		if err != nil && !isKeychainFailure(err) {
			return nil, err
		}
		if err != nil {
			why = "the OS keychain is not reachable here: " + err.Error()
		} else {
			why = fmt.Sprintf("the keychain entry %s / %s is gone", quote(adopted.Service), quote(adopted.Account))
		}
		if len(key) > 0 {
			return key, nil
		}
	}
	if s.hasCiphertext() {
		return nil, s.keyMissing(why)
	}
	return s.createKey(ctx)
}

// Error for ciphertext whose key could not be found, with the ways to recover it
func (s *Store) keyMissing(why string) error {
	return failure(ErrKeyMissing, "The vault key could not be found (%s). vault.json.enc in %s was left untouched. "+
		"If this profile came from another machine, run `icebath vault key migrate --to file` there and "+
		"copy its vault.key next to vault.json.enc; if the keychain entry was deleted, restore it from a "+
		"keychain backup. `icebath vault key` shows where the key is expected. To start over instead, "+
		"delete vault.json.enc (its saved items are lost).", why, s.dir)
}

// Error for a key file that does not open the vault, which no move may overwrite or delete
func (s *Store) keyConflict() error {
	return failure(ErrKeyConflict, "%s does not decrypt vault.json.enc, so the key was not moved and nothing "+
		"was deleted. If the vault's key is in the OS keychain, move this vault.key aside and run "+
		"`icebath vault key` again.", s.keyFile)
}

// New key in the keychain when configured and reachable, else in the 0600 key file
func (s *Store) createKey(ctx context.Context) ([]byte, error) {
	key, err := newFernetKey()
	if err != nil {
		return nil, err
	}
	if s.keyStorage == "keychain" {
		ref, err := newKeyRef(s.dir)
		if err != nil {
			return nil, err
		}
		if err := s.storeKey(ctx, ref, key); err != nil {
			slog.Warn(fmt.Sprintf("vault.key_storage is keychain, but %s; using %s instead", err, s.keyFile))
		} else {
			if err := s.saveNewRef(ctx, ref); err != nil {
				return nil, err
			}
			return key, nil
		}
	}
	if err := removeFile(s.keyRef); err != nil {
		return nil, err
	}
	if err := s.writePrivate(ctx, s.keyFile, key); err != nil {
		return nil, err
	}
	return key, nil
}

// Own entry for a vault moved or cloned from another directory, the entry it came from never deleted
func (s *Store) adopt(ctx context.Context, ref KeyRef) (KeyRef, error) {
	if s.owns(ref) {
		return ref, nil
	}
	key, err := s.loadKey(ctx, ref)
	if err != nil {
		return ref, err
	}
	ok, err := s.decrypts(key)
	if err != nil {
		return ref, err
	}
	if !ok {
		return ref, nil
	}
	mine, err := newKeyRef(s.dir)
	if err != nil {
		return ref, err
	}
	err = s.storeKey(ctx, mine, key)
	if err == nil {
		err = s.writeRef(ctx, mine)
	}
	if err == nil {
		return mine, nil
	}
	if saved, rerr := s.readRef(); rerr == nil && saved != nil && saved.Account == mine.Account {
		return mine, nil
	}
	s.deleteKey(ctx, mine)
	slog.Warn(fmt.Sprintf("could not give the copied vault in %s its own keychain entry", s.dir), "error", err)
	return ref, nil
}

// Where the key lives, never the key itself
func (s *Store) KeyLocation() (KeyLocation, error) {
	ref, err := s.readRef()
	if err != nil {
		return KeyLocation{}, err
	}
	if exists(s.keyFile) {
		where := KeyLocation{Storage: "file", Path: s.keyFile}
		if ref != nil {
			where.LeftAccount = ref.Account
		}
		return where, nil
	}
	if ref != nil {
		return KeyLocation{Storage: "keychain", Service: ref.Service, Account: ref.Account, VaultDir: ref.VaultDir}, nil
	}
	return KeyLocation{Storage: "none", Path: s.keyFile}, nil
}

// Moves the key to target, file or keychain, proving the destination opens the vault before the source goes
func (s *Store) MigrateKey(ctx context.Context, target string) (string, error) {
	if target != "file" && target != "keychain" {
		return "", failure(ErrInvalid, "key storage must be 'file' or 'keychain'")
	}
	var out string
	err := s.locked(ctx, func(ctx context.Context) error {
		if err := s.ensureDir(ctx); err != nil {
			return err
		}
		var err error
		if target == "keychain" {
			out, err = s.migrateToKeychain(ctx)
		} else {
			out, err = s.migrateToFile(ctx)
		}
		return err
	})
	return out, err
}

// Moves the key file into an entry this vault owns, resuming an interrupted move
func (s *Store) migrateToKeychain(ctx context.Context) (string, error) {
	existing, err := s.readRef()
	if err != nil {
		return "", err
	}
	if !exists(s.keyFile) {
		if existing != nil {
			adopted, err := s.adopt(ctx, *existing)
			var key []byte
			if err == nil {
				key, err = s.loadKey(ctx, adopted)
			}
			if err != nil && !isKeychainFailure(err) {
				return "", err
			}
			if err != nil {
				return "", failure(ErrKeyStorage, "cannot read the vault key from the OS keychain: %s", err)
			}
			ok, err := s.decrypts(key)
			if err != nil {
				return "", err
			}
			if !ok {
				return "", s.keyMissing(fmt.Sprintf("the keychain entry %s does not open the vault", quote(existing.Account)))
			}
			return "keychain", nil
		}
		if s.hasCiphertext() {
			return "", s.keyMissing(filepath.Base(s.keyFile) + " is missing")
		}
		if err := s.keychain.Check(ctx); err != nil {
			return "", failure(ErrKeyStorage, "cannot use the OS keychain: %s", err)
		}
		return "keychain", nil
	}
	key, err := s.readKeyFile()
	if err != nil {
		return "", err
	}
	ok, err := s.decrypts(key)
	if err != nil {
		return "", err
	}
	if !ok {
		return "", s.keyConflict()
	}
	fresh := existing == nil || !s.owns(*existing)
	var ref KeyRef
	if fresh {
		if ref, err = newKeyRef(s.dir); err != nil {
			return "", err
		}
	} else {
		ref = *existing
	}
	moveErr := s.storeKey(ctx, ref, key)
	if moveErr == nil {
		back, err := s.loadKey(ctx, ref)
		if err != nil {
			moveErr = err
		} else if ok, err := s.decrypts(back); err != nil {
			return "", err
		} else if !ok {
			moveErr = errors.New("the key read back from the keychain does not decrypt the vault")
		}
	}
	if moveErr != nil {
		if fresh {
			s.deleteKey(ctx, ref)
		}
		return "", failure(ErrKeyStorage, "could not move the vault key to the OS keychain (%s); it stays in %s", moveErr, s.keyFile)
	}
	if fresh {
		if err := s.saveNewRef(ctx, ref); err != nil {
			return "", failure(ErrKeyStorage, "could not save %s (%s); the key stays in %s", filepath.Base(s.keyRef), err, s.keyFile)
		}
	}
	if err := removeFile(s.keyFile); err != nil {
		return "", err
	}
	return "keychain", nil
}

// Moves the keychain entry into the key file, releasing the entry once the file reads back
func (s *Store) migrateToFile(ctx context.Context) (string, error) {
	ref, err := s.readRef()
	if err != nil {
		return "", err
	}
	if exists(s.keyFile) {
		if ref != nil {
			key, err := s.readKeyFile()
			if err != nil {
				return "", err
			}
			ok, err := s.decrypts(key)
			if err != nil {
				return "", err
			}
			if !ok {
				return "", s.keyConflict()
			}
			if err := s.releaseRef(ctx, *ref); err != nil {
				return "", err
			}
		}
		return "file", nil
	}
	if ref == nil {
		if s.hasCiphertext() {
			return "", s.keyMissing(filepath.Base(s.keyFile) + " is missing")
		}
		return "file", nil
	}
	key, err := s.loadKey(ctx, *ref)
	if err != nil {
		return "", failure(ErrKeyStorage, "cannot read the vault key from the OS keychain: %s", err)
	}
	ok, err := s.decrypts(key)
	if err != nil {
		return "", err
	}
	if !ok {
		return "", s.keyMissing(fmt.Sprintf("the keychain entry %s does not hold this vault's key", quote(ref.Account)))
	}
	if err := s.writePrivate(ctx, s.keyFile, key); err != nil {
		return "", err
	}
	back, err := s.readKeyFile()
	if err != nil {
		return "", err
	}
	if !bytes.Equal(back, key) {
		if err := removeFile(s.keyFile); err != nil {
			return "", err
		}
		return "", failure(ErrKeyStorage, "could not write %s; the key stays in the OS keychain", s.keyFile)
	}
	if err := s.releaseRef(ctx, *ref); err != nil {
		return "", err
	}
	return "file", nil
}

// Drops the keychain copy once the file holds the key, keeping the pointer when this vault's own delete fails
func (s *Store) releaseRef(ctx context.Context, ref KeyRef) error {
	if s.owns(ref) && !s.deleteKey(ctx, ref) {
		return nil
	}
	return removeFile(s.keyRef)
}

// Gives a profile clone about to be published at publishedHome an entry of its own, nil when nothing is shared
func (s *Store) OwnCopiedKey(ctx context.Context, publishedHome string) (*KeyRef, error) {
	ref, err := s.readRef()
	if err != nil {
		return nil, err
	}
	if ref == nil || exists(s.keyFile) {
		return nil, nil
	}
	published, err := filepath.Abs(publishedHome)
	if err != nil {
		return nil, err
	}
	key, err := s.loadKey(ctx, *ref)
	var mine KeyRef
	if err == nil {
		ok, derr := s.decrypts(key)
		if derr != nil {
			return nil, derr
		}
		if !ok {
			return nil, nil
		}
		if mine, err = newKeyRef(home.Vault.In(published)); err != nil {
			return nil, err
		}
		err = s.storeKey(ctx, mine, key)
	}
	if err != nil {
		return nil, failure(ErrKeyStorage, "its key is in the OS keychain, which this session cannot use (%s). Clone from a session "+
			"that can reach the keychain, or run `icebath vault key migrate --to file` on the source first.", err)
	}
	if err := s.writeRef(ctx, mine); err != nil {
		s.deleteKey(ctx, mine)
		return nil, err
	}
	return &mine, nil
}
