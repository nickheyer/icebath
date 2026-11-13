package connections

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
)

// NativeTokenStoreIO is the encryption and store file access the native token store runs over
type NativeTokenStoreIO struct {
	Encrypt        func(plaintext string) (any, error)
	Decrypt        func(secret any) string
	ReadStoreText  func() (string, error)
	WriteStoreText func(text string) error
	RememberLog    func(message string)
}

func (io NativeTokenStoreIO) log(message string) {
	if io.RememberLog != nil {
		io.RememberLog(message)
	}
}

// Stored blobs keyed by gateway URL, empty when the file is missing, unparseable or not an object
func readNativeTokenStore(io NativeTokenStoreIO) map[string]any {
	text, err := io.ReadStoreText()
	if err != nil {
		return map[string]any{}
	}
	var parsed any
	if json.Unmarshal([]byte(text), &parsed) != nil {
		return map[string]any{}
	}
	store, ok := parsed.(map[string]any)
	if !ok {
		return map[string]any{}
	}
	return store
}

// Gateway URL without its userinfo, safe for a log line
func redactGatewayURL(baseURL string) string {
	parsed, err := parseURL(baseURL)
	if err != nil {
		return "<invalid gateway URL>"
	}
	parsed.SetUsername("")
	parsed.SetPassword("")
	return parsed.Protocol() + "//" + parsed.Host() + parsed.Pathname()
}

// PersistNativeTokenSet encrypts and stores a gateway token set beside the others, nil tokens dropping it
func PersistNativeTokenSet(baseURL string, tokens *NativeTokenSet, io NativeTokenStoreIO) error {
	store := readNativeTokenStore(io)
	if tokens != nil {
		plaintext, err := jsonMarshal(tokens)
		if err != nil {
			return err
		}
		secret, err := io.Encrypt(plaintext)
		if err != nil {
			return err
		}
		if secret == nil {
			return errors.New("Secure token storage returned no encrypted payload; refusing to overwrite stored native tokens.")
		}
		store[baseURL] = secret
	} else {
		delete(store, baseURL)
	}
	text, err := jsonMarshal(store)
	if err == nil {
		err = io.WriteStoreText(text)
	}
	if err != nil {
		io.log("[native-oauth] failed to persist tokens: " + err.Error())
	}
	return nil
}

// LoadNativeTokenSet decrypts the stored token set of a gateway, nil when absent, undecryptable or malformed
func LoadNativeTokenSet(baseURL string, io NativeTokenStoreIO) *NativeTokenSet {
	secret := readNativeTokenStore(io)[baseURL]
	if !jsTruthy(secret) {
		return nil
	}
	plaintext := io.Decrypt(secret)
	if plaintext == "" {
		io.log("[native-oauth] failed to decrypt stored tokens for " + redactGatewayURL(baseURL) + "; keeping stored entry for retry")
		return nil
	}
	var parsed any
	err := json.Unmarshal([]byte(plaintext), &parsed)
	var tokens NativeTokenSet
	if err == nil {
		tokens, err = ParseStoredTokenSet(parsed)
	}
	if err != nil {
		io.log("[native-oauth] failed to load stored tokens for " + redactGatewayURL(baseURL) + ": " + err.Error())
		return nil
	}
	return &tokens
}

func (s *Service) nativeTokenStorePath() string {
	return filepath.Join(s.userData, "native-oauth-tokens.json")
}

// Store IO over userData with the policy aware desktop secret encryption
func (s *Service) nativeTokenStoreIO() NativeTokenStoreIO {
	path := s.nativeTokenStorePath()
	return NativeTokenStoreIO{
		Encrypt: func(plaintext string) (any, error) { return s.encryptDesktopSecret(plaintext, false) },
		Decrypt: func(secret any) string {
			s.mu.Lock()
			defer s.mu.Unlock()
			return s.decryptDesktopSecret(secret)
		},
		ReadStoreText: func() (string, error) {
			data, err := os.ReadFile(path)
			return string(data), err
		},
		WriteStoreText: func(text string) error {
			if err := os.MkdirAll(filepath.Dir(path), 0o777); err != nil {
				return err
			}
			return os.WriteFile(path, []byte(text), 0o600)
		},
		RememberLog: s.log,
	}
}

// Policy aware desktop secret, plain when the keychain policy is off, sealed through safeStorage otherwise
func (s *Service) encryptDesktopSecret(value string, allowPlainText bool) (any, error) {
	s.mu.Lock()
	on := s.secretStoragePolicy().On
	s.mu.Unlock()
	if !on {
		if value == "" {
			return nil, nil
		}
		return SecretEnvelope{Encoding: "plain", Value: value}, nil
	}
	return EncryptDesktopSecretStrict(value, s.safe, allowPlainText)
}
