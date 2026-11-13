package connections

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"

	"github.com/nickheyer/icebath/pkg/desktop/hardening"
)

// SecretStoragePolicyFile is the userData file holding the keychain encryption opt in
const SecretStoragePolicyFile = "secure-token-storage.json"

const safeStorageEncoding = "safeStorage"

// SafeStorage is the Electron safeStorage surface stored desktop secrets go through
type SafeStorage interface {
	IsEncryptionAvailable() bool
	EncryptString(plaintext string) ([]byte, error)
	DecryptString(ciphertext []byte) (string, error)
	SetUsePlainTextEncryption(use bool)
}

// SecretStoragePolicy is the opt in keychain encryption choice and whether the legacy pass ran
type SecretStoragePolicy struct {
	On       bool `json:"on"`
	Migrated bool `json:"migrated"`
}

// SecretStorageState is the answer of the secret storage get and set calls
type SecretStorageState struct {
	On bool `json:"on"`
}

// ReadSecretStoragePolicy parses the policy file, both flags off when it is missing or malformed
func ReadSecretStoragePolicy(readText func() (string, error)) SecretStoragePolicy {
	text, err := readText()
	if err != nil {
		return SecretStoragePolicy{}
	}
	var parsed any
	if json.Unmarshal([]byte(text), &parsed) != nil {
		return SecretStoragePolicy{}
	}
	if obj, ok := parsed.(map[string]any); ok {
		return SecretStoragePolicy{On: obj["on"] == true, Migrated: obj["migrated"] == true}
	}
	return SecretStoragePolicy{}
}

// WriteSecretStoragePolicy writes both flags as compact JSON
func WriteSecretStoragePolicy(policy SecretStoragePolicy, writeText func(string) error) error {
	text, err := jsonMarshal(SecretStoragePolicy{On: policy.On, Migrated: policy.Migrated})
	if err != nil {
		return err
	}
	return writeText(text)
}

// Encoding and value of a stored secret object, ok false for anything that is not a JSON object
func secretFields(secret any) (encoding any, value any, ok bool) {
	switch t := secret.(type) {
	case map[string]any:
		return t["encoding"], t["value"], true
	case SecretEnvelope:
		return t.Encoding, t.Value, true
	case *SecretEnvelope:
		if t == nil {
			return nil, nil, false
		}
		return t.Encoding, t.Value, true
	case []any:
		return nil, nil, true
	}
	return nil, nil, false
}

// ClassifyStoredSecret says whether a stored secret is kept, migrated to plain or read as absent under policy
func ClassifyStoredSecret(secret any, policy SecretStoragePolicy) string {
	encoding, _, ok := secretFields(secret)
	if !ok || encoding != safeStorageEncoding {
		return "keep"
	}
	if policy.On {
		return "keep"
	}
	if policy.Migrated {
		return "drop"
	}
	return "migrate"
}

// EncryptDesktopSecretStrict seals a secret through safeStorage, plain only when allowed and no keyring exists
func EncryptDesktopSecretStrict(value string, api SafeStorage, allowPlainText bool) (any, error) {
	if value == "" {
		return nil, nil
	}
	if !api.IsEncryptionAvailable() {
		if allowPlainText {
			return SecretEnvelope{Encoding: "plain", Value: value}, nil
		}
		return nil, errors.New("Secure token storage is unavailable (no OS keyring service was found), so Icebath Desktop cannot save remote gateway tokens. " +
			"Either enable an OS keyring (e.g. GNOME Keyring or KWallet providing org.freedesktop.secrets) and try again, " +
			"confirm the plain-text storage option when prompted in Settings → Gateway, " +
			"or set ICEBATH_DESKTOP_REMOTE_URL and ICEBATH_DESKTOP_REMOTE_TOKEN in your environment.")
	}
	sealed, err := api.EncryptString(value)
	if err != nil {
		detail := ""
		if err.Error() != "" {
			detail = " (" + err.Error() + ")"
		}
		return nil, errors.New("Failed to encrypt the remote gateway token for secure storage" + detail + ". " +
			"Set ICEBATH_DESKTOP_REMOTE_URL and ICEBATH_DESKTOP_REMOTE_TOKEN in your environment as a fallback.")
	}
	return SecretEnvelope{Encoding: safeStorageEncoding, Value: base64.StdEncoding.EncodeToString(sealed)}, nil
}

// EnableBasicPasswordStoreEncryption turns on v10 encryption for the keyring less Linux basic store
func EnableBasicPasswordStoreEncryption(platform, passwordStoreSwitch string, api SafeStorage) bool {
	if platform != "linux" || passwordStoreSwitch != "basic" {
		return false
	}
	api.SetUsePlainTextEncryption(true)
	return true
}

// Bytes of a base64 string decoded as leniently as Node Buffer.from does
func nodeBase64Decode(s string) []byte {
	var b strings.Builder
	for _, c := range s {
		switch {
		case c == '-':
			b.WriteByte('+')
		case c == '_':
			b.WriteByte('/')
		case c == '=':
			goto done
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '+', c == '/':
			b.WriteRune(c)
		}
	}
done:
	clean := b.String()
	if len(clean)%4 == 1 {
		clean = clean[:len(clean)-1]
	}
	out, _ := base64.RawStdEncoding.DecodeString(clean)
	return out
}

func (s *Service) secretStoragePolicyPath() string {
	return filepath.Join(s.userData, SecretStoragePolicyFile)
}

// Cached policy, read from userData on first use
func (s *Service) secretStoragePolicy() SecretStoragePolicy {
	if s.policy == nil {
		policy := ReadSecretStoragePolicy(func() (string, error) {
			data, err := os.ReadFile(s.secretStoragePolicyPath())
			return string(data), err
		})
		s.policy = &policy
	}
	return *s.policy
}

// Replaces the cached policy and writes it owner only
func (s *Service) setSecretStoragePolicy(next SecretStoragePolicy) error {
	policy := SecretStoragePolicy{On: next.On, Migrated: next.Migrated}
	s.policy = &policy
	return WriteSecretStoragePolicy(policy, func(text string) error {
		return hardening.WriteSecretFileAtomic(s.secretStoragePolicyPath(), []byte(text), hardening.SecretFileOptions{})
	})
}

// Plaintext of a stored secret, empty when absent, dropped by policy or undecryptable
func (s *Service) decryptDesktopSecret(secret any) string {
	encoding, raw, ok := secretFields(secret)
	if !ok {
		return ""
	}
	value := ""
	if jsTruthy(raw) {
		value = jsString(raw)
	}
	if value == "" {
		return ""
	}
	if encoding == safeStorageEncoding {
		if ClassifyStoredSecret(secret, s.secretStoragePolicy()) == "drop" {
			return ""
		}
		plaintext, err := s.safe.DecryptString(nodeBase64Decode(value))
		if err != nil {
			return ""
		}
		return plaintext
	}
	return value
}

// Stored secret as the envelope type the registry headers hold
func toEnvelope(secret any) SecretEnvelope {
	encoding, value, _ := secretFields(secret)
	out := SecretEnvelope{}
	if e, ok := encoding.(string); ok {
		out.Encoding = e
	}
	if v, ok := value.(string); ok {
		out.Value = v
	}
	return out
}

// Header map of a stored block as generic secrets, nil when the block has no header object
func blockHeaders(headers any) map[string]any {
	switch t := headers.(type) {
	case map[string]any:
		return t
	case map[string]SecretEnvelope:
		if t == nil {
			return nil
		}
		out := make(map[string]any, len(t))
		for k, v := range t {
			out[k] = v
		}
		return out
	}
	return nil
}

// Rewrites every stored desktop secret through reencode, true when any store was rewritten
func (s *Service) rewriteAllStoredSecrets(shouldRewrite func(any) bool, reencode func(any) (any, error)) (bool, error) {
	touched := false
	headersNeedRewrite := func(headers any) bool {
		for _, v := range blockHeaders(headers) {
			if shouldRewrite(v) {
				return true
			}
		}
		return false
	}
	blockNeedsRewrite := func(block any) bool {
		obj := jsObject(block)
		return shouldRewrite(obj["token"]) || headersNeedRewrite(obj["headers"])
	}
	rewriteBlock := func(block any) (any, error) {
		obj, ok := block.(map[string]any)
		if !ok {
			return block, nil
		}
		next := jsSpread(obj, nil)
		if jsTruthy(obj["token"]) {
			token, err := reencode(obj["token"])
			if err != nil {
				return nil, err
			}
			next["token"] = token
		}
		if headers := blockHeaders(obj["headers"]); headers != nil {
			out := make(map[string]any, len(headers))
			for k, v := range headers {
				value, err := reencode(v)
				if err != nil {
					return nil, err
				}
				out[k] = value
			}
			next["headers"] = out
		}
		return next, nil
	}

	config := s.readDesktopConnectionConfig()
	profilesNeedRewrite := false
	for _, v := range config.Profiles {
		if blockNeedsRewrite(v) {
			profilesNeedRewrite = true
		}
	}
	if blockNeedsRewrite(config.Remote) || profilesNeedRewrite {
		touched = true
		next := config.clone()
		remote, err := rewriteBlock(config.Remote)
		if err != nil {
			return touched, err
		}
		next.Remote = remote
		for k, v := range config.Profiles {
			profile, err := rewriteBlock(v)
			if err != nil {
				return touched, err
			}
			next.Profiles[k] = profile
		}
		if err := s.writeDesktopConnectionConfig(next); err != nil {
			return touched, err
		}
	}

	registry, err := s.readDesktopConnectionsRegistry()
	if err != nil {
		return touched, err
	}
	registryNeedsRewrite := false
	for _, c := range registry.Connections {
		if shouldRewrite(c.Token) || headersNeedRewrite(c.Headers) {
			registryNeedsRewrite = true
		}
	}
	if registryNeedsRewrite {
		touched = true
		next := registry.clone()
		for i, c := range next.Connections {
			if jsTruthy(c.Token) {
				token, err := reencode(c.Token)
				if err != nil {
					return touched, err
				}
				next.Connections[i].Token = token
			}
			if c.Headers != nil {
				headers := make(map[string]SecretEnvelope, len(c.Headers))
				for k, v := range c.Headers {
					value, err := reencode(v)
					if err != nil {
						return touched, err
					}
					headers[k] = toEnvelope(value)
				}
				next.Connections[i].Headers = headers
			}
		}
		if err := s.writeDesktopConnectionsRegistry(next); err != nil {
			return touched, err
		}
	}

	if s.rewriteNativeTokenStore(shouldRewrite, reencode) {
		touched = true
	}
	return touched, nil
}

// Rewrites the native OAuth token store blobs, swallowing a missing or corrupt store and failed writes
func (s *Service) rewriteNativeTokenStore(shouldRewrite func(any) bool, reencode func(any) (any, error)) bool {
	path := filepath.Join(s.userData, "native-oauth-tokens.json")
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	var parsed any
	if json.Unmarshal(data, &parsed) != nil {
		return false
	}
	store, ok := parsed.(map[string]any)
	if !ok {
		return false
	}
	needs := false
	for _, v := range store {
		if shouldRewrite(v) {
			needs = true
		}
	}
	if !needs {
		return false
	}
	out := make(map[string]any, len(store))
	for k, v := range store {
		value, err := reencode(v)
		if err != nil {
			return true
		}
		out[k] = value
	}
	text, err := jsonMarshal(out)
	if err != nil {
		return true
	}
	if os.MkdirAll(filepath.Dir(path), 0o777) == nil {
		_ = os.WriteFile(path, []byte(text), 0o600)
	}
	return true
}

// One shot pass rewriting legacy keychain blobs as plain so later launches never touch the keychain
func (s *Service) migrateLegacyEncryptedSecretsOnce() error {
	policy := s.secretStoragePolicy()
	if policy.On || policy.Migrated {
		return nil
	}
	needsMigration := func(secret any) bool { return ClassifyStoredSecret(secret, policy) == "migrate" }
	reencode := func(secret any) (any, error) {
		if !needsMigration(secret) {
			return secret, nil
		}
		if plaintext := s.decryptDesktopSecret(secret); plaintext != "" {
			return SecretEnvelope{Encoding: "plain", Value: plaintext}, nil
		}
		return secret, nil
	}
	touchedKeychain, err := s.rewriteAllStoredSecrets(needsMigration, reencode)
	if err != nil {
		touchedKeychain = false
		s.log("[secret-storage] legacy migration pass failed: " + err.Error())
	}
	if err := s.setSecretStoragePolicy(SecretStoragePolicy{On: false, Migrated: true}); err != nil {
		return err
	}
	if touchedKeychain {
		s.log("[secret-storage] migrated legacy keychain-encrypted secrets to opt-out storage (one-shot pass)")
	}
	return nil
}

// Flips keychain encryption and re-encodes every stored secret to match
func (s *Service) applySecretStorageEncryption(on bool) (SecretStorageState, error) {
	if s.secretStoragePolicy().On == on {
		return SecretStorageState{On: on}, nil
	}
	if on {
		needsEncrypt := func(secret any) bool {
			encoding, value, _ := secretFields(secret)
			return encoding == "plain" && jsTruthy(value)
		}
		if !s.safe.IsEncryptionAvailable() {
			return SecretStorageState{}, errors.New("OS keychain encryption is unavailable on this machine, so stored gateway secrets cannot be encrypted.")
		}
		if err := s.setSecretStoragePolicy(SecretStoragePolicy{On: true, Migrated: true}); err != nil {
			return SecretStorageState{}, err
		}
		_, err := s.rewriteAllStoredSecrets(needsEncrypt, func(secret any) (any, error) {
			if !needsEncrypt(secret) {
				return secret, nil
			}
			_, value, _ := secretFields(secret)
			return EncryptDesktopSecretStrict(jsString(value), s.safe, false)
		})
		if err != nil {
			if revertErr := s.setSecretStoragePolicy(SecretStoragePolicy{On: false, Migrated: true}); revertErr != nil {
				return SecretStorageState{}, revertErr
			}
			return SecretStorageState{}, err
		}
		return SecretStorageState{On: true}, nil
	}
	needsDecrypt := func(secret any) bool {
		encoding, _, _ := secretFields(secret)
		return encoding == safeStorageEncoding
	}
	_, err := s.rewriteAllStoredSecrets(needsDecrypt, func(secret any) (any, error) {
		if !needsDecrypt(secret) {
			return secret, nil
		}
		if plaintext := s.decryptDesktopSecret(secret); plaintext != "" {
			return SecretEnvelope{Encoding: "plain", Value: plaintext}, nil
		}
		return secret, nil
	})
	if err != nil {
		return SecretStorageState{}, err
	}
	if err := s.setSecretStoragePolicy(SecretStoragePolicy{On: false, Migrated: true}); err != nil {
		return SecretStorageState{}, err
	}
	return SecretStorageState{On: false}, nil
}

// Startup enables the basic store when launched with it and runs the legacy secret migration, before any window
func (s *Service) Startup() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	EnableBasicPasswordStoreEncryption(s.platform, s.passwordStoreSwitch, s.safe)
	return s.migrateLegacyEncryptedSecretsOnce()
}

// GetSecretStorageEncryption answers the current policy without touching the keychain
func (s *Service) GetSecretStorageEncryption() SecretStorageState {
	s.mu.Lock()
	defer s.mu.Unlock()
	return SecretStorageState{On: s.secretStoragePolicy().On}
}

// SetSecretStorageEncryption flips the policy, only a literal true turns it on
func (s *Service) SetSecretStorageEncryption(on any) (SecretStorageState, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.applySecretStorageEncryption(on == true)
}
