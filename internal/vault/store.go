package vault

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/nickheyer/icebath/pkg/fsx"
)

// Kinds of item a vault keeps
var Kinds = []string{"login", "payment", "address"}

// What a login's identifier is
var IdentifierTypes = []string{"email", "phone", "username"}

// Fields of a card and of an address in the order they are asked, each with the WHATWG autocomplete token a fill targets
var (
	PaymentFields = [][2]string{{"card_number", "cc-number"}, {"cardholder_name", "cc-name"}, {"exp_month", "cc-exp-month"}, {"exp_year", "cc-exp-year"},
		{"cvc", "cc-csc"}, {"billing_postal_code", "postal-code"}}
	AddressFields = [][2]string{{"address_line1", "address-line1"}, {"address_line2", "address-line2"}, {"city", "address-level2"},
		{"state", "address-level1"}, {"postal_code", "postal-code"}, {"country", "country-name"}}
)

// Name of a field as people read it
func FieldName(key string) string {
	switch key {
	case "identifier_type":
		return "kind of sign-in"
	case "identifier":
		return "email, phone number or username"
	case "otp_secret":
		return "authenticator key"
	case "card_number":
		return "card number"
	case "cardholder_name":
		return "name on the card"
	case "exp_month":
		return "expiry month"
	case "exp_year":
		return "expiry year"
	case "cvc":
		return "security code"
	case "billing_postal_code":
		return "billing postal code"
	case "address_line1":
		return "first address line"
	case "address_line2":
		return "second address line"
	case "postal_code":
		return "postal code"
	}
	return strings.ReplaceAll(key, "_", " ")
}

// Words joined as a list is said: a, b and c
func joined(words []string) string {
	if len(words) < 2 {
		return strings.Join(words, "")
	}
	return strings.Join(words[:len(words)-1], ", ") + " and " + words[len(words)-1]
}

// Fields an item of a kind can't go without
var required = map[string][]string{"payment": {"card_number", "exp_month", "exp_year", "cvc"}, "address": {"address_line1", "city", "postal_code", "country"}}

// Failure of the vault that is safe to show, never holding a secret value
type Error struct{ Msg string }

func (e *Error) Error() string { return e.Msg }

// Metadata of one item, never its secret
type Item struct {
	ID, Kind, Label, Origin, Created string
	IdentifierType, Identifier       string
	// A login keeps an authenticator key
	OTP bool
	// Every site the item may be filled on, when there is more than one
	Origins []string
}

// One item as the vault file keeps it
type record struct {
	ID             string            `json:"id"`
	Kind           string            `json:"kind"`
	Label          string            `json:"label"`
	Origin         *string           `json:"origin"`
	Created        string            `json:"created_at"`
	IdentifierType *string           `json:"identifier_type"`
	Identifier     *string           `json:"identifier"`
	Secret         map[string]string `json:"secret"`
}

func (r record) item() Item {
	item := Item{ID: r.ID, Kind: r.Kind, Label: r.Label, Created: r.Created, OTP: r.Secret["otp_secret"] != ""}
	if r.Origin != nil {
		item.Origin = *r.Origin
	}
	if r.Identifier != nil && *r.Identifier != "" {
		item.Identifier = *r.Identifier
		if r.IdentifierType != nil {
			item.IdentifierType = *r.IdentifierType
		}
	}
	return item
}

// Read modify write cycles of every vault of this process, the file lock guarding them from other processes
var stores sync.Mutex

// The encrypted vault of one profile: vault.json.enc beside its key, vault.key, or the pointer to its key in the OS keychain, vault.keyref
type Store struct {
	dir string
	// Where a new key goes, file or keychain
	storage string
}

func (s *Store) vaultPath() string { return filepath.Join(s.dir, "vault.json.enc") }
func (s *Store) keyPath() string   { return filepath.Join(s.dir, "vault.key") }
func (s *Store) refPath() string   { return filepath.Join(s.dir, "vault.keyref") }

func (s *Store) ensureDir() error {
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return err
	}
	return os.Chmod(s.dir, 0o700)
}

// Runs the work holding the vault's lock
func (s *Store) locked(work func() error) error {
	stores.Lock()
	defer stores.Unlock()
	if err := s.ensureDir(); err != nil {
		return err
	}
	release, err := fsx.Lock(context.Background(), filepath.Join(s.dir, ".vault.lock"), -1)
	if err != nil {
		return err
	}
	defer release()
	return work()
}

func (s *Store) sealed() bool {
	info, err := os.Stat(s.vaultPath())
	return err == nil && info.Size() > 0
}

func readKey(path string) ([]byte, error) {
	data, err := os.ReadFile(path)
	return bytes.TrimSpace(data), err
}

// The vault's key: its file when there is one, else the keychain entry its pointer names, else a new key for a vault with nothing in it
func (s *Store) key() ([]byte, error) {
	if err := s.ensureDir(); err != nil {
		return nil, err
	}
	if fsx.Exists(s.keyPath()) {
		_ = os.Chmod(s.keyPath(), 0o600)
		return readKey(s.keyPath())
	}
	ref, err := readRef(s.refPath())
	if err != nil {
		return nil, err
	}
	var why string
	if ref != nil {
		adopted, err := s.adopt(ref)
		var key []byte
		if err == nil {
			key, err = loadKey(adopted)
		}
		var unavailable *KeychainUnavailable
		switch {
		case errors.As(err, &unavailable):
			why = "icebath can't reach the keychain of this computer here. " + sentence(unavailable.Msg)
		case err != nil:
			return nil, err
		case len(key) > 0:
			return key, nil
		default:
			why = fmt.Sprintf("The keychain entry %q for %q is gone.", ref.service(), ref.Account)
		}
	} else {
		why = "The file " + filepath.Base(s.keyPath()) + " is missing."
	}
	if s.sealed() {
		return nil, &Error{s.missingKey(why)}
	}
	return s.newKey()
}

func (s *Store) missingKey(why string) string {
	return fmt.Sprintf("icebath can't find the key of the vault. %s The vault file vault.json.enc in %s is untouched.\n", why, s.dir) +
		"If this profile came from another computer, run icebath vault key migrate --to file there and copy its vault.key next to " +
		"vault.json.enc. If the keychain entry was deleted, bring it back from a backup of the keychain. icebath vault key shows where the " +
		"key should be. To start over, delete vault.json.enc, and the items saved in it are lost."
}

func (s *Store) conflictingKey() string {
	return s.keyPath() + " does not open vault.json.enc, so the key was not moved and nothing was deleted. If the key of the vault is in the " +
		"keychain, move this vault.key somewhere else and run icebath vault key again."
}

// Text of a keychain failure as the end of a sentence
func sentence(text string) string {
	text = strings.TrimSpace(text)
	if text == "" {
		return ""
	}
	text = strings.ToUpper(text[:1]) + text[1:]
	if !strings.HasSuffix(text, ".") && !strings.HasSuffix(text, "!") && !strings.HasSuffix(text, "?") {
		text += "."
	}
	return text
}

// New key in the keychain when the settings ask for it and it is reachable, else in the 0600 key file
func (s *Store) newKey() ([]byte, error) {
	key := fernetKey()
	if s.storage == "keychain" {
		ref := newRef(s.dir)
		if err := storeKey(ref, key); err != nil {
			slog.Warn(fmt.Sprintf("vault.key_storage is keychain, but %s; using %s instead", err, s.keyPath()))
		} else {
			if err := saveNewRef(s.refPath(), ref); err != nil {
				return nil, err
			}
			return key, nil
		}
	}
	if err := removeFile(s.refPath()); err != nil {
		return nil, err
	}
	if err := fsx.Write(s.keyPath(), key, 0o600); err != nil {
		return nil, err
	}
	return key, nil
}

// An entry of the vault's own for a vault moved or copied from another folder, never deleting the one it came from; the shared entry
// serves when one of its own can't be made
func (s *Store) adopt(ref *keyRef) (*keyRef, error) {
	if ref.owned(s.dir) {
		return ref, nil
	}
	key, err := loadKey(ref)
	if err != nil {
		return nil, err
	}
	if !s.opens(key) {
		return ref, nil
	}
	mine := newRef(s.dir)
	err = storeKey(mine, key)
	if err == nil {
		err = writeRef(s.refPath(), mine)
	}
	if err == nil {
		return mine, nil
	}
	if saved, rerr := readRef(s.refPath()); rerr == nil && saved != nil && saved.Account == mine.Account {
		return mine, nil
	}
	deleteKey(mine)
	slog.Warn(fmt.Sprintf("could not give the copied vault in %s its own keychain entry", s.dir), "error", err)
	return ref, nil
}

// Whether the key opens the vault, any key opening one with nothing in it
func (s *Store) opens(key []byte) bool {
	if len(key) == 0 {
		return false
	}
	if !s.sealed() {
		return true
	}
	f, err := newFernet(key)
	if err != nil {
		return false
	}
	blob, err := os.ReadFile(s.vaultPath())
	if err != nil {
		return false
	}
	_, err = f.decrypt(blob)
	return err == nil
}

// Where the vault's key lives, never the key itself
type Location struct {
	// file, keychain, or none before the first item
	Storage                string
	Path, Service, Account string
	// Keychain account a move back to the file could not remove yet
	Left string
}

func (s *Store) Location() (Location, error) {
	ref, err := readRef(s.refPath())
	if err != nil {
		return Location{}, err
	}
	if fsx.Exists(s.keyPath()) {
		where := Location{Storage: "file", Path: s.keyPath()}
		if ref != nil {
			where.Left = ref.Account
		}
		return where, nil
	}
	if ref != nil {
		return Location{Storage: "keychain", Service: ref.service(), Account: ref.Account}, nil
	}
	return Location{Storage: "none", Path: s.keyPath()}, nil
}

// Moves the key between its file and the OS keychain, the copy proved to open the vault before the original goes
func (s *Store) Migrate(target string) error {
	if target != "file" && target != "keychain" {
		return &Error{"The vault key can be kept in the keychain or in a file, not in “" + target + "”."}
	}
	return s.locked(func() error {
		if target == "keychain" {
			return s.toKeychain()
		}
		return s.toFile()
	})
}

func (s *Store) toKeychain() error {
	existing, err := readRef(s.refPath())
	if err != nil {
		return err
	}
	if !fsx.Exists(s.keyPath()) {
		if existing != nil {
			adopted, err := s.adopt(existing)
			var key []byte
			if err == nil {
				key, err = loadKey(adopted)
			}
			var unavailable *KeychainUnavailable
			if errors.As(err, &unavailable) {
				return &Error{"icebath can't read the vault key from the keychain of this computer. " + sentence(unavailable.Msg)}
			}
			if err != nil {
				return err
			}
			if !s.opens(key) {
				return &Error{s.missingKey(fmt.Sprintf("The keychain entry %q does not open the vault.", existing.Account))}
			}
			return nil
		}
		if s.sealed() {
			return &Error{s.missingKey("The file " + filepath.Base(s.keyPath()) + " is missing.")}
		}
		if _, err := osKeyring(); err != nil {
			return &Error{"icebath can't use the keychain of this computer. " + sentence(err.Error())}
		}
		return nil
	}
	key, err := readKey(s.keyPath())
	if err != nil {
		return err
	}
	if !s.opens(key) {
		return &Error{s.conflictingKey()}
	}
	ref, fresh := existing, existing == nil || !existing.owned(s.dir)
	if fresh {
		ref = newRef(s.dir)
	}
	err = storeKey(ref, key)
	if err == nil {
		var back []byte
		if back, err = loadKey(ref); err == nil && !s.opens(back) {
			err = &KeychainUnavailable{Msg: "The key read back from the keychain does not open the vault."}
		}
	}
	if err != nil {
		if fresh {
			deleteKey(ref)
		}
		return &Error{fmt.Sprintf("The vault key could not be moved to the keychain, so it stays in %s. %s", s.keyPath(), sentence(err.Error()))}
	}
	if fresh {
		if err := saveNewRef(s.refPath(), ref); err != nil {
			return &Error{fmt.Sprintf("%s could not be saved, so the key stays in %s. %s", filepath.Base(s.refPath()), s.keyPath(), sentence(err.Error()))}
		}
	}
	return removeFile(s.keyPath())
}

func (s *Store) toFile() error {
	ref, err := readRef(s.refPath())
	if err != nil {
		return err
	}
	if fsx.Exists(s.keyPath()) {
		if ref == nil {
			return nil
		}
		key, err := readKey(s.keyPath())
		if err != nil {
			return err
		}
		if !s.opens(key) {
			return &Error{s.conflictingKey()}
		}
		return s.release(ref)
	}
	if ref == nil {
		if s.sealed() {
			return &Error{s.missingKey("The file " + filepath.Base(s.keyPath()) + " is missing.")}
		}
		return nil
	}
	key, err := loadKey(ref)
	if err != nil {
		return &Error{"icebath can't read the vault key from the keychain of this computer. " + sentence(err.Error())}
	}
	if len(key) == 0 || !s.opens(key) {
		return &Error{s.missingKey(fmt.Sprintf("The keychain entry %q does not hold the key of this vault.", ref.Account))}
	}
	if err := fsx.Write(s.keyPath(), key, 0o600); err != nil {
		return err
	}
	if back, err := readKey(s.keyPath()); err != nil || !bytes.Equal(back, key) {
		removeFile(s.keyPath())
		return &Error{fmt.Sprintf("%s could not be written, so the key stays in the keychain.", s.keyPath())}
	}
	return s.release(ref)
}

// Drops the keychain copy once the file holds the key, the pointer kept while a delete of the vault's own entry fails
func (s *Store) release(ref *keyRef) error {
	if ref.owned(s.dir) && !deleteKey(ref) {
		return nil
	}
	return removeFile(s.refPath())
}

func (s *Store) fernet() (*fernet, error) {
	key, err := s.key()
	if err != nil {
		return nil, err
	}
	return newFernet(key)
}

// Every record of the vault, none before the first item
func (s *Store) read() ([]record, error) {
	blob, err := os.ReadFile(s.vaultPath())
	if errors.Is(err, fs.ErrNotExist) || (err == nil && len(blob) == 0) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	f, err := s.fernet()
	if err != nil {
		return nil, err
	}
	data, err := f.decrypt(blob)
	if err != nil {
		return nil, &Error{"The vault file can't be opened. Its key does not fit it, or the file is damaged."}
	}
	var doc struct {
		Items []record `json:"items"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, &Error{"The vault file is damaged, and icebath can't read the items in it."}
	}
	return doc.Items, nil
}

func (s *Store) write(records []record) error {
	if records == nil {
		records = []record{}
	}
	data, err := json.Marshal(map[string]any{"version": 1, "items": records})
	if err != nil {
		return err
	}
	f, err := s.fernet()
	if err != nil {
		return err
	}
	return fsx.Write(s.vaultPath(), f.encrypt(data), 0o600)
}

// Saves an item: a login keeps its identifier as metadata and only its password and authenticator key secret, a card or an address only
// the fields of its kind
func (s *Store) Add(kind, label, origin string, secret map[string]string) (Item, error) {
	if !slices.Contains(Kinds, kind) {
		return Item{}, &Error{fmt.Sprintf("The vault keeps logins, payment cards and addresses, not “%s”.", kind)}
	}
	if label = strings.TrimSpace(label); label == "" {
		return Item{}, &Error{"Give the item a name."}
	}
	rec := record{ID: "vault_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:12], Kind: kind, Label: label,
		Created: time.Now().UTC().Format("2006-01-02T15:04:05.000000+00:00")}
	if kind == "login" {
		if origin == "" {
			return Item{}, &Error{"A login needs the address of the site it is for, such as https://github.com."}
		}
		normal, err := NormalizeOrigin(origin)
		if err != nil {
			return Item{}, err
		}
		idType := secret["identifier_type"]
		if !slices.Contains(IdentifierTypes, idType) {
			return Item{}, &Error{"A login signs in with an email, a phone number or a username, not “" + idType + "”."}
		}
		identifier, password := strings.TrimSpace(secret["identifier"]), secret["password"]
		if identifier == "" || password == "" {
			return Item{}, &Error{"A login needs what you sign in with and a password."}
		}
		seed, err := NormalizeOTP(secret["otp_secret"])
		if err != nil {
			return Item{}, err
		}
		rec.Origin, rec.IdentifierType, rec.Identifier = &normal, &idType, &identifier
		rec.Secret = map[string]string{"password": password}
		if seed != "" {
			rec.Secret["otp_secret"] = seed
		}
	} else {
		fields := AddressFields
		if kind == "payment" {
			fields = PaymentFields
		}
		rec.Secret = map[string]string{}
		for _, f := range fields {
			if value := secret[f[0]]; strings.TrimSpace(value) != "" {
				rec.Secret[f[0]] = value
			}
		}
		var missing []string
		for _, f := range required[kind] {
			if _, ok := rec.Secret[f]; !ok {
				missing = append(missing, f)
			}
		}
		if len(missing) > 0 {
			names := make([]string, len(missing))
			for i, key := range missing {
				names[i] = FieldName(key)
			}
			return Item{}, &Error{fmt.Sprintf("Fill in the %s.", joined(names))}
		}
		if origin != "" {
			normal, err := NormalizeOrigin(origin)
			if err != nil {
				return Item{}, err
			}
			rec.Origin = &normal
		}
	}
	err := s.locked(func() error {
		records, err := s.read()
		if err != nil {
			return err
		}
		return s.write(append(records, rec))
	})
	if err != nil {
		return Item{}, err
	}
	return rec.item(), nil
}

// Every item, never a secret
func (s *Store) Items() ([]Item, error) {
	var out []Item
	err := s.locked(func() error {
		records, err := s.read()
		for _, rec := range records {
			out = append(out, rec.item())
		}
		return err
	})
	return out, err
}

// Removes an item, false when there was none of the id
func (s *Store) Remove(id string) (bool, error) {
	removed := false
	err := s.locked(func() error {
		records, err := s.read()
		if err != nil {
			return err
		}
		kept := slices.DeleteFunc(slices.Clone(records), func(rec record) bool { return rec.ID == id })
		if len(kept) == len(records) {
			return nil
		}
		removed = true
		return s.write(kept)
	})
	return removed, err
}

// Metadata of the item, nil when there is none of the id
func (s *Store) Item(id string) (*Item, error) {
	var found *Item
	err := s.locked(func() error {
		records, err := s.read()
		for _, rec := range records {
			if rec.ID == id {
				item := rec.item()
				found = &item
				break
			}
		}
		return err
	})
	return found, err
}

// Secret of the item, for a fill on the server only: never for a result, a log or an error
func (s *Store) Secret(id string) (map[string]string, error) {
	var found map[string]string
	err := s.locked(func() error {
		records, err := s.read()
		for _, rec := range records {
			if rec.ID == id {
				found = map[string]string{}
				for k, v := range rec.Secret {
					found[k] = v
				}
				break
			}
		}
		return err
	})
	if err != nil {
		return nil, err
	}
	if found == nil {
		return nil, &Error{fmt.Sprintf("The vault has no item with the handle %q. Run icebath vault list to see the handles.", id)}
	}
	return found, nil
}
