package vault

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base32"
	"encoding/binary"
	"errors"
	"fmt"
	"hash"
	"io/fs"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/google/uuid"
	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pydatetime"
	"github.com/nickheyer/icebath/pkg/internal/pyfernet"
	"github.com/nickheyer/icebath/pkg/internal/pyfs"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pyurl"
)

var VaultKinds = []string{"login", "payment", "address"}

var LoginIdentifierTypes = []string{"email", "phone", "username"}

// Payment payload names mapped to the WHATWG autocomplete token the fill targets
var PaymentFields = pyjson.ObjectOf("card_number", "cc-number", "cardholder_name", "cc-name", "exp_month", "cc-exp-month",
	"exp_year", "cc-exp-year", "cvc", "cc-csc", "billing_postal_code", "postal-code")

// Address payload names mapped to the WHATWG autocomplete token the fill targets
var AddressFields = pyjson.ObjectOf("address_line1", "address-line1", "address_line2", "address-line2", "city", "address-level2",
	"state", "address-level1", "postal_code", "postal-code", "country", "country-name")

var vaultRequiredFields = map[string][]string{"payment": {"card_number", "exp_month", "exp_year", "cvc"},
	"address": {"address_line1", "city", "postal_code", "country"}}

var vaultDefaultPorts = map[string]int{"http": 80, "https": 443}

var vaultLock sync.Mutex

// Vault failure that is safe to surface, never carrying a secret value
type VaultError struct{ Msg string }

func (e *VaultError) Error() string { return e.Msg }

// Where a new vault key goes, file by default or keychain, from the home's vault.key_storage
func configuredKeyStorage(ctx context.Context, h string) string {
	tree, _ := config.LoadReadonly(ctx, h, nil)
	var value any
	switch section := tree.Get("vault").(type) {
	case map[string]any:
		value = section["key_storage"]
	case config.Tree:
		value = section["key_storage"]
	}
	if !pyrepr.Truthy(value) {
		value = "file"
	}
	return strings.ToLower(strings.TrimSpace(pyrepr.Str(value)))
}

var otpAlgos = map[string]func() hash.Hash{"SHA1": sha1.New, "SHA256": sha256.New, "SHA512": sha512.New}

var (
	otpStripRE   = regexp.MustCompile(`[\s-]`)
	otpBadSeedRE = regexp.MustCompile(`[^A-Z2-7]`)
)

// Canonical stored seed of a base32 secret or otpauth totp URI, non default parameters appended
func NormalizeOTPSecret(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", nil
	}
	digits, period, algo := 6, 30, "SHA1"
	if strings.HasPrefix(strings.ToLower(value), "otpauth://") {
		parsed, err := pyurl.Parse(value)
		if err != nil {
			return "", &VaultError{"authenticator key must be a base32 secret or an otpauth:// URI"}
		}
		if strings.ToLower(parsed.Netloc) != "totp" {
			return "", &VaultError{"only otpauth://totp links are supported (counter-based HOTP is not)"}
		}
		first := pyjson.NewObject()
		for _, kv := range pyurl.ParseQSL(parsed.Query, false) {
			if _, seen := first.Get(kv[0]); !seen {
				first.Set(kv[0], kv[1])
			}
		}
		qs := map[string]string{}
		for _, k := range first.Keys {
			v, _ := first.Get(k)
			qs[strings.ToLower(k)] = v.(string)
		}
		value = qs["secret"]
		parseInt := func(key string, def int) (int, bool) {
			raw, ok := qs[key]
			if !ok {
				return def, true
			}
			n, ok := pyint.Parse(raw)
			if !ok || !n.IsInt64() {
				return 0, false
			}
			return int(n.Int64()), true
		}
		var okD, okP bool
		if digits, okD = parseInt("digits", digits); okD {
			period, okP = parseInt("period", period)
		}
		if !okD || !okP {
			return "", &VaultError{"otpauth:// digits/period must be integers"}
		}
		if a, ok := qs["algorithm"]; ok {
			algo = a
		}
		algo = strings.ReplaceAll(strings.ToUpper(algo), "-", "")
		if _, known := otpAlgos[algo]; digits < 6 || digits > 8 || period <= 0 || !known {
			return "", &VaultError{"unsupported otpauth:// parameters (digits 6-8, period > 0, SHA1/SHA256/SHA512)"}
		}
	}
	seed := strings.TrimRight(strings.ToUpper(otpStripRE.ReplaceAllString(value, "")), "=")
	if seed == "" || otpBadSeedRE.MatchString(seed) {
		return "", &VaultError{"authenticator key must be a base32 secret or an otpauth:// URI"}
	}
	if digits == 6 && period == 30 && algo == "SHA1" {
		return seed, nil
	}
	return fmt.Sprintf("%s|%d|%d|%s", seed, digits, period, algo), nil
}

// RFC 6238 code for a stored seed at a unix time, the seed form NormalizeOTPSecret produces
func TOTPAt(seed string, digits, period int, at float64) (string, error) {
	newHash := sha1.New
	if strings.Contains(seed, "|") {
		parts := strings.SplitN(seed, "|", 4)
		if len(parts) != 4 {
			return "", fmt.Errorf("ValueError: not enough values to unpack (expected 4, got %d)", len(parts))
		}
		d, okD := pyint.Parse(parts[1])
		p, okP := pyint.Parse(parts[2])
		if !okD || !okP {
			return "", errors.New("ValueError: invalid literal for int()")
		}
		seed, digits, period = parts[0], int(d.Int64()), int(p.Int64())
		if h, ok := otpAlgos[strings.ToUpper(parts[3])]; ok {
			newHash = h
		}
	}
	padded := strings.ToUpper(seed) + strings.Repeat("=", (8-len(seed)%8)%8)
	key, err := base32.StdEncoding.DecodeString(padded)
	if err != nil {
		return "", fmt.Errorf("binascii.Error: %v", err)
	}
	counter := uint64(int64(math.Floor(at / float64(period))))
	mac := hmac.New(newHash, key)
	mac.Write(binary.BigEndian.AppendUint64(nil, counter))
	digest := mac.Sum(nil)
	offset := digest[len(digest)-1] & 0x0f
	code := binary.BigEndian.Uint32(digest[offset:offset+4]) & 0x7fffffff
	mod := uint32(1)
	for i := 0; i < digits; i++ {
		mod *= 10
	}
	return fmt.Sprintf("%0*d", digits, code%mod), nil
}

// RFC 6238 code for a stored seed now
func TOTPNow(seed string) (string, error) {
	return TOTPAt(seed, 6, 30, float64(time.Now().UnixNano())/1e9)
}

// Scheme, host and non default port of a URL or origin, a VaultError without scheme and host
func NormalizeOrigin(urlOrOrigin string) (string, error) {
	value := strings.TrimSpace(urlOrOrigin)
	if value == "" {
		return "", &VaultError{"origin is required"}
	}
	if !strings.Contains(value, "://") {
		return "", &VaultError{fmt.Sprintf("origin must include a scheme (got %s)", pyrepr.Repr(value))}
	}
	parts, err := pyurl.Split(value)
	if err != nil {
		return "", &VaultError{fmt.Sprintf("could not parse origin from %s", pyrepr.Repr(value))}
	}
	scheme := strings.ToLower(parts.Scheme)
	host := strings.ToLower(parts.Hostname())
	if scheme == "" || host == "" {
		return "", &VaultError{fmt.Sprintf("could not parse origin from %s", pyrepr.Repr(value))}
	}
	port, hasPort, err := parts.Port()
	if err != nil {
		return "", &VaultError{fmt.Sprintf("invalid port in origin %s", pyrepr.Repr(value))}
	}
	if def, ok := vaultDefaultPorts[scheme]; !hasPort || (ok && port == def) {
		return scheme + "://" + host, nil
	}
	return fmt.Sprintf("%s://%s:%d", scheme, host, port), nil
}

// Metadata only view of a vault item, never a secret value
type VaultItemMeta struct {
	ID             string
	Kind           string
	Label          string
	Origin         any
	CreatedAt      string
	IdentifierType any
	Identifier     any
	HasOTP         bool
	AllowedOrigins []string
}

func (m VaultItemMeta) ToDict() *pyjson.Object {
	out := pyjson.ObjectOf("id", m.ID, "kind", m.Kind, "label", m.Label, "origin", m.Origin, "created_at", m.CreatedAt)
	if m.Identifier != nil {
		out.Set("identifier", m.Identifier)
		out.Set("identifier_type", m.IdentifierType)
	}
	if m.HasOTP {
		out.Set("has_otp", true)
	}
	if len(m.AllowedOrigins) > 1 {
		list := make([]any, len(m.AllowedOrigins))
		for i, o := range m.AllowedOrigins {
			list[i] = o
		}
		out.Set("allowed_origins", list)
	}
	return out
}

// Encrypted profile scoped vault under the home's vault directory
type VaultStore struct {
	ctx        context.Context
	home       string
	base       string
	vaultPath  string
	keyPath    string
	keyrefPath string
}

// Store at baseDir, empty meaning the vault directory of the home in effect for ctx
func NewVaultStore(ctx context.Context, baseDir string) *VaultStore {
	h := home.Dir(ctx)
	if baseDir == "" {
		baseDir = filepath.Join(h, "vault")
	}
	return &VaultStore{ctx: ctx, home: h, base: baseDir, vaultPath: filepath.Join(baseDir, "vault.json.enc"),
		keyPath: filepath.Join(baseDir, "vault.key"), keyrefPath: filepath.Join(baseDir, "vault.keyref")}
}

func (s *VaultStore) ensureDir() error {
	if _, err := home.MakeUserDir(s.ctx, s.base, 0o700, true, true); err != nil {
		return err
	}
	config.SecureDir(s.home, s.base)
	return nil
}

// Serializes read modify write cycles across goroutines and processes through the lock file
func (s *VaultStore) locked(fn func() error) error {
	vaultLock.Lock()
	defer vaultLock.Unlock()
	if err := s.ensureDir(); err != nil {
		return err
	}
	lockPath := filepath.Join(s.base, ".vault.lock")
	if runtime.GOOS == "windows" {
		if st, err := os.Stat(lockPath); err != nil || st.Size() == 0 {
			os.WriteFile(lockPath, []byte(" "), 0o600)
		}
	}
	fd, err := os.OpenFile(lockPath, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return err
	}
	defer fd.Close()
	if err := pyfs.LockFile(fd); err != nil {
		return err
	}
	defer pyfs.UnlockFile(fd)
	return fn()
}

func (s *VaultStore) fernet() (*pyfernet.Fernet, error) {
	key, err := s.key()
	if err != nil {
		return nil, err
	}
	return pyfernet.New(key)
}

func (s *VaultStore) hasCiphertext() bool {
	st, err := os.Stat(s.vaultPath)
	return err == nil && st.Size() > 0
}

func pathExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func readKeyFile(path string) ([]byte, error) {
	raw, err := os.ReadFile(path)
	return bytes.TrimSpace(raw), err
}

// Key file when present, else the keychain entry the pointer names, else a new key for an empty vault
func (s *VaultStore) key() ([]byte, error) {
	if err := s.ensureDir(); err != nil {
		return nil, err
	}
	if pathExists(s.keyPath) {
		os.Chmod(s.keyPath, 0o600)
		return readKeyFile(s.keyPath)
	}
	ref, err := vaultReadRef(s.keyrefPath)
	if err != nil {
		return nil, err
	}
	var why string
	if ref != nil {
		var key []byte
		adopted, aerr := s.adopt(ref)
		if aerr == nil {
			ref = adopted
			key, aerr = vaultLoadKey(ref)
		}
		var ku *KeychainUnavailable
		if errors.As(aerr, &ku) {
			why = "the OS keychain is not reachable here: " + ku.Msg
		} else if aerr != nil {
			return nil, aerr
		} else {
			why = fmt.Sprintf("the keychain entry %s / %s is gone", pyrepr.Repr(refString(ref, "service")), pyrepr.Repr(refString(ref, "account")))
		}
		if len(key) > 0 {
			return key, nil
		}
	} else {
		why = filepath.Base(s.keyPath) + " is missing"
	}
	if s.hasCiphertext() {
		return nil, &VaultError{s.missingKeyMessage(why)}
	}
	return s.createKey()
}

func (s *VaultStore) missingKeyMessage(why string) string {
	return fmt.Sprintf("The vault key could not be found (%s). vault.json.enc in %s was left untouched. ", why, s.base) +
		"If this profile came from another machine, run `icebath vault key migrate --to file` there and " +
		"copy its vault.key next to vault.json.enc; if the keychain entry was deleted, restore it from a " +
		"keychain backup. `icebath vault key` shows where the key is expected. To start over instead, " +
		"delete vault.json.enc (its saved items are lost)."
}

func (s *VaultStore) conflictingKeyMessage() string {
	return s.keyPath + " does not decrypt vault.json.enc, so the key was not moved and nothing " +
		"was deleted. If the vault's key is in the OS keychain, move this vault.key aside and run " +
		"`icebath vault key` again."
}

// New key in the keychain when configured and reachable, else in the 0600 key file
func (s *VaultStore) createKey() ([]byte, error) {
	key := pyfernet.GenerateKey()
	if configuredKeyStorage(s.ctx, s.home) == "keychain" {
		ref := vaultNewRef(s.base)
		if err := vaultStoreKey(ref, key); err != nil {
			slog.Warn(fmt.Sprintf("vault.key_storage is keychain, but %s; using %s instead", err, s.keyPath))
		} else {
			if err := vaultSaveNewRef(s.ctx, s.keyrefPath, ref); err != nil {
				return nil, err
			}
			return key, nil
		}
	}
	if err := vaultRemoveFile(s.keyrefPath); err != nil {
		return nil, err
	}
	if err := s.writeKeyFile(key); err != nil {
		return nil, err
	}
	return key, nil
}

func (s *VaultStore) writeKeyFile(key []byte) error {
	mode := os.FileMode(0o600)
	return pyfs.AtomicWriteBytes(s.ctx, s.keyPath, key, "", &mode, true)
}

// Gives a cloned vault its own keychain entry before publication at publishedBase, nil when nothing is shared
func (s *VaultStore) OwnCopiedKey(publishedBase string) (*pyjson.Object, error) {
	ref, err := vaultReadRef(s.keyrefPath)
	if err != nil {
		return nil, err
	}
	if ref == nil || pathExists(s.keyPath) {
		return nil, nil
	}
	key, err := vaultLoadKey(ref)
	var mine *pyjson.Object
	if err == nil {
		if !s.decrypts(key) {
			return nil, nil
		}
		mine = vaultNewRef(publishedBase)
		err = vaultStoreKey(mine, key)
	}
	var ku *KeychainUnavailable
	if errors.As(err, &ku) {
		return nil, &VaultError{fmt.Sprintf("its key is in the OS keychain, which this session cannot use (%s). Clone from a session "+
			"that can reach the keychain, or run `icebath vault key migrate --to file` on the source first.", ku.Msg)}
	}
	if err != nil {
		return nil, err
	}
	if err := vaultWriteRef(s.ctx, s.keyrefPath, mine); err != nil {
		vaultDeleteKey(mine)
		return nil, err
	}
	return mine, nil
}

// Own entry for a moved vault, never deleting the entry it came from, the shared one serving on failure
func (s *VaultStore) adopt(ref *pyjson.Object) (*pyjson.Object, error) {
	if vaultOwnsRef(ref, s.base) {
		return ref, nil
	}
	key, err := vaultLoadKey(ref)
	if err != nil {
		return nil, err
	}
	if !s.decrypts(key) {
		return ref, nil
	}
	mine := vaultNewRef(s.base)
	err = vaultStoreKey(mine, key)
	if err == nil {
		err = vaultWriteRef(s.ctx, s.keyrefPath, mine)
	}
	if err == nil {
		return mine, nil
	}
	if saved, rerr := vaultReadRef(s.keyrefPath); rerr == nil && saved != nil && refString(saved, "account") == refString(mine, "account") {
		return mine, nil
	}
	vaultDeleteKey(mine)
	slog.Warn(fmt.Sprintf("could not give the copied vault in %s its own keychain entry", s.base), "error", err)
	return ref, nil
}

func (s *VaultStore) decrypts(key []byte) bool {
	if len(key) == 0 {
		return false
	}
	if !s.hasCiphertext() {
		return true
	}
	f, err := pyfernet.New(key)
	if err != nil {
		return false
	}
	blob, err := os.ReadFile(s.vaultPath)
	if err != nil {
		return false
	}
	_, err = f.Decrypt(blob)
	return err == nil
}

// Where this vault's key lives, never the key itself
func (s *VaultStore) KeyLocation() (*pyjson.Object, error) {
	ref, err := vaultReadRef(s.keyrefPath)
	if err != nil {
		return nil, err
	}
	if pathExists(s.keyPath) {
		where := pyjson.ObjectOf("storage", "file", "path", s.keyPath)
		if ref != nil {
			acct, _ := ref.Get("account")
			where.Set("keychain_entry_left", acct)
		}
		return where, nil
	}
	if ref != nil {
		return pyjson.Clone(ref).(*pyjson.Object), nil
	}
	return pyjson.ObjectOf("storage", "none", "path", s.keyPath), nil
}

// Moves the key between the file and the OS keychain, proving the destination before removing the source
func (s *VaultStore) MigrateKey(target string) (string, error) {
	if target != "file" && target != "keychain" {
		return "", &VaultError{"key storage must be 'file' or 'keychain'"}
	}
	var out string
	err := s.locked(func() error {
		if err := s.ensureDir(); err != nil {
			return err
		}
		var err error
		if target == "keychain" {
			out, err = s.migrateToKeychain()
		} else {
			out, err = s.migrateToFile()
		}
		return err
	})
	return out, err
}

func (s *VaultStore) migrateToKeychain() (string, error) {
	existing, err := vaultReadRef(s.keyrefPath)
	if err != nil {
		return "", err
	}
	if !pathExists(s.keyPath) {
		if existing != nil {
			adopted, err := s.adopt(existing)
			var key []byte
			if err == nil {
				key, err = vaultLoadKey(adopted)
			}
			var ku *KeychainUnavailable
			if errors.As(err, &ku) {
				return "", &VaultError{"cannot read the vault key from the OS keychain: " + ku.Msg}
			} else if err != nil {
				return "", err
			}
			if !s.decrypts(key) {
				return "", &VaultError{s.missingKeyMessage(fmt.Sprintf("the keychain entry %s does not open the vault", pyrepr.Repr(refString(existing, "account"))))}
			}
			return "keychain", nil
		}
		if s.hasCiphertext() {
			return "", &VaultError{s.missingKeyMessage(filepath.Base(s.keyPath) + " is missing")}
		}
		if _, err := osKeyring(); err != nil {
			return "", &VaultError{"cannot use the OS keychain: " + err.Error()}
		}
		return "keychain", nil
	}
	key, err := readKeyFile(s.keyPath)
	if err != nil {
		return "", err
	}
	if !s.decrypts(key) {
		return "", &VaultError{s.conflictingKeyMessage()}
	}
	ref := existing
	fresh := existing == nil || !vaultOwnsRef(existing, s.base)
	if fresh {
		ref = vaultNewRef(s.base)
	}
	err = vaultStoreKey(ref, key)
	if err == nil {
		var back []byte
		if back, err = vaultLoadKey(ref); err == nil && !s.decrypts(back) {
			err = &KeychainUnavailable{"the key read back from the keychain does not decrypt the vault"}
		}
	}
	if err != nil {
		if fresh {
			vaultDeleteKey(ref)
		}
		return "", &VaultError{fmt.Sprintf("could not move the vault key to the OS keychain (%s); it stays in %s", err, s.keyPath)}
	}
	if fresh {
		if err := vaultSaveNewRef(s.ctx, s.keyrefPath, ref); err != nil {
			return "", &VaultError{fmt.Sprintf("could not save %s (%s); the key stays in %s", filepath.Base(s.keyrefPath), pyOSErrorStr(err), s.keyPath)}
		}
	}
	if err := vaultRemoveFile(s.keyPath); err != nil {
		return "", err
	}
	return "keychain", nil
}

// Python str of an OSError, errno and strerror in brackets when the error carries them
func pyOSErrorStr(err error) string {
	var errno syscall.Errno
	if !errors.As(err, &errno) {
		return err.Error()
	}
	msg := errno.Error()
	msg = fmt.Sprintf("[Errno %d] %s%s", int(errno), strings.ToUpper(msg[:1]), msg[1:])
	var pe *fs.PathError
	if errors.As(err, &pe) {
		msg += ": " + pyrepr.Repr(pe.Path)
	}
	return msg
}

func (s *VaultStore) migrateToFile() (string, error) {
	ref, err := vaultReadRef(s.keyrefPath)
	if err != nil {
		return "", err
	}
	if pathExists(s.keyPath) {
		if ref != nil {
			key, err := readKeyFile(s.keyPath)
			if err != nil {
				return "", err
			}
			if !s.decrypts(key) {
				return "", &VaultError{s.conflictingKeyMessage()}
			}
			if err := s.releaseRef(ref); err != nil {
				return "", err
			}
		}
		return "file", nil
	}
	if ref == nil {
		if s.hasCiphertext() {
			return "", &VaultError{s.missingKeyMessage(filepath.Base(s.keyPath) + " is missing")}
		}
		return "file", nil
	}
	key, err := vaultLoadKey(ref)
	if err != nil {
		return "", &VaultError{"cannot read the vault key from the OS keychain: " + err.Error()}
	}
	if len(key) == 0 || !s.decrypts(key) {
		return "", &VaultError{s.missingKeyMessage(fmt.Sprintf("the keychain entry %s does not hold this vault's key", pyrepr.Repr(refString(ref, "account"))))}
	}
	if err := s.writeKeyFile(key); err != nil {
		return "", err
	}
	if back, err := readKeyFile(s.keyPath); err != nil || !bytes.Equal(back, key) {
		vaultRemoveFile(s.keyPath)
		return "", &VaultError{fmt.Sprintf("could not write %s; the key stays in the OS keychain", s.keyPath)}
	}
	if err := s.releaseRef(ref); err != nil {
		return "", err
	}
	return "file", nil
}

// Drops the keychain copy once the file holds the key, keeping the pointer when our delete fails
func (s *VaultStore) releaseRef(ref *pyjson.Object) error {
	if vaultOwnsRef(ref, s.base) && !vaultDeleteKey(ref) {
		return nil
	}
	return vaultRemoveFile(s.keyrefPath)
}

func (s *VaultStore) readAll() ([]any, error) {
	blob, err := os.ReadFile(s.vaultPath)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if len(blob) == 0 {
		return nil, nil
	}
	f, err := s.fernet()
	if err != nil {
		return nil, err
	}
	raw, err := f.Decrypt(blob)
	if err != nil {
		return nil, &VaultError{"vault file could not be decrypted (key mismatch or corruption)"}
	}
	text, err := pyjson.DecodeBytes(raw)
	var data any
	if err == nil {
		data, err = pyjson.Loads(text)
	}
	if err != nil {
		return nil, &VaultError{"vault file is corrupted (invalid JSON)"}
	}
	obj, ok := data.(*pyjson.Object)
	if !ok {
		return nil, &VaultError{"vault file is corrupted (unexpected shape)"}
	}
	items, present := obj.Get("items")
	if !present {
		return nil, nil
	}
	list, _ := items.([]any)
	return list, nil
}

func (s *VaultStore) writeAll(items []any) error {
	if err := s.ensureDir(); err != nil {
		return err
	}
	if items == nil {
		items = []any{}
	}
	payload, err := pyjson.Dumps(pyjson.ObjectOf("version", 1, "items", items))
	if err != nil {
		return err
	}
	f, err := s.fernet()
	if err != nil {
		return err
	}
	mode := os.FileMode(0o600)
	return pyfs.AtomicWriteBytes(s.ctx, s.vaultPath, f.Encrypt([]byte(payload)), "", &mode, true)
}

func contains(list []string, v any) bool {
	s, ok := v.(string)
	if !ok {
		return false
	}
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func pyTuple(list []string) string {
	parts := make([]string, len(list))
	for i, s := range list {
		parts[i] = pyrepr.Repr(s)
	}
	return "(" + strings.Join(parts, ", ") + ")"
}

// Adds an item, a login keeping its identifier in metadata and only the password and seed secret
func (s *VaultStore) AddItem(kind, label string, secret *pyjson.Object, origin string) (VaultItemMeta, error) {
	if !contains(VaultKinds, kind) {
		return VaultItemMeta{}, &VaultError{fmt.Sprintf("unknown vault kind %s (expected one of %s)", pyrepr.Repr(kind), pyTuple(VaultKinds))}
	}
	label = strings.TrimSpace(label)
	if label == "" {
		return VaultItemMeta{}, &VaultError{"label is required"}
	}
	var normOrigin, identifier, identifierType any
	secret = pyjson.Clone(secret).(*pyjson.Object)
	if kind == "login" {
		if origin == "" {
			return VaultItemMeta{}, &VaultError{"origin is required for login items"}
		}
		o, err := NormalizeOrigin(origin)
		if err != nil {
			return VaultItemMeta{}, err
		}
		normOrigin = o
		idType, _ := secret.Get("identifier_type")
		secret.Delete("identifier_type")
		if !contains(LoginIdentifierTypes, idType) {
			return VaultItemMeta{}, &VaultError{"identifier_type must be one of " + pyTuple(LoginIdentifierTypes)}
		}
		rawID, _ := secret.Get("identifier")
		secret.Delete("identifier")
		if !pyrepr.Truthy(rawID) {
			rawID = ""
		}
		ident := strings.TrimSpace(pyrepr.Str(rawID))
		password, _ := secret.Get("password")
		if ident == "" || !pyrepr.Truthy(password) {
			return VaultItemMeta{}, &VaultError{"login items require identifier and password"}
		}
		identifier, identifierType = ident, idType.(string)
		rawOTP, _ := secret.Get("otp_secret")
		if !pyrepr.Truthy(rawOTP) {
			rawOTP = ""
		}
		otp, err := NormalizeOTPSecret(pyrepr.Str(rawOTP))
		if err != nil {
			return VaultItemMeta{}, err
		}
		secret = pyjson.ObjectOf("password", password)
		if otp != "" {
			secret.Set("otp_secret", otp)
		}
	} else {
		allowed := PaymentFields
		if kind == "address" {
			allowed = AddressFields
		}
		kept := pyjson.NewObject()
		for _, k := range secret.Keys {
			v, _ := secret.Get(k)
			if _, ok := allowed.Get(k); !ok {
				continue
			}
			probe := v
			if !pyrepr.Truthy(probe) {
				probe = ""
			}
			if strings.TrimSpace(pyrepr.Str(probe)) != "" {
				kept.Set(k, pyrepr.Str(v))
			}
		}
		secret = kept
		var missing []string
		for _, f := range vaultRequiredFields[kind] {
			if _, ok := secret.Get(f); !ok {
				missing = append(missing, f)
			}
		}
		if len(missing) > 0 {
			return VaultItemMeta{}, &VaultError{fmt.Sprintf("%s items require %s", kind, strings.Join(missing, ", "))}
		}
		if origin != "" {
			o, err := NormalizeOrigin(origin)
			if err != nil {
				return VaultItemMeta{}, err
			}
			normOrigin = o
		}
	}
	itemID := "vault_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:12]
	record := pyjson.ObjectOf("id", itemID, "kind", kind, "label", label, "origin", normOrigin,
		"created_at", pydatetime.ISOFormatUTC(time.Now()), "identifier_type", identifierType, "identifier", identifier,
		"secret", secret)
	err := s.locked(func() error {
		items, err := s.readAll()
		if err != nil {
			return err
		}
		return s.writeAll(append(items, record))
	})
	if err != nil {
		return VaultItemMeta{}, err
	}
	return vaultMeta(record), nil
}

// Metadata only listing, secret payloads never included
func (s *VaultStore) ListItems() ([]VaultItemMeta, error) {
	var out []VaultItemMeta
	err := s.locked(func() error {
		items, err := s.readAll()
		for _, rec := range items {
			o, rerr := recObject(rec)
			if rerr != nil {
				return rerr
			}
			out = append(out, vaultMeta(o))
		}
		return err
	})
	return out, err
}

func (s *VaultStore) HasItems() bool {
	has := false
	err := s.locked(func() error {
		items, err := s.readAll()
		has = len(items) > 0
		return err
	})
	return err == nil && has
}

// Record as a dict, the AttributeError Python raises calling get on anything else
func recObject(rec any) (*pyjson.Object, error) {
	if o, ok := rec.(*pyjson.Object); ok {
		return o, nil
	}
	return nil, fmt.Errorf("AttributeError: '%s' object has no attribute 'get'", pyrepr.TypeName(rec))
}

func recID(rec any) (any, error) {
	o, err := recObject(rec)
	if err != nil {
		return nil, err
	}
	v, _ := o.Get("id")
	return v, nil
}

func (s *VaultStore) RemoveItem(itemID string) (bool, error) {
	removed := false
	err := s.locked(func() error {
		items, err := s.readAll()
		if err != nil {
			return err
		}
		remaining := []any{}
		for _, rec := range items {
			id, err := recID(rec)
			if err != nil {
				return err
			}
			if id != itemID {
				remaining = append(remaining, rec)
			}
		}
		if len(remaining) == len(items) {
			return nil
		}
		removed = true
		return s.writeAll(remaining)
	})
	return removed, err
}

func (s *VaultStore) GetMeta(itemID string) (*VaultItemMeta, error) {
	var found *VaultItemMeta
	err := s.locked(func() error {
		items, err := s.readAll()
		for _, rec := range items {
			id, rerr := recID(rec)
			if rerr != nil {
				return rerr
			}
			if id == itemID {
				m := vaultMeta(rec.(*pyjson.Object))
				found = &m
				break
			}
		}
		return err
	})
	return found, err
}

// Decrypted secret payload for server side use only, never for results, logs or errors
func (s *VaultStore) ResolveSecret(itemID string) (*pyjson.Object, error) {
	var found *pyjson.Object
	err := s.locked(func() error {
		items, err := s.readAll()
		for _, rec := range items {
			id, rerr := recID(rec)
			if rerr != nil {
				return rerr
			}
			if id == itemID {
				sec, _ := rec.(*pyjson.Object).Get("secret")
				if o, ok := sec.(*pyjson.Object); ok {
					found = pyjson.Clone(o).(*pyjson.Object)
				} else {
					found = pyjson.NewObject()
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
		return nil, &VaultError{fmt.Sprintf("no vault item with id %s", pyrepr.Repr(itemID))}
	}
	return found, nil
}

func vaultMeta(rec *pyjson.Object) VaultItemMeta {
	get := func(k string) any { v, _ := rec.Get(k); return v }
	str := func(k string) string {
		if v, ok := rec.Get(k); ok {
			return pyrepr.Str(v)
		}
		return ""
	}
	m := VaultItemMeta{ID: str("id"), Kind: str("kind"), Label: str("label"), Origin: get("origin"), CreatedAt: str("created_at")}
	if ident := get("identifier"); pyrepr.Truthy(ident) {
		m.Identifier, m.IdentifierType = ident, get("identifier_type")
	}
	if sec, ok := get("secret").(*pyjson.Object); ok {
		otp, _ := sec.Get("otp_secret")
		m.HasOTP = pyrepr.Truthy(otp)
	}
	return m
}

// Default store of the home in effect for ctx
func GetVaultStore(ctx context.Context) *VaultStore { return NewVaultStore(ctx, "") }

var passwordEchoRE = regexp.MustCompile(`(password['"]?\s*[:=]\s*)\S+`)

// Strips every secret value of at least three characters and password echoes from text
func ScrubSecretFromText(text string, secret *pyjson.Object) string {
	scrubbed := text
	for _, k := range secret.Keys {
		v, _ := secret.Get(k)
		if s, ok := v.(string); ok && len([]rune(s)) >= 3 && strings.Contains(scrubbed, s) {
			scrubbed = strings.ReplaceAll(scrubbed, s, "[REDACTED]")
		}
	}
	return passwordEchoRE.ReplaceAllString(scrubbed, "${1}[REDACTED]")
}

// ConfiguredKeyStorage is the vault.key_storage setting of a home, file when unset
func ConfiguredKeyStorage(ctx context.Context, h string) string { return configuredKeyStorage(ctx, h) }
