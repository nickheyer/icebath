package vault

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json/jsontext"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/nickheyer/icebath/internal/fileutil"
	"github.com/nickheyer/icebath/internal/home"
)

// Encrypted item store of one profile home, its key in a 0600 file or the OS keychain
type Store struct {
	resolver   *home.Resolver
	keychain   *Keychain
	home       string
	dir        string
	data       string
	keyFile    string
	keyRef     string
	lock       string
	keyStorage string
}

// Decrypted vault items with the version text kept only when newer than the one this build writes
type document struct {
	items []jsontext.Value
	newer string
}

// Item vault of the profile home ctx resolves to
func (v *Vault) Store(ctx context.Context, opts Options) (*Store, error) {
	h, err := v.resolver.Home(ctx)
	if err != nil {
		return nil, err
	}
	return v.StoreAt(h, opts)
}

// Store below homeDir, a profile home that need not be the active one
func (v *Vault) StoreAt(homeDir string, opts Options) (*Store, error) {
	abs, err := filepath.Abs(homeDir)
	if err != nil {
		return nil, err
	}
	return &Store{resolver: v.resolver, keychain: v.keychain, home: abs, dir: home.Vault.In(abs),
		data: home.VaultData.In(abs), keyFile: home.VaultKey.In(abs), keyRef: home.VaultKeyRef.In(abs),
		lock: home.VaultLock.In(abs), keyStorage: KeyStorage(opts.KeyStorage)}, nil
}

// Creates the vault directory owner-only and applies the home directory policy to it
func (s *Store) ensureDir(ctx context.Context) error {
	if err := s.resolver.MakeUserDir(ctx, s.dir, 0o700, true); err != nil {
		return err
	}
	home.ApplySecureDirPolicy(s.dir, s.home)
	return nil
}

// Runs fn holding the cross-process vault lock so concurrent writers never drop each other's items
func (s *Store) locked(ctx context.Context, fn func(context.Context) error) error {
	if err := s.ensureDir(ctx); err != nil {
		return err
	}
	lock, err := fileutil.OpenLock(ctx, s.lock)
	if err != nil {
		return err
	}
	defer func() { _ = lock.Release() }()
	return fn(lock.Context(ctx))
}

// Atomically replaces path with data owner-only and durable, never recreating a deleted profile
func (s *Store) writePrivate(ctx context.Context, path string, data []byte) error {
	if err := s.resolver.Mkdir(ctx, filepath.Dir(path)); err != nil {
		return err
	}
	return fileutil.WriteFile(ctx, path, data, fileutil.Private(), fileutil.SyncDir())
}

// Reports whether the vault file exists with content
func (s *Store) hasCiphertext() bool {
	fi, err := os.Stat(s.data)
	return err == nil && fi.Size() > 0
}

// Decrypted vault, empty for a missing or empty file, a non-list items member giving no items
func (s *Store) read(ctx context.Context) (document, error) {
	blob, err := os.ReadFile(s.data)
	if errors.Is(err, fs.ErrNotExist) {
		return document{}, nil
	}
	if err != nil {
		return document{}, err
	}
	if len(blob) == 0 {
		return document{}, nil
	}
	key, err := s.cipher(ctx)
	if err != nil {
		return document{}, err
	}
	plain, err := key.decrypt(blob)
	if err != nil {
		return document{}, failure(ErrUndecryptable, "vault file could not be decrypted (key mismatch or corruption)")
	}
	v, err := parseValue(plain)
	if err != nil {
		return document{}, failure(ErrCorrupt, "vault file is corrupted (invalid JSON)")
	}
	top, ok := objectMembers(v)
	if !ok {
		return document{}, shapeError()
	}
	var doc document
	if items, ok := arrayItems(lookup(top, "items")); ok {
		doc.items = items
	}
	if version := lookup(top, "version"); version.Kind() == jsontext.KindNumber {
		if n, err := strconv.ParseFloat(string(version), 64); err != nil || n > 1 {
			doc.newer = string(version)
		}
	}
	return doc, nil
}

// Encrypts and atomically publishes the version 1 document holding items, refusing one a newer build wrote
func (s *Store) write(ctx context.Context, doc document, items []jsontext.Value) error {
	if doc.newer != "" {
		return failure(ErrNewerFormat, "vault.json.enc was written in format version %s by a newer icebath and was left untouched; "+
			"upgrade icebath to change its items", doc.newer)
	}
	if err := s.ensureDir(ctx); err != nil {
		return err
	}
	if items == nil {
		items = []jsontext.Value{}
	}
	plain, err := fileutil.EncodeJSON(struct {
		Version int              `json:"version"`
		Items   []jsontext.Value `json:"items"`
	}{1, items}, referenceStyle)
	if err != nil {
		return err
	}
	key, err := s.cipher(ctx)
	if err != nil {
		return err
	}
	blob, err := key.encrypt(plain, time.Now())
	if err != nil {
		return err
	}
	return s.writePrivate(ctx, s.data, blob)
}

// Fernet key the vault is encrypted under, created for a vault without ciphertext
func (s *Store) cipher(ctx context.Context) (fernetKey, error) {
	text, err := s.key(ctx)
	if err != nil {
		return fernetKey{}, err
	}
	return parseFernetKey(text)
}

// Error for a vault holding a document or record that is not an object
func shapeError() error { return failure(ErrCorrupt, "vault file is corrupted (unexpected shape)") }

// Members of a record, failing on a record that is not an object
func recordMembers(rec jsontext.Value) ([]member, error) {
	ms, ok := objectMembers(rec)
	if !ok {
		return nil, shapeError()
	}
	return ms, nil
}

// Secret members of a record, none when it has a falsy secret
func recordSecret(rec []member) ([]member, error) {
	secret := lookup(rec, "secret")
	if !truthy(secret) {
		return nil, nil
	}
	ms, ok := objectMembers(secret)
	if !ok {
		return nil, shapeError()
	}
	return ms, nil
}

// Reports whether a record carries id, failing on a record that is not an object
func recordHasID(rec jsontext.Value, id string) (bool, error) {
	ms, err := recordMembers(rec)
	if err != nil {
		return false, err
	}
	v := lookup(ms, "id")
	return v.Kind() == jsontext.KindString && valueText(v) == id, nil
}

// Metadata view of a record, secret values never copied
func metaOf(rec jsontext.Value) (ItemMeta, error) {
	ms, err := recordMembers(rec)
	if err != nil {
		return ItemMeta{}, err
	}
	field := func(name string) string { return valueText(lookup(ms, name)) }
	m := ItemMeta{ID: field("id"), Kind: field("kind"), Label: field("label"), Origin: field("origin"), CreatedAt: field("created_at")}
	if ident := lookup(ms, "identifier"); truthy(ident) {
		m.Identifier, m.IdentifierType = valueText(ident), field("identifier_type")
	}
	secret, err := recordSecret(ms)
	if err != nil {
		return ItemMeta{}, err
	}
	m.HasOTP = truthy(lookup(secret, "otp_secret"))
	return m, nil
}

// Adds an item, a login keeping its identifier as metadata and only its password and authenticator key secret
func (s *Store) Add(ctx context.Context, kind, label string, payload Payload, origin string) (ItemMeta, error) {
	rec, err := newRecord(kind, label, payload, origin)
	if err != nil {
		return ItemMeta{}, err
	}
	err = s.locked(ctx, func(ctx context.Context) error {
		doc, err := s.read(ctx)
		if err != nil {
			return err
		}
		return s.write(ctx, doc, append(slices.Clone(doc.items), rec))
	})
	if err != nil {
		return ItemMeta{}, err
	}
	return metaOf(rec)
}

// Validated record of a new item with a fresh handle and creation stamp
func newRecord(kind, label string, payload Payload, origin string) (jsontext.Value, error) {
	if !slices.Contains(Kinds(), kind) {
		return nil, failure(ErrInvalid, "unknown vault kind %s (expected one of ('login', 'payment', 'address'))", quote(kind))
	}
	if label = trimSpace(label); label == "" {
		return nil, failure(ErrInvalid, "label is required")
	}
	fields := payload.members()
	var normOrigin, identifier, identifierType string
	var secret []member
	var err error
	if kind == "login" {
		if origin == "" {
			return nil, failure(ErrInvalid, "origin is required for login items")
		}
		if normOrigin, err = NormalizeOrigin(origin); err != nil {
			return nil, err
		}
		var idType, ident jsontext.Value
		fields, idType = takeMember(fields, "identifier_type")
		if idType.Kind() != jsontext.KindString || !slices.Contains(IdentifierTypes(), valueText(idType)) {
			return nil, failure(ErrInvalid, "identifier_type must be one of ('email', 'phone', 'username')")
		}
		fields, ident = takeMember(fields, "identifier")
		identifier = trimSpace(truthyText(ident))
		password := lookup(fields, PasswordField)
		if identifier == "" || !truthy(password) {
			return nil, failure(ErrInvalid, "login items require identifier and password")
		}
		identifierType = valueText(idType)
		otp, err := normalizeOTPSecret(truthyText(lookup(fields, "otp_secret")))
		if err != nil {
			return nil, err
		}
		secret = []member{{PasswordField, password}}
		if otp != "" {
			secret = append(secret, member{"otp_secret", stringValue(otp)})
		}
	} else {
		allowed := KindFields(kind)
		for _, m := range fields {
			if !slices.ContainsFunc(allowed, func(f KindField) bool { return f.Name == m.name }) || trimSpace(truthyText(m.value)) == "" {
				continue
			}
			value := m.value
			if value.Kind() != jsontext.KindString {
				value = stringValue(valueText(value))
			}
			secret = setMember(secret, m.name, value)
		}
		var missing []string
		for _, f := range allowed {
			if f.Required && lookup(secret, f.Name) == nil {
				missing = append(missing, f.Name)
			}
		}
		if len(missing) > 0 {
			return nil, failure(ErrInvalid, "%s items require %s", kind, strings.Join(missing, ", "))
		}
		if origin != "" {
			if normOrigin, err = NormalizeOrigin(origin); err != nil {
				return nil, err
			}
		}
	}
	id := make([]byte, 6)
	if _, err := rand.Read(id); err != nil {
		return nil, err
	}
	return objectValue(
		member{"id", stringValue(localPrefix + hex.EncodeToString(id))},
		member{"kind", stringValue(kind)},
		member{"label", stringValue(label)},
		member{"origin", nullableValue(normOrigin)},
		member{"created_at", stringValue(isoStamp(time.Now()))},
		member{"identifier_type", nullableValue(identifierType)},
		member{"identifier", nullableValue(identifier)},
		member{"secret", objectValue(secret...)},
	), nil
}

// UTC ISO 8601 stamp with microseconds unless zero, ending in a plus zero offset
func isoStamp(t time.Time) string {
	t = t.UTC()
	stamp := t.Format("2006-01-02T15:04:05")
	if micro := t.Nanosecond() / 1000; micro != 0 {
		stamp += fmt.Sprintf(".%06d", micro)
	}
	return stamp + "+00:00"
}

// Metadata of every item, secret payloads never included
func (s *Store) list(ctx context.Context) ([]ItemMeta, error) {
	var out []ItemMeta
	err := s.locked(ctx, func(ctx context.Context) error {
		doc, err := s.read(ctx)
		if err != nil {
			return err
		}
		for _, rec := range doc.items {
			m, err := metaOf(rec)
			if err != nil {
				return err
			}
			out = append(out, m)
		}
		return nil
	})
	return out, err
}

// First record carrying id and whether one does
func (s *Store) find(ctx context.Context, id string) (jsontext.Value, bool, error) {
	var found jsontext.Value
	err := s.locked(ctx, func(ctx context.Context) error {
		doc, err := s.read(ctx)
		if err != nil {
			return err
		}
		for _, rec := range doc.items {
			match, err := recordHasID(rec, id)
			if err != nil {
				return err
			}
			if match {
				found = rec
				return nil
			}
		}
		return nil
	})
	return found, found != nil, err
}

// Metadata of the item carrying id and whether one does
func (s *Store) meta(ctx context.Context, id string) (ItemMeta, bool, error) {
	rec, ok, err := s.find(ctx, id)
	if err != nil || !ok {
		return ItemMeta{}, false, err
	}
	m, err := metaOf(rec)
	return m, err == nil, err
}

// Decrypted payload entries of the item carrying id, for server side fills only
func (s *Store) secret(ctx context.Context, id string) ([]member, error) {
	rec, ok, err := s.find(ctx, id)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, failure(ErrNoItem, "no vault item with id %s", quote(id))
	}
	ms, err := recordMembers(rec)
	if err != nil {
		return nil, err
	}
	return recordSecret(ms)
}

// Removes the item carrying id, false when no item does
func (s *Store) Remove(ctx context.Context, id string) (bool, error) {
	removed := false
	err := s.locked(ctx, func(ctx context.Context) error {
		doc, err := s.read(ctx)
		if err != nil {
			return err
		}
		remaining := make([]jsontext.Value, 0, len(doc.items))
		for _, rec := range doc.items {
			match, err := recordHasID(rec, id)
			if err != nil {
				return err
			}
			if !match {
				remaining = append(remaining, rec)
			}
		}
		if len(remaining) == len(doc.items) {
			return nil
		}
		removed = true
		return s.write(ctx, doc, remaining)
	})
	return removed, err
}
