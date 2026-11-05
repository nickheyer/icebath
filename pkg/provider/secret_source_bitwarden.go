package provider

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"
)

const (
	bwsRunTimeout             = 30 * time.Second
	bwsDiskCacheBasename      = "bws_cache.json"
	bwsEncryptedCacheBasename = "bws_cache.enc.json"
	bwsEncryptedCacheVersion  = 1
	bwsEncryptedCacheInfo     = "icebath-bws-encrypted-cache-v1"
	bwsAuthRemediation        = "Run `icebath secrets bitwarden token` to paste a fresh access token (create one in the Bitwarden web app: Secrets Manager → Machine accounts → Access tokens).  Wrong region?  Re-run `icebath secrets bitwarden setup` and pick EU/self-hosted."
)

var bwsStore = NewSecretCache(bwsDiskCacheBasename)

var bwsErrorRules = []SecretErrorRule{
	{SecretErrorTimeout, []string{"timed out"}},
	{SecretErrorBinaryMissing, []string{"binary not available", "failed to invoke"}},
	{SecretErrorAuthFailed, []string{"unauthorized", "invalid token", "access token", "401", "403", "invalid_client", "invalid_grant", "400 bad request"}},
	{SecretErrorNetwork, []string{"network", "connection", "resolve", "download", "dns"}},
}

func classifyBwsError(message string) SecretErrorKind {
	return ClassifySecretCLIError(message, bwsErrorRules)
}

// FindBws resolves the bws binary from PATH or the tool store, installing on a miss when asked
func FindBws(installIfMissing bool) string {
	if system, err := exec.LookPath("bws"); err == nil {
		return system
	}
	if binary, ok := pmInstalledBinary("bws"); ok {
		return binary
	}
	if installIfMissing {
		binary, err := pmEnsure("bws", false)
		if err != nil {
			slog.Default().Warn(fmt.Sprintf("bws auto-install failed: %v", err))
			return ""
		}
		return binary
	}
	return ""
}

// InstallBws is the explicit setup or repair install of the pinned bws binary
func InstallBws() (string, error) { return pmEnsure("bws", true) }

func encryptedBwsCachePath(homePath string) string {
	return CurrentCachePath(ResolveCacheHome(homePath), bwsEncryptedCacheBasename)
}

// HKDF SHA-256 of the bootstrap token with the cache info string
func deriveEncryptedCacheKey(accessToken string, salt []byte) []byte {
	extractor := hmac.New(sha256.New, salt)
	extractor.Write([]byte(accessToken))
	prk := extractor.Sum(nil)
	expander := hmac.New(sha256.New, prk)
	expander.Write([]byte(bwsEncryptedCacheInfo))
	expander.Write([]byte{1})
	return expander.Sum(nil)[:32]
}

// Persists an AES-GCM encrypted last good entry, a success removes the plaintext cache
func writeEncryptedBwsCache(cacheKey, accessToken string, entry CachedFetch, homePath string) {
	salt := make([]byte, 16)
	nonce := make([]byte, 12)
	fillRandom(salt)
	fillRandom(nonce)
	key := deriveEncryptedCacheKey(accessToken, salt)
	plaintext, err := json.Marshal(map[string]any{"secrets": entry.Secrets, "fetched_at": entry.FetchedAt})
	if err != nil {
		return
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return
	}
	ciphertext := gcm.Seal(nil, nonce, plaintext, []byte(cacheKey))
	payload := map[string]any{"version": bwsEncryptedCacheVersion, "key": cacheKey, "salt": base64.StdEncoding.EncodeToString(salt),
		"nonce": base64.StdEncoding.EncodeToString(nonce), "ciphertext": base64.StdEncoding.EncodeToString(ciphertext)}
	if err := AtomicWriteCacheJSON(encryptedBwsCachePath(homePath), payload); err != nil {
		return
	}
	bwsStore.Disk.Clear(homePath)
}

// Decrypted encrypted cache entry when it matches the key and is in the age window
func readEncryptedBwsCache(cacheKey, accessToken string, maxAge float64, homePath string) (CachedFetch, bool) {
	if maxAge <= 0 {
		return CachedFetch{}, false
	}
	raw, err := os.ReadFile(encryptedBwsCachePath(homePath))
	if err != nil {
		return CachedFetch{}, false
	}
	var payload map[string]any
	if json.Unmarshal(bytes.TrimPrefix(raw, []byte("\xef\xbb\xbf")), &payload) != nil {
		return CachedFetch{}, false
	}
	version, _ := anyFloat(payload["version"])
	if int(version) != bwsEncryptedCacheVersion || anyString(payload["key"]) != cacheKey {
		return CachedFetch{}, false
	}
	decode := func(field string) ([]byte, bool) {
		out, err := base64.StdEncoding.Strict().DecodeString(anyString(payload[field]))
		return out, err == nil
	}
	salt, ok1 := decode("salt")
	nonce, ok2 := decode("nonce")
	ciphertext, ok3 := decode("ciphertext")
	if !ok1 || !ok2 || !ok3 {
		return CachedFetch{}, false
	}
	block, err := aes.NewCipher(deriveEncryptedCacheKey(accessToken, salt))
	if err != nil {
		return CachedFetch{}, false
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil || len(nonce) != gcm.NonceSize() {
		return CachedFetch{}, false
	}
	plaintext, err := gcm.Open(nil, nonce, ciphertext, []byte(cacheKey))
	if err != nil {
		return CachedFetch{}, false
	}
	var decoded any
	if json.Unmarshal(plaintext, &decoded) != nil {
		return CachedFetch{}, false
	}
	entry, ok := CachedFetchFromPayload(decoded)
	if !ok {
		return CachedFetch{}, false
	}
	age := nowEpoch() - entry.FetchedAt
	if age < 0 || age > maxAge {
		return CachedFetch{}, false
	}
	return entry, true
}

// BitwardenFetchOptions parameterize one pull of a BSM project
type BitwardenFetchOptions struct {
	AccessToken            string
	ProjectID              string
	Binary                 string
	CacheTTL               float64
	UseCache               bool
	ServerURL              string
	HomePath               string
	EncryptedCacheEnabled  bool
	EncryptedCacheMaxStale float64
	Environ                *SourceEnvironment
	InstallBinaryIfMissing bool
}

// FetchBitwardenSecrets pulls the project secrets, serving a stale last good entry only after a transport failure
func FetchBitwardenSecrets(opts BitwardenFetchOptions) (map[string]string, []string, error) {
	if opts.AccessToken == "" {
		return nil, nil, errors.New("Bitwarden access token is empty")
	}
	if opts.ProjectID == "" {
		return nil, nil, errors.New("Bitwarden project_id is empty")
	}
	cacheKey := strings.Join([]string{SecretFingerprint(opts.AccessToken), opts.ProjectID, opts.ServerURL}, "|")
	readEncrypted := func(maxAge float64) (CachedFetch, bool) {
		return readEncryptedBwsCache(cacheKey, opts.AccessToken, maxAge, opts.HomePath)
	}
	if opts.UseCache && opts.CacheTTL > 0 {
		var reader func() (CachedFetch, bool)
		if opts.EncryptedCacheEnabled {
			reader = func() (CachedFetch, bool) { return readEncrypted(opts.CacheTTL) }
		}
		cached, ok, err := bwsStore.Lookup(cacheKey, opts.CacheTTL, opts.HomePath, reader)
		if err != nil {
			return nil, nil, err
		}
		if ok {
			return cached.Secrets, nil, nil
		}
	}
	bws := opts.Binary
	if bws == "" {
		bws = FindBws(true)
	}
	if bws == "" {
		return nil, nil, errors.New("bws binary not available — auto-install failed and `bws` is not on PATH.  Install manually from https://github.com/bitwarden/sdk-sm/releases or re-run `icebath secrets bitwarden setup`.")
	}
	secrets, warnings, err := runBwsList(bws, opts.AccessToken, opts.ProjectID, opts.ServerURL, opts.Environ)
	if err != nil {
		kind := classifyBwsError(err.Error())
		if opts.UseCache && (kind == SecretErrorNetwork || kind == SecretErrorTimeout) {
			var stale CachedFetch
			found, label := false, ""
			if opts.EncryptedCacheEnabled {
				stale, found = readEncrypted(opts.EncryptedCacheMaxStale)
				label = "stale ENCRYPTED disk cache"
			} else if opts.CacheTTL > 0 {
				var readErr error
				if stale, found, readErr = bwsStore.Disk.Read(cacheKey, math.Inf(1), opts.HomePath); readErr != nil {
					return nil, nil, readErr
				}
				label = "stale disk cache"
			}
			if found {
				age := math.Max(0, nowEpoch()-stale.FetchedAt)
				bwsStore.StoreMemory(cacheKey, stale)
				return stale.Secrets, []string{fmt.Sprintf("bws live fetch failed (%v); falling back to %s (%ds old)", err, label, int(age))}, nil
			}
		}
		return nil, nil, err
	}
	entry := CachedFetch{Secrets: secrets, FetchedAt: nowEpoch()}
	if opts.UseCache {
		if opts.CacheTTL > 0 {
			bwsStore.StoreMemory(cacheKey, entry)
		}
		if opts.EncryptedCacheEnabled {
			writeEncryptedBwsCache(cacheKey, opts.AccessToken, entry, opts.HomePath)
		} else {
			bwsStore.Disk.Write(cacheKey, entry, opts.CacheTTL, opts.HomePath)
		}
	}
	return secrets, warnings, nil
}

var bwsCauseNumberRe = regexp.MustCompile(`^\d+:\s*`)

// Reduces a bws error dump to its numbered cause lines, raw text when unrecognized
func summarizeBwsStderr(raw string) string {
	text := strings.TrimSpace(strings.ReplaceAll(raw, "\x1b", ""))
	var causes []string
	for _, line := range strings.Split(text, "\n") {
		stripped := strings.TrimSpace(line)
		if strings.HasPrefix(stripped, "Location:") || strings.HasPrefix(stripped, "Backtrace omitted") || strings.HasPrefix(stripped, "Run with ") {
			break
		}
		if stripped == "" || stripped == "Error:" {
			continue
		}
		if cause := bwsCauseNumberRe.ReplaceAllString(stripped, ""); cause != "" {
			causes = append(causes, cause)
		}
	}
	if len(causes) > 0 {
		return strings.Join(causes, "; ")
	}
	return text
}

func runBwsList(bws, accessToken, projectID, serverURL string, environ *SourceEnvironment) (map[string]string, []string, error) {
	cmd := []string{bws, "secret", "list", projectID, "--output", "json"}
	env := SourceChildEnv(environ)
	env["BWS_ACCESS_TOKEN"] = accessToken
	if _, ok := env["NO_COLOR"]; !ok {
		env["NO_COLOR"] = "1"
	}
	if serverURL != "" {
		env["BWS_SERVER_URL"] = serverURL
	}
	proc, err := RunCLI(cmd, env, bwsRunTimeout, "bws", fmt.Sprintf("bws timed out after %.0fs fetching secrets", bwsRunTimeout.Seconds()), nil)
	if err != nil {
		return nil, nil, err
	}
	if proc.ReturnCode != 0 {
		text := proc.Stderr
		if text == "" {
			text = proc.Stdout
		}
		summary := summarizeBwsStderr(text)
		if len(summary) > 200 {
			summary = summary[:200]
		}
		return nil, nil, fmt.Errorf("bws exited %d: %s", proc.ReturnCode, summary)
	}
	raw := strings.TrimSpace(proc.Stdout)
	if raw == "" {
		return map[string]string{}, []string{"bws returned no output (empty project?)"}, nil
	}
	var payload any
	if err := json.Unmarshal([]byte(raw), &payload); err != nil {
		return nil, nil, fmt.Errorf("bws returned non-JSON output: %v", err)
	}
	items, ok := payload.([]any)
	if !ok {
		return nil, nil, fmt.Errorf("bws returned unexpected shape: %T", payload)
	}
	secrets := map[string]string{}
	var warnings []string
	for _, item := range items {
		m, isMap := item.(map[string]any)
		if !isMap {
			continue
		}
		key, keyOK := m["key"].(string)
		value, valueOK := m["value"].(string)
		if !keyOK || !valueOK {
			continue
		}
		if IsValidEnvName(key) {
			secrets[key] = value
		} else {
			warnings = append(warnings, fmt.Sprintf("Skipping secret %q: not a valid env-var name", key))
		}
	}
	return secrets, warnings, nil
}

// BitwardenSource is Bitwarden Secrets Manager as a bulk source
type BitwardenSource struct{}

func (s *BitwardenSource) Spec() SecretSourceSpec {
	return SecretSourceSpec{APIVersion: SecretSourceAPIVersion, Name: "bitwarden", Label: "Bitwarden Secrets Manager", Shape: "bulk", Scheme: "bws",
		TokenEnvKey: "access_token_env", DefaultTokenEnv: "BWS_ACCESS_TOKEN", OverrideExistingDefault: true,
		RemediationHints: map[SecretErrorKind]string{SecretErrorAuthFailed: bwsAuthRemediation, SecretErrorAuthExpired: bwsAuthRemediation}}
}

func (s *BitwardenSource) ConfigSchema() map[string]map[string]any {
	return map[string]map[string]any{
		"enabled":           {"description": "Master switch", "default": false},
		"access_token_env":  {"description": "Env var holding the machine-account access token", "default": "BWS_ACCESS_TOKEN"},
		"project_id":        {"description": "BSM project UUID", "default": ""},
		"cache_ttl_seconds": {"description": "Fresh disk+memory cache TTL; 0 disables fresh-cache reuse", "default": 300},
		"encrypted_cache":   {"description": "Encrypted last-good cache for network/timeout fallback", "default": map[string]any{"enabled": false, "max_stale_seconds": 0}},
		"override_existing": {"description": "BSM values overwrite .env/shell values", "default": true},
		"auto_install":      {"description": "Auto-download the pinned bws binary", "default": true},
		"server_url":        {"description": "Region / self-hosted endpoint (empty = US Cloud)", "default": ""},
	}
}

func (s *BitwardenSource) Fetch(cfg map[string]any, homePath string, environ *SourceEnvironment) *SecretFetchResult {
	result := NewSecretFetchResult()
	tokenEnv := SecretSourceTokenEnv(s.Spec(), cfg)
	accessToken := strings.TrimSpace(environ.Value(tokenEnv))
	if accessToken == "" {
		return result.Fail(fmt.Sprintf("secrets.bitwarden.enabled is true but %s is not set.  Run `icebath secrets bitwarden setup`.", tokenEnv), SecretErrorNotConfigured)
	}
	projectID := anyString(cfg["project_id"])
	if projectID == "" {
		return result.Fail("secrets.bitwarden.project_id is empty.  Run `icebath secrets bitwarden setup`.", SecretErrorNotConfigured)
	}
	autoInstall := true
	if v, ok := cfg["auto_install"]; ok {
		autoInstall = truthyAny(v)
	}
	binary := FindBws(autoInstall)
	result.BinaryPath = binary
	if binary == "" {
		return result.Fail("bws binary not available and auto-install is disabled.  Run `icebath secrets bitwarden setup` to install.", SecretErrorBinaryMissing)
	}
	encryptedCfg, _ := cfg["encrypted_cache"].(map[string]any)
	ttl := CoerceFloat(cfg["cache_ttl_seconds"], 300)
	if _, ok := cfg["cache_ttl_seconds"]; !ok {
		ttl = 300
	}
	secrets, warnings, err := FetchBitwardenSecrets(BitwardenFetchOptions{AccessToken: accessToken, ProjectID: projectID, Binary: binary,
		CacheTTL: ttl, UseCache: true, ServerURL: strings.TrimSpace(anyString(cfg["server_url"])), HomePath: homePath,
		EncryptedCacheEnabled: truthyAny(encryptedCfg["enabled"]), EncryptedCacheMaxStale: CoerceFloat(encryptedCfg["max_stale_seconds"], 0), Environ: environ})
	if err != nil {
		result.Fail(err.Error(), classifyBwsError(err.Error()))
		if result.ErrorKind == SecretErrorAuthFailed {
			result.Error = fmt.Sprintf("Bitwarden rejected the machine-account access token (%s) — it was likely revoked, expired, or belongs to another region.  (%s)", tokenEnv, result.Error)
		}
		return result
	}
	result.Secrets = secrets
	result.Warnings = append(result.Warnings, warnings...)
	return result
}

// ClearBitwardenCaches drops the in process and both disk caches after a token rotation
func ClearBitwardenCaches(homePath string) {
	bwsStore.Clear(homePath)
	RemoveCacheFile(ResolveCacheHome(homePath), bwsEncryptedCacheBasename)
}
