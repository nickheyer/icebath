package secret

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"regexp"
	"strings"
	"time"

	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/fileutil"
	"github.com/nickheyer/icebath/internal/home"
	"github.com/nickheyer/icebath/internal/toolchain"
)

// Bound on one bws run and the shape of the encrypted cache file
const (
	bwsRunTimeout         = 30 * time.Second
	bwsDefaultTokenEnv    = "BWS_ACCESS_TOKEN"
	bwsDefaultTTL         = 300.0
	bwsEncryptedVersion   = 1
	bwsEncryptedCacheInfo = "icebath-bws-encrypted-cache-v1"
	bwsAuthFix            = "Run `icebath secrets bitwarden token` to paste a fresh access token (create one in the Bitwarden web app: " +
		"Secrets Manager → Machine accounts → Access tokens).  Wrong region?  Re-run `icebath secrets bitwarden setup` and pick EU/self-hosted."
)

// Failure classes of bws messages, the first matching rule winning
var bwsErrorRules = []errorRule{
	{Timeout, []string{"timed out"}},
	{BinaryMissing, []string{"binary not available", "failed to invoke"}},
	{AuthFailed, []string{"unauthorized", "invalid token", "access token", "401", "403", "invalid_client", "invalid_grant", "400 bad request"}},
	{Network, []string{"network", "connection", "resolve", "download", "dns"}},
}

// Leading cause number of a bws error line
var bwsCauseNumber = regexp.MustCompile(`^\d+:\s*`)

// Bitwarden Secrets Manager through the pinned bws program, one list call per project
type BitwardenSource struct {
	cache *secretCache
	tools *toolchain.Store
}

// Source installing bws through tools, with an empty cache
func NewBitwardenSource(tools *toolchain.Store) *BitwardenSource {
	return &BitwardenSource{cache: newSecretCache(home.BitwardenCache), tools: tools}
}

// Usable bws program from the search path, then the store, then an install when allowed, empty for none
func (s *BitwardenSource) FindBws(ctx context.Context, installIfMissing bool) string {
	found, err := s.tools.Resolve(ctx, toolchain.Bws, toolchain.Lookup{System: []string{"bws"}, SystemFirst: true, Install: installIfMissing})
	if err != nil {
		if installIfMissing && !errors.Is(err, toolchain.ErrNotInstalled) {
			slog.Warn(fmt.Sprintf("bws auto-install failed: %v", err))
		}
		return ""
	}
	return found.Path
}

// Installs the pinned bws on purpose, verifying and repairing an existing install, and returns its path
func (s *BitwardenSource) InstallBws(ctx context.Context, progress func(toolchain.Progress)) (string, error) {
	inst, err := s.tools.Ensure(ctx, toolchain.Bws, toolchain.EnsureOptions{Explicit: true, Progress: progress})
	if err != nil {
		return "", err
	}
	return inst.Binary, nil
}

// Key of the encrypted cache derived from the access token and a salt
func deriveCacheKey(token string, salt []byte) ([]byte, error) {
	return hkdf.Key(sha256.New, []byte(token), salt, bwsEncryptedCacheInfo, 32)
}

// AES-GCM sealer under the derived key
func cacheCipher(token string, salt []byte) (cipher.AEAD, error) {
	key, err := deriveCacheKey(token, salt)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// Encrypted cache file as stored
type encryptedCache struct {
	Version    int    `json:"version"`
	Key        string `json:"key"`
	Salt       string `json:"salt"`
	Nonce      string `json:"nonce"`
	Ciphertext string `json:"ciphertext"`
}

// Writes the entry encrypted and, once that file exists, removes the plain cache file
func (s *BitwardenSource) writeEncrypted(ctx context.Context, homeDir, key, token string, entry cachedFetch) {
	salt, nonce := make([]byte, 16), make([]byte, 12)
	if _, err := rand.Read(salt); err != nil {
		return
	}
	if _, err := rand.Read(nonce); err != nil {
		return
	}
	aead, err := cacheCipher(token, salt)
	if err != nil {
		return
	}
	plain, err := json.Marshal(map[string]any{"secrets": entry.secrets, "fetched_at": entry.fetchedAt})
	if err != nil {
		return
	}
	payload := encryptedCache{
		Version: bwsEncryptedVersion, Key: key,
		Salt: base64.StdEncoding.EncodeToString(salt), Nonce: base64.StdEncoding.EncodeToString(nonce),
		Ciphertext: base64.StdEncoding.EncodeToString(aead.Seal(nil, nonce, plain, []byte(key))),
	}
	if writeCacheJSON(ctx, currentCachePath(homeDir, home.BitwardenEncryptedCache), payload) != nil {
		return
	}
	removeCacheFile(homeDir, home.BitwardenCache)
}

// Decrypted entry of the encrypted cache when it is for key and no older than maxAge seconds
func (s *BitwardenSource) readEncrypted(homeDir, key, token string, maxAge float64) (cachedFetch, bool) {
	if maxAge <= 0 {
		return cachedFetch{}, false
	}
	var payload encryptedCache
	if fileutil.ReadJSON(currentCachePath(homeDir, home.BitwardenEncryptedCache), &payload) != nil ||
		payload.Version != bwsEncryptedVersion || payload.Key != key {
		return cachedFetch{}, false
	}
	salt, errA := base64.StdEncoding.Strict().DecodeString(payload.Salt)
	nonce, errB := base64.StdEncoding.Strict().DecodeString(payload.Nonce)
	sealed, errC := base64.StdEncoding.Strict().DecodeString(payload.Ciphertext)
	if errA != nil || errB != nil || errC != nil {
		return cachedFetch{}, false
	}
	aead, err := cacheCipher(token, salt)
	if err != nil || len(nonce) != aead.NonceSize() {
		return cachedFetch{}, false
	}
	plain, err := aead.Open(nil, nonce, sealed, []byte(key))
	if err != nil {
		return cachedFetch{}, false
	}
	var decoded map[string]json.RawMessage
	if json.Unmarshal(plain, &decoded) != nil {
		return cachedFetch{}, false
	}
	entry, ok := entryFromPayload(decoded)
	if !ok {
		return cachedFetch{}, false
	}
	if age := nowSeconds() - entry.fetchedAt; age < 0 || age > maxAge {
		return cachedFetch{}, false
	}
	return entry, true
}

// Cause lines of a bws error dump joined with semicolons, the raw text when it has no recognisable causes
func summarizeBwsError(raw string) string {
	text := strings.TrimSpace(strings.ReplaceAll(raw, "\x1b", ""))
	var causes []string
	for _, line := range strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n") {
		stripped := strings.TrimSpace(line)
		if strings.HasPrefix(stripped, "Location:") || strings.HasPrefix(stripped, "Backtrace omitted") || strings.HasPrefix(stripped, "Run with ") {
			break
		}
		if stripped == "" || stripped == "Error:" {
			continue
		}
		if cause := bwsCauseNumber.ReplaceAllString(stripped, ""); cause != "" {
			causes = append(causes, cause)
		}
	}
	if len(causes) == 0 {
		return text
	}
	return strings.Join(causes, "; ")
}

// JSON type of a value in words
func jsonKind(raw json.RawMessage) string {
	switch strings.TrimSpace(string(raw))[0] {
	case '{':
		return "object"
	case '"':
		return "string"
	case 't', 'f':
		return "boolean"
	case 'n':
		return "null"
	}
	return "number"
}

// Lists the secrets of a project by legal variable name in list order, warning about every other key
func runBwsList(ctx context.Context, bws, token, projectID, serverURL string, childEnv []string) (map[string]string, []string, []string, error) {
	env := envMap(childEnv)
	env[bwsDefaultTokenEnv] = token
	if _, ok := env["NO_COLOR"]; !ok {
		env["NO_COLOR"] = "1"
	}
	if serverURL != "" {
		env["BWS_SERVER_URL"] = serverURL
	}
	res, err := runCLI(ctx, []string{bws, "secret", "list", projectID, "--output", "json"}, envList(env), bwsRunTimeout, "bws",
		fmt.Sprintf("bws timed out after %.0fs fetching secrets", bwsRunTimeout.Seconds()), nil)
	if err != nil {
		return nil, nil, nil, err
	}
	if res.Code != 0 {
		text := res.Stderr
		if text == "" {
			text = res.Stdout
		}
		detail := summarizeBwsError(text)
		if runes := []rune(detail); len(runes) > 200 {
			detail = string(runes[:200])
		}
		return nil, nil, nil, fmt.Errorf("bws exited %d: %s", res.Code, detail)
	}
	raw := strings.TrimSpace(res.Stdout)
	if raw == "" {
		return map[string]string{}, nil, []string{"bws returned no output (empty project?)"}, nil
	}
	var probe json.RawMessage
	if err := json.Unmarshal([]byte(raw), &probe); err != nil {
		return nil, nil, nil, fmt.Errorf("bws returned non-JSON output: %v", err)
	}
	var items []json.RawMessage
	if json.Unmarshal(probe, &items) != nil || strings.TrimSpace(string(probe))[0] != '[' {
		return nil, nil, nil, fmt.Errorf("bws returned unexpected shape: %s", jsonKind(probe))
	}
	secrets := map[string]string{}
	var order, warnings []string
	for _, item := range items {
		var fields map[string]json.RawMessage
		if json.Unmarshal(item, &fields) != nil {
			continue
		}
		var key, value string
		if !jsonString(fields["key"], &key) || !jsonString(fields["value"], &value) {
			continue
		}
		if !ValidEnvName(key) {
			warnings = append(warnings, fmt.Sprintf("Skipping secret '%s': not a valid env-var name", key))
			continue
		}
		if _, seen := secrets[key]; !seen {
			order = append(order, key)
		}
		secrets[key] = value
	}
	return secrets, order, warnings, nil
}

// Decodes raw into out when it is a JSON string
func jsonString(raw json.RawMessage, out *string) bool {
	return len(raw) > 0 && raw[0] == '"' && json.Unmarshal(raw, out) == nil
}

// What one Bitwarden pull reads
type BitwardenFetch struct {
	AccessToken string
	ProjectID   string
	// bws program to run, found or installed when empty
	Binary string
	// Seconds a pull stays fresh in memory and on disk, zero disabling fresh reuse
	CacheTTL float64
	// Skips every cache layer
	NoCache bool
	// Region or self hosted endpoint, empty for the US cloud
	ServerURL string
	Home      string
	// Keeps the last good pull encrypted for network and timeout failures
	EncryptedCache bool
	// Seconds an encrypted last good pull may be served during an outage
	EncryptedMaxStale float64
	// Environment the bws child runs with before the token is added
	ChildEnv []string
}

// Outcome of one Bitwarden pull
type BitwardenSecrets struct {
	Secrets  map[string]string
	Order    []string
	Warnings []string
}

// Pulls the secrets of a project, cached, a transport failure answered from the last good cache
func (s *BitwardenSource) FetchSecrets(ctx context.Context, in BitwardenFetch) (BitwardenSecrets, error) {
	if in.AccessToken == "" {
		return BitwardenSecrets{}, errors.New("Bitwarden access token is empty")
	}
	if in.ProjectID == "" {
		return BitwardenSecrets{}, errors.New("Bitwarden project_id is empty")
	}
	key := strings.Join([]string{fingerprint(in.AccessToken), in.ProjectID, in.ServerURL}, "|")
	useCache := !in.NoCache
	if useCache && in.CacheTTL > 0 {
		var readDisk func() (cachedFetch, bool)
		if in.EncryptedCache {
			readDisk = func() (cachedFetch, bool) { return s.readEncrypted(in.Home, key, in.AccessToken, in.CacheTTL) }
		}
		if cached, ok := s.cache.lookup(in.Home, key, key, in.CacheTTL, readDisk); ok {
			return BitwardenSecrets{Secrets: cached.secrets}, nil
		}
	}
	bws := in.Binary
	if bws == "" {
		bws = s.FindBws(ctx, true)
	}
	if bws == "" {
		return BitwardenSecrets{}, errors.New("bws binary not available — auto-install failed and `bws` is not on PATH.  Install manually from " +
			"https://github.com/bitwarden/sdk-sm/releases or re-run `icebath secrets bitwarden setup`.")
	}
	secrets, order, warnings, err := runBwsList(ctx, bws, in.AccessToken, in.ProjectID, in.ServerURL, in.ChildEnv)
	if err != nil {
		if kind := classify(err.Error(), bwsErrorRules); useCache && ctx.Err() == nil && (kind == Network || kind == Timeout) {
			var stale cachedFetch
			found, label := false, ""
			if in.EncryptedCache {
				stale, found = s.readEncrypted(in.Home, key, in.AccessToken, in.EncryptedMaxStale)
				label = "stale ENCRYPTED disk cache"
			} else if in.CacheTTL > 0 {
				stale, found = s.cache.readDisk(in.Home, key, math.Inf(1))
				label = "stale disk cache"
			}
			if found {
				age := math.Max(0, nowSeconds()-stale.fetchedAt)
				s.cache.remember(key, stale)
				return BitwardenSecrets{Secrets: stale.secrets, Warnings: []string{
					fmt.Sprintf("bws live fetch failed (%v); falling back to %s (%ds old)", err, label, int(age)),
				}}, nil
			}
		}
		return BitwardenSecrets{}, err
	}
	entry := cachedFetch{secrets: secrets, fetchedAt: nowSeconds()}
	if useCache {
		if in.CacheTTL > 0 {
			s.cache.remember(key, entry)
		}
		if in.EncryptedCache {
			s.writeEncrypted(ctx, in.Home, key, in.AccessToken, entry)
		} else {
			s.cache.writeDisk(ctx, in.Home, key, entry, in.CacheTTL)
		}
	}
	return BitwardenSecrets{Secrets: secrets, Order: order, Warnings: warnings}, nil
}

// Drops the memory cache and both cache files of a home, for after a token rotation
func (s *BitwardenSource) ClearCaches(homeDir string) {
	s.cache.clear(homeDir)
	removeCacheFile(homeDir, home.BitwardenEncryptedCache)
}

// Section key and provenance label
func (*BitwardenSource) Name() string { return "bitwarden" }

// Words status lines use
func (*BitwardenSource) Label() string { return "Bitwarden Secrets Manager" }

// Whole project injected at once
func (*BitwardenSource) Shape() Shape { return Bulk }

// Prefix of bws references
func (*BitwardenSource) Scheme() string { return "bws" }

// Reports the master switch
func (*BitwardenSource) Enabled(cfg config.Raw) bool { return sectionOf(cfg).truthy("enabled", false) }

// Central rotation beats a stale env file line unless told otherwise
func (*BitwardenSource) Override(cfg config.Raw) bool {
	return sectionOf(cfg).truthy("override_existing", true)
}

// Variable holding the machine account access token
func (*BitwardenSource) TokenEnv(cfg config.Raw) string {
	if name := sectionOf(cfg).text("access_token_env"); name != "" {
		return name
	}
	return bwsDefaultTokenEnv
}

// The token variable, which no source may overwrite
func (s *BitwardenSource) Protected(cfg config.Raw) []string { return []string{s.TokenEnv(cfg)} }

// Budget of the whole fetch
func (*BitwardenSource) Timeout(cfg config.Raw) time.Duration { return fetchTimeout(cfg) }

// Next step for a failed fetch
func (s *BitwardenSource) Remediation(kind ErrorKind, cfg config.Raw) string {
	return remediationFor(kind, map[ErrorKind]string{AuthFailed: bwsAuthFix, AuthExpired: bwsAuthFix}, "bitwarden", s.TokenEnv(cfg))
}

// Pulls every secret of the configured project
func (s *BitwardenSource) Fetch(ctx context.Context, in FetchInput) Result {
	cfg := sectionOf(in.Config)
	var result Result
	tokenEnv := s.TokenEnv(in.Config)
	token := strings.TrimSpace(in.Env[tokenEnv])
	if token == "" {
		return result.fail(fmt.Sprintf("secrets.bitwarden.enabled is true but %s is not set.  Run `icebath secrets bitwarden setup`.", tokenEnv), NotConfigured)
	}
	projectID := cfg.text("project_id")
	if projectID == "" {
		return result.fail("secrets.bitwarden.project_id is empty.  Run `icebath secrets bitwarden setup`.", NotConfigured)
	}
	binary := s.FindBws(ctx, cfg.truthy("auto_install", true))
	result.Binary = binary
	if binary == "" {
		return result.fail("bws binary not available and auto-install is disabled.  Run `icebath secrets bitwarden setup` to install.", BinaryMissing)
	}
	encrypted := cfg.sub("encrypted_cache")
	pulled, err := s.FetchSecrets(ctx, BitwardenFetch{
		AccessToken: token, ProjectID: projectID, Binary: binary,
		CacheTTL: cfg.float("cache_ttl_seconds", bwsDefaultTTL), ServerURL: strings.TrimSpace(cfg.text("server_url")), Home: in.Home,
		EncryptedCache: encrypted.truthy("enabled", false), EncryptedMaxStale: encrypted.float("max_stale_seconds", 0),
		ChildEnv: in.ChildEnv,
	})
	if err != nil {
		result = result.fail(err.Error(), classify(err.Error(), bwsErrorRules))
		if result.Kind == AuthFailed {
			result.Err = fmt.Sprintf("Bitwarden rejected the machine-account access token (%s) — it was likely revoked, expired, or belongs to another region.  (%s)", tokenEnv, result.Err)
		}
		return result
	}
	result.Secrets, result.Order = pulled.Secrets, pulled.Order
	result.Warnings = append(result.Warnings, pulled.Warnings...)
	return result
}
