package provider

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
)

const (
	anthropicOAuthClientID = "9d1c250a-e61b-44d9-88ed-5944d1962f5e"
	// Bare axios there because the token endpoint 429s claude-code and Mozilla user agents
	anthropicOAuthTokenUserAgent = "axios/1.7.9"
	anthropicOAuthRedirectURI    = "https://console.anthropic.com/oauth/code/callback"
	anthropicOAuthScopes         = "org:create_api_key user:profile user:inference"
	claudeCodeKeychainService    = "Claude Code-credentials"
	spentRotationMaxTracked      = 64
	spentRotationSidecarComment  = "Non-secret one-way fingerprints of Anthropic OAuth credentials whose rotation was " +
		"consumed server-side but never durably committed. Written by Icebath so sibling " +
		"processes sharing this credential source fail closed instead of replaying a spent " +
		"single-use refresh token."
)

// Live token host first, the console host kept as a fallback
var anthropicOAuthTokenURLs = []string{"https://platform.claude.com/v1/oauth/token", "https://console.anthropic.com/v1/oauth/token"}

// IsAnthropicOAuthToken is true for OAuth and setup tokens, false for the Console API key families
func IsAnthropicOAuthToken(key string) bool {
	if key == "" {
		return false
	}
	for _, p := range []string{"sk-ant-api", "sk-ant-usr", "sk-ant-svc", "sk-ant-admin"} {
		if strings.HasPrefix(key, p) {
			return false
		}
	}
	for _, p := range []string{"sk-ant-", "eyJ", "cc-"} {
		if strings.HasPrefix(key, p) {
			return true
		}
	}
	return false
}

// IsNativeAnthropicHost is true when the base URL is exactly api.anthropic.com or empty
func IsNativeAnthropicHost(baseURL string) bool {
	text := strings.TrimSpace(baseURL)
	return text == "" || baseURLHostname(text) == "api.anthropic.com"
}

// AnthropicRouteIsOAuth reports the Claude Code OAuth identity for one Anthropic Messages route
func AnthropicRouteIsOAuth(ctx context.Context, baseURL, credential string, tokenSource func(context.Context) (string, error), provider string) bool {
	if !(IsNativeAnthropicHost(baseURL) || strings.ToLower(strings.TrimSpace(provider)) == "anthropic") {
		return false
	}
	if credential == "" && tokenSource != nil {
		minted, err := tokenSource(ctx)
		if err != nil {
			return false
		}
		credential = minted
	}
	return IsAnthropicOAuthToken(credential)
}

// CredentialPersistError is a rotated single use credential that could not be durably committed
type CredentialPersistError struct {
	Path  string
	Cause error
}

func (e *CredentialPersistError) Error() string {
	return fmt.Sprintf("failed to durably persist rotated Anthropic credentials to %s: %v", e.Path, e.Cause)
}

func (e *CredentialPersistError) Unwrap() error { return e.Cause }

// Parsed JSON from the path, nil when missing unreadable or corrupt
func loadJSONIfExists(env PoolEnv, path, what string) map[string]any {
	raw, err := os.ReadFile(path)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			env.log().Debug(fmt.Sprintf("Failed to read %s: %v", what, err))
		}
		return nil
	}
	raw = bytes.TrimPrefix(raw, []byte("\xef\xbb\xbf"))
	var data map[string]any
	if err := json.Unmarshal(raw, &data); err != nil {
		env.log().Debug(fmt.Sprintf("Failed to read %s: %v", what, err))
		return nil
	}
	return data
}

// Private atomic writer behind every singleton commit, swappable so tests can fail the durable write
var atomicWritePrivate = config.AtomicWrite

// Atomic private write whose failure becomes CredentialPersistError
func commitPrivateJSON(env PoolEnv, path string, payload any, what string) error {
	raw, err := json.MarshalIndent(payload, "", "  ")
	if err == nil {
		err = atomicWritePrivate(path, raw, 0o600)
	}
	if err != nil {
		env.log().Error(fmt.Sprintf("Failed to write refreshed %s to %s: %v", what, path, err))
		return &CredentialPersistError{Path: path, Cause: err}
	}
	return nil
}

var (
	spentRotationMu           sync.Mutex
	deadRefreshTokenPrints    = map[string]bool{}
	spentRotationFingerprints []string
)

func spentRotationSidecarPath(sourcePath string) string {
	return filepath.Join(filepath.Dir(sourcePath), filepath.Base(sourcePath)+".icebath-spent-rotations.json")
}

// SpentRotationSourcePath maps a pool entry source to the shared singleton file it borrows from
func SpentRotationSourcePath(env PoolEnv, source string) string {
	switch source {
	case "claude_code":
		return ClaudeCodeCredentialsPath()
	case "hermes_pkce":
		return HermesOAuthFile(env)
	}
	return ""
}

func readSpentRotationSidecar(sourcePath string) map[string]bool {
	out := map[string]bool{}
	if sourcePath == "" {
		return out
	}
	raw, err := os.ReadFile(spentRotationSidecarPath(sourcePath))
	if err != nil {
		return out
	}
	var payload map[string]any
	if json.Unmarshal(bytes.TrimPrefix(raw, []byte("\xef\xbb\xbf")), &payload) != nil {
		return out
	}
	list, _ := payload["fingerprints"].([]any)
	for _, fp := range list {
		if s, ok := fp.(string); ok && s != "" {
			out[s] = true
		}
	}
	return out
}

// Merges fingerprints into the sidecar, a write failure never masks the process local verdict
func appendSpentRotationSidecar(env PoolEnv, sourcePath string, fingerprints []string) {
	sidecar := spentRotationSidecarPath(sourcePath)
	merged := readSpentRotationSidecar(sourcePath)
	for _, fp := range fingerprints {
		merged[fp] = true
	}
	keys := make([]string, 0, len(merged))
	for k := range merged {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	if len(keys) > spentRotationMaxTracked*4 {
		keys = keys[len(keys)-spentRotationMaxTracked*4:]
	}
	payload, err := json.MarshalIndent(map[string]any{"version": 1, "comment": spentRotationSidecarComment, "fingerprints": keys}, "", "  ")
	if err == nil {
		err = os.MkdirAll(filepath.Dir(sidecar), 0o755)
	}
	if err == nil {
		tmp := sidecar + ".tmp"
		if err = os.WriteFile(tmp, payload, 0o644); err == nil {
			err = os.Rename(tmp, sidecar)
		}
	}
	if err != nil {
		env.log().Debug(fmt.Sprintf("Failed to persist spent-rotation fingerprints to %s: %v", sidecar, err))
	}
}

func secretFingerprintOf(secret string) string {
	value := strings.TrimSpace(secret)
	if value == "" {
		return ""
	}
	return FingerprintSecretValue(value)
}

// MarkRotationConsumedUncommitted records the pre rotation pair of a refresh whose replacement never committed
func MarkRotationConsumedUncommitted(sourcePath string, secrets ...string) {
	var recorded []string
	for _, s := range secrets {
		if fp := secretFingerprintOf(s); fp != "" {
			recorded = append(recorded, fp)
		}
	}
	spentRotationMu.Lock()
	for _, fp := range recorded {
		kept := spentRotationFingerprints[:0:0]
		for _, existing := range spentRotationFingerprints {
			if existing != fp {
				kept = append(kept, existing)
			}
		}
		spentRotationFingerprints = append(kept, fp)
		if len(spentRotationFingerprints) > spentRotationMaxTracked {
			spentRotationFingerprints = spentRotationFingerprints[len(spentRotationFingerprints)-spentRotationMaxTracked:]
		}
	}
	spentRotationMu.Unlock()
	if len(recorded) > 0 && sourcePath != "" {
		appendSpentRotationSidecar(PoolEnv{}, sourcePath, recorded)
	}
}

// IsRotationConsumedUncommitted is true when the secret belongs to a rotation that was spent but not committed
func IsRotationConsumedUncommitted(secret, sourcePath string) bool {
	fp := secretFingerprintOf(secret)
	if fp == "" {
		return false
	}
	spentRotationMu.Lock()
	for _, existing := range spentRotationFingerprints {
		if existing == fp {
			spentRotationMu.Unlock()
			return true
		}
	}
	spentRotationMu.Unlock()
	return readSpentRotationSidecar(sourcePath)[fp]
}

// ResetSpentRotationsForTests clears the process local spent rotation registry
func ResetSpentRotationsForTests() {
	spentRotationMu.Lock()
	defer spentRotationMu.Unlock()
	spentRotationFingerprints = nil
	deadRefreshTokenPrints = map[string]bool{}
}

// Normalizes a claudeAiOauth payload into the credential record shape
func claudeOAuthRecord(data map[string]any, source string) map[string]any {
	oauth, _ := data["claudeAiOauth"].(map[string]any)
	access := anyString(oauth["accessToken"])
	if access == "" {
		return nil
	}
	expires, _ := anyFloat(oauth["expiresAt"])
	return map[string]any{"accessToken": access, "refreshToken": anyString(oauth["refreshToken"]), "expiresAt": expires, "source": source}
}

var keychainAttrRe = regexp.MustCompile(`(?m)^\s*"acct"<blob>=(?:0x([0-9A-Fa-f]+)\b.*|"(.*)")\s*$`)
var keychainPasswordRe = regexp.MustCompile(`(?m)^password: (?:0x([0-9A-Fa-f]+)\b.*|"(.*)")\s*$`)

// Decodes a security attribute printed as hex or as a raw quoted string
func decodeKeychainAttr(match []string) string {
	if match == nil {
		return ""
	}
	if match[1] != "" {
		raw, err := hex.DecodeString(match[1])
		if err != nil {
			return ""
		}
		return string(raw)
	}
	return match[2]
}

func runSecurity(args []string, stdin string, timeout time.Duration) (string, string, int, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "security", args...)
	cmd.Stdin = strings.NewReader(stdin)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	code := 0
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		code = exitErr.ExitCode()
		err = nil
	}
	return stdout.String(), stderr.String(), code, err
}

// Account and payload of the Claude Code login Keychain item
func findClaudeCodeKeychainItem() (string, map[string]any, bool) {
	if runtime.GOOS != "darwin" {
		return "", nil, false
	}
	stdout, stderr, code, err := runSecurity([]string{"find-generic-password", "-s", claudeCodeKeychainService, "-g"}, "", 5*time.Second)
	if err != nil || code != 0 {
		return "", nil, false
	}
	account := decodeKeychainAttr(keychainAttrRe.FindStringSubmatch(stdout))
	raw := decodeKeychainAttr(keychainPasswordRe.FindStringSubmatch(stderr))
	if account == "" || raw == "" {
		return "", nil, false
	}
	var payload map[string]any
	if json.Unmarshal([]byte(raw), &payload) != nil || payload == nil {
		return "", nil, false
	}
	return account, payload, true
}

// Raw Keychain payload so a refresh write can merge over the existing metadata
func readClaudeCodeKeychainPayload(env PoolEnv) map[string]any {
	if runtime.GOOS != "darwin" {
		return nil
	}
	stdout, _, code, err := runSecurity([]string{"find-generic-password", "-s", claudeCodeKeychainService, "-w"}, "", 5*time.Second)
	if err != nil {
		env.log().Debug("Keychain: security command not available or timed out")
		return nil
	}
	if code != 0 {
		env.log().Debug(fmt.Sprintf("Keychain: no entry found for %q", claudeCodeKeychainService))
		return nil
	}
	raw := strings.TrimSpace(stdout)
	if raw == "" {
		return nil
	}
	var payload map[string]any
	if json.Unmarshal([]byte(raw), &payload) != nil {
		env.log().Debug("Keychain: credentials payload is not valid JSON")
		return nil
	}
	return payload
}

// Command line for security -i that updates the Keychain item with the payload hex encoded
func keychainMirrorCommand(account string, payload map[string]any) ([]string, string) {
	quoted := func(v string) string {
		return `"` + strings.ReplaceAll(strings.ReplaceAll(v, `\`, `\\`), `"`, `\"`) + `"`
	}
	raw, _ := json.Marshal(payload)
	line := fmt.Sprintf("add-generic-password -U -a %s -s %s -X %s\n", quoted(account), quoted(claudeCodeKeychainService), hex.EncodeToString(raw))
	return []string{"security", "-i"}, line
}

func readClaudeCodeCredentialsFromKeychain(env PoolEnv) map[string]any {
	payload := readClaudeCodeKeychainPayload(env)
	if payload == nil {
		return nil
	}
	return claudeOAuthRecord(payload, "macos_keychain")
}

// ClaudeCodeCredentialsPath is Claude Code's shared OAuth file honoring CLAUDE_CONFIG_DIR
func ClaudeCodeCredentialsPath() string {
	override := strings.TrimSpace(os.Getenv("CLAUDE_CONFIG_DIR"))
	root := ""
	if override != "" {
		root = expandTildePath(override)
	} else {
		userHome, err := os.UserHomeDir()
		if err != nil {
			userHome = os.Getenv("HOME")
		}
		root = filepath.Join(userHome, ".claude")
	}
	return filepath.Join(root, ".credentials.json")
}

func expandTildePath(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		if userHome, err := os.UserHomeDir(); err == nil {
			return filepath.Join(userHome, strings.TrimPrefix(p, "~"))
		}
	}
	return p
}

func readClaudeCodeCredentialsFromFile(env PoolEnv) map[string]any {
	data := loadJSONIfExists(env, ClaudeCodeCredentialsPath(), "~/.claude/.credentials.json")
	if data == nil {
		return nil
	}
	return claudeOAuthRecord(data, "claude_code_credentials_file")
}

// ReadClaudeCodeCredentials reads the borrowed login from Keychain and file, preferring the only valid one
func ReadClaudeCodeCredentials(env PoolEnv) map[string]any {
	if !AdoptExternalLoginsEnabled(env) {
		return nil
	}
	kc := readClaudeCodeCredentialsFromKeychain(env)
	file := readClaudeCodeCredentialsFromFile(env)
	if kc == nil || file == nil {
		if kc != nil {
			return kc
		}
		return file
	}
	kcValid, fileValid := IsClaudeCodeTokenValid(kc), IsClaudeCodeTokenValid(file)
	if kcValid != fileValid {
		if kcValid {
			return kc
		}
		return file
	}
	kcExp, _ := anyFloat(kc["expiresAt"])
	fileExp, _ := anyFloat(file["expiresAt"])
	if kcExp >= fileExp {
		return kc
	}
	return file
}

// IsClaudeCodeTokenValid is a non expired access token with a 60s buffer, a missing expiry means managed key
func IsClaudeCodeTokenValid(creds map[string]any) bool {
	expires, _ := anyFloat(creds["expiresAt"])
	if expires != 0 {
		return float64(nowFunc().UnixMilli()) < expires-60_000
	}
	return anyString(creds["accessToken"]) != ""
}

// AnthropicOAuthError is a token endpoint rejection carrying the OAuth error code
type AnthropicOAuthError struct {
	Status      int
	Code        string
	Description string
	What        string
}

func (e *AnthropicOAuthError) Error() string {
	code := e.Code
	if code == "" {
		code = "error"
	}
	detail := ""
	if e.Description != "" {
		detail = " (" + e.Description + ")"
	}
	return fmt.Sprintf("Anthropic token %s failed: HTTP %d %s%s", e.What, e.Status, code, detail)
}

// ReloginRequired is a 400 or 401 whose code says the grant is dead
func (e *AnthropicOAuthError) ReloginRequired() bool {
	return (e.Status == 400 || e.Status == 401) && oauthGrantDeadCodes[e.Code]
}

// IsTerminalAnthropicRefreshError is true when retrying the same refresh token cannot succeed
func IsTerminalAnthropicRefreshError(err error) bool {
	var oe *AnthropicOAuthError
	return errors.As(err, &oe) && oe.ReloginRequired()
}

func (env PoolEnv) httpClient() *http.Client {
	if env.HTTP != nil {
		return env.HTTP
	}
	return http.DefaultClient
}

// POSTs to the token endpoints in order, returning the last error when all fail
func postAnthropicOAuthToken(env PoolEnv, data []byte, contentType string, timeout time.Duration, what, userAgent string) (map[string]any, error) {
	var lastErr error
	for _, endpoint := range anthropicOAuthTokenURLs {
		ctx, cancel := context.WithTimeout(env.ctx(), timeout)
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(data))
		if err != nil {
			cancel()
			lastErr = err
			continue
		}
		req.Header.Set("Content-Type", contentType)
		req.Header.Set("User-Agent", userAgent)
		resp, err := env.httpClient().Do(req)
		if err != nil {
			cancel()
			lastErr = err
			env.log().Debug(fmt.Sprintf("Anthropic token %s failed at %s: %v", what, endpoint, err))
			continue
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		cancel()
		if resp.StatusCode >= 400 {
			oe := anthropicOAuthHTTPError(resp.StatusCode, body, what)
			lastErr = oe
			env.log().Debug(fmt.Sprintf("Anthropic token %s failed at %s: %v", what, endpoint, oe))
			if oe.ReloginRequired() {
				break
			}
			continue
		}
		var result map[string]any
		if err := json.Unmarshal(body, &result); err != nil {
			lastErr = err
			env.log().Debug(fmt.Sprintf("Anthropic token %s failed at %s: %v", what, endpoint, err))
			continue
		}
		return result, nil
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("Anthropic token %s failed", what)
	}
	return nil, lastErr
}

// Structured rejection of a token endpoint error response, carrying the OAuth error code of its body when readable
func anthropicOAuthHTTPError(status int, body []byte, what string) *AnthropicOAuthError {
	var payload map[string]any
	_ = json.Unmarshal(body, &payload)
	return &AnthropicOAuthError{Status: status, Code: anyString(payload["error"]), Description: anyString(payload["error_description"]), What: what}
}

// Token endpoint JSON to the access refresh and expires_at_ms state with a 3600s default
func oauthTokenState(result map[string]any, fallbackRefresh string) map[string]any {
	expiresIn, ok := anyFloat(result["expires_in"])
	if !ok {
		expiresIn = 3600
	}
	refresh := anyString(result["refresh_token"])
	if _, has := result["refresh_token"]; !has {
		refresh = fallbackRefresh
	}
	return map[string]any{"access_token": anyString(result["access_token"]), "refresh_token": refresh,
		"expires_at_ms": float64(nowFunc().UnixMilli()) + expiresIn*1000}
}

// RefreshAnthropicOAuthPure refreshes an Anthropic OAuth token without touching local credential files
func RefreshAnthropicOAuthPure(env PoolEnv, refreshToken string, useJSON bool) (map[string]any, error) {
	if refreshToken == "" {
		return nil, errors.New("refresh_token is required")
	}
	payload := map[string]string{"grant_type": "refresh_token", "refresh_token": refreshToken, "client_id": anthropicOAuthClientID}
	var data []byte
	contentType := "application/x-www-form-urlencoded"
	if useJSON {
		data, _ = json.Marshal(payload)
		contentType = "application/json"
	} else {
		form := url.Values{}
		for k, v := range payload {
			form.Set(k, v)
		}
		data = []byte(form.Encode())
	}
	result, err := postAnthropicOAuthToken(env, data, contentType, 10*time.Second, "refresh", anthropicOAuthTokenUserAgent)
	if err != nil {
		return nil, err
	}
	if anyString(result["access_token"]) == "" {
		return nil, errors.New("Anthropic refresh response was missing access_token")
	}
	return oauthTokenState(result, refreshToken), nil
}

// Claude Code credential read, pure OAuth refresh and credential write, swapped by tests the way hermes tests patch them
var (
	readClaudeCodeCredentialsFn  = ReadClaudeCodeCredentials
	refreshAnthropicOAuthPureFn  = RefreshAnthropicOAuthPure
	writeClaudeCodeCredentialsFn = WriteClaudeCodeCredentials
)

// Refreshes an expired Claude Code token under the path keyed lock, adopting an already rotated one first
func refreshClaudeCodeOAuthToken(env PoolEnv, creds map[string]any) string {
	refreshTimeout := envFloat("ICEBATH_ANTHROPIC_REFRESH_TIMEOUT_SECONDS", 20)
	lockTimeout := time.Duration((refreshTimeout + 5) * float64(time.Second))
	if lockTimeout < AuthLockTimeout {
		lockTimeout = AuthLockTimeout
	}
	credPath := ClaudeCodeCredentialsPath()
	unlock, err := env.Store.fileLock(credPath, lockTimeout)
	if err != nil {
		env.log().Debug(fmt.Sprintf("Failed to acquire Claude Code refresh lock: %v", err))
		return ""
	}
	defer unlock()
	current := readClaudeCodeCredentialsFn(env)
	if current == nil {
		current = map[string]any{}
	}
	currentToken := anyString(current["accessToken"])
	curExp, _ := anyFloat(current["expiresAt"])
	if currentToken != "" && currentToken != anyString(creds["accessToken"]) && curExp > 0 && IsClaudeCodeTokenValid(current) {
		env.log().Debug("Adopted Claude Code's already-refreshed OAuth token")
		return currentToken
	}
	refreshToken := anyString(current["refreshToken"])
	if refreshToken == "" {
		refreshToken = anyString(creds["refreshToken"])
	}
	if refreshToken == "" {
		env.log().Debug("No refresh token available — cannot refresh")
		return ""
	}
	if IsRotationConsumedUncommitted(refreshToken, credPath) {
		env.log().Debug("Refresh token was already consumed by an uncommitted rotation - refusing to replay it; run 'icebath auth add anthropic'")
		return ""
	}
	sum := sha256.Sum256([]byte(refreshToken))
	fingerprint := hex.EncodeToString(sum[:])[:32]
	spentRotationMu.Lock()
	dead := deadRefreshTokenPrints[fingerprint]
	spentRotationMu.Unlock()
	if dead {
		env.log().Debug("Claude Code refresh token was already rejected as terminally invalid - not replaying it")
		return ""
	}
	refreshed, err := refreshAnthropicOAuthPureFn(env, refreshToken, false)
	if err != nil {
		if IsTerminalAnthropicRefreshError(err) {
			spentRotationMu.Lock()
			deadRefreshTokenPrints[fingerprint] = true
			spentRotationMu.Unlock()
			env.log().Warn(fmt.Sprintf("Claude Code OAuth refresh token is terminally invalid (%v); Icebath cannot use this login. Run 'icebath auth add anthropic' to give Icebath its own login.", err))
		} else {
			env.log().Debug(fmt.Sprintf("Failed to refresh Claude Code token: %v", err))
		}
		return ""
	}
	ms, _ := anyFloat(refreshed["expires_at_ms"])
	if err := writeClaudeCodeCredentialsFn(env, anyString(refreshed["access_token"]), anyString(refreshed["refresh_token"]), int64(ms), nil, refreshToken); err != nil {
		env.log().Error(fmt.Sprintf("Anthropic OAuth refresh rotated the single-use token but could not commit it to %s (%v) — treating the refresh as failed; run 'icebath auth add anthropic' to give Icebath its own login", credPath, err))
		MarkRotationConsumedUncommitted(credPath, refreshToken, anyString(creds["accessToken"]), anyString(current["accessToken"]), anyString(current["refreshToken"]))
		return ""
	}
	env.log().Debug("Successfully refreshed Claude Code OAuth token")
	return anyString(refreshed["access_token"])
}

// WriteClaudeCodeCredentials commits refreshed credentials to the Claude Code file and mirrors them into the Keychain
func WriteClaudeCodeCredentials(env PoolEnv, accessToken, refreshToken string, expiresAtMS int64, scopes []any, spentRefreshToken string) error {
	credPath := ClaudeCodeCredentialsPath()
	existing := map[string]any{}
	if raw, err := os.ReadFile(credPath); err == nil {
		if err := json.Unmarshal(bytes.TrimPrefix(raw, []byte("\xef\xbb\xbf")), &existing); err != nil {
			env.log().Error(fmt.Sprintf("Failed to write refreshed credentials to %s: %v", credPath, err))
			return &CredentialPersistError{Path: credPath, Cause: err}
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		env.log().Error(fmt.Sprintf("Failed to write refreshed credentials to %s: %v", credPath, err))
		return &CredentialPersistError{Path: credPath, Cause: err}
	}
	oauth := map[string]any{"accessToken": accessToken, "refreshToken": refreshToken, "expiresAt": expiresAtMS}
	if scopes != nil {
		oauth["scopes"] = scopes
	} else if prev, ok := existing["claudeAiOauth"].(map[string]any); ok {
		if s, has := prev["scopes"]; has {
			oauth["scopes"] = s
		}
	}
	existing["claudeAiOauth"] = oauth
	if err := commitPrivateJSON(env, credPath, existing, "credentials"); err != nil {
		return err
	}
	mirrorClaudeCodeCredentialsToKeychain(env, accessToken, refreshToken, expiresAtMS, spentRefreshToken)
	return nil
}

// Rotates the token triple over the existing Keychain payload keeping its metadata
func mergeKeychainCredentialPayload(existing map[string]any, accessToken, refreshToken string, expiresAtMS int64) map[string]any {
	merged := cloneMap(existing)
	oauth := map[string]any{}
	if prev, ok := existing["claudeAiOauth"].(map[string]any); ok {
		oauth = cloneMap(prev)
	}
	oauth["accessToken"], oauth["refreshToken"], oauth["expiresAt"] = accessToken, refreshToken, expiresAtMS
	merged["claudeAiOauth"] = oauth
	return merged
}

// Writes the rotated pair into the Claude Code Keychain item that held the pair just spent
func mirrorClaudeCodeCredentialsToKeychain(env PoolEnv, accessToken, refreshToken string, expiresAtMS int64, spentRefreshToken string) {
	if runtime.GOOS != "darwin" {
		return
	}
	account, existing, ok := findClaudeCodeKeychainItem()
	if !ok {
		return
	}
	oauth, isMap := existing["claudeAiOauth"].(map[string]any)
	if !isMap || anyString(oauth["refreshToken"]) != spentRefreshToken {
		env.log().Debug("Keychain mirror skipped: item does not hold the pair that was just rotated")
		return
	}
	argv, line := keychainMirrorCommand(account, mergeKeychainCredentialPayload(existing, accessToken, refreshToken, expiresAtMS))
	_, stderr, code, err := runSecurity(argv[1:], line, 10*time.Second)
	if err != nil {
		env.log().Debug(fmt.Sprintf("Keychain mirror skipped (%v)", err))
		return
	}
	if code != 0 {
		msg := strings.TrimSpace(stderr)
		if len(msg) > 200 {
			msg = msg[:200]
		}
		env.log().Debug(fmt.Sprintf("Keychain mirror failed (rc=%d): %s", code, msg))
	}
}

// Token from the Claude Code credential files, refreshing when expired
func resolveClaudeCodeTokenFromCredentials(env PoolEnv, creds map[string]any) string {
	if creds == nil {
		creds = ReadClaudeCodeCredentials(env)
	}
	if creds == nil {
		return ""
	}
	if IsRotationConsumedUncommitted(anyString(creds["accessToken"]), ClaudeCodeCredentialsPath()) {
		env.log().Debug("Claude Code credentials hold a rotated-but-uncommitted token - refusing")
		return ""
	}
	if IsClaudeCodeTokenValid(creds) {
		env.log().Debug("Using Claude Code credentials (auto-detected)")
		return anyString(creds["accessToken"])
	}
	env.log().Debug("Claude Code credentials expired — attempting refresh")
	refreshed := refreshClaudeCodeOAuthToken(env, creds)
	if refreshed == "" {
		env.log().Debug("Token refresh failed — run 'icebath auth add anthropic' to give Icebath its own login")
	}
	return refreshed
}

// Prefers refreshable Claude Code credentials over a static env OAuth token
func preferRefreshableClaudeCodeToken(env PoolEnv, envToken string, creds map[string]any) string {
	if envToken == "" || !IsAnthropicOAuthToken(envToken) || creds == nil || anyString(creds["refreshToken"]) == "" {
		return ""
	}
	resolved := resolveClaudeCodeTokenFromCredentials(env, creds)
	if resolved != "" && resolved != envToken {
		env.log().Debug("Preferring Claude Code credential file over static env OAuth token so refresh can proceed")
		return resolved
	}
	return ""
}

// First available Anthropic OAuth token from the pool, read only and never selecting
func resolveAnthropicPoolToken(env PoolEnv, skipBorrowed bool) string {
	pool := LoadPool(env, "anthropic")
	pool.mu.Lock()
	entries, _ := pool.availableEntries(false, false, "")
	pool.mu.Unlock()
	for _, entry := range entries {
		if skipBorrowed && entry.Source == "claude_code" {
			continue
		}
		token := strings.TrimSpace(entry.AccessToken)
		if entry.AuthType != AuthTypeOAuth || token == "" {
			continue
		}
		sourcePath := SpentRotationSourcePath(env, entry.Source)
		if IsRotationConsumedUncommitted(token, sourcePath) || IsRotationConsumedUncommitted(entry.RefreshToken, sourcePath) {
			env.log().Debug(fmt.Sprintf("Skipping Anthropic pool entry %s: rotated-but-uncommitted credential", entry.ID))
			continue
		}
		return token
	}
	return ""
}

// The token unless the pool holds an active cooldown for it on model
func availableAnthropicToken(env PoolEnv, token, model string) string {
	if token == "" || model == "" {
		return token
	}
	if LoadPool(env, "anthropic").TokenIsBlocked(token, model) {
		return ""
	}
	return token
}

// ResolveAnthropicToken resolves an Anthropic token from env, pool and the borrowed Claude Code login in order
func ResolveAnthropicToken(env PoolEnv, model string) string {
	var cached map[string]any
	readOnce := false
	readCreds := func() map[string]any {
		if !readOnce {
			cached, readOnce = ReadClaudeCodeCredentials(env), true
		}
		return cached
	}
	firstEnv := func(names ...string) string {
		for _, n := range names {
			if v := strings.TrimSpace(env.secretStr(n)); v != "" {
				return v
			}
		}
		return ""
	}
	if token := firstEnv("ANTHROPIC_TOKEN", "CLAUDE_CODE_OAUTH_TOKEN"); token != "" {
		if preferred := preferRefreshableClaudeCodeToken(env, token, readCreds()); preferred != "" {
			token = preferred
		}
		return availableAnthropicToken(env, token, model)
	}
	if apiKey := firstEnv("ANTHROPIC_API_KEY"); apiKey != "" {
		return availableAnthropicToken(env, apiKey, model)
	}
	token := resolveAnthropicPoolToken(env, true)
	if token == "" {
		token = resolveClaudeCodeTokenFromCredentials(env, readCreds())
	}
	return availableAnthropicToken(env, token, model)
}

// FileNotFoundError of a missing claude CLI so the setup flow guides a manual install
type claudeCLIMissingError struct{}

func (claudeCLIMissingError) Error() string {
	return "The 'claude' CLI is not installed. Install it with: npm install -g @anthropic-ai/claude-code"
}

func (claudeCLIMissingError) Is(target error) bool { return target == fs.ErrNotExist }

// RunOAuthSetupToken runs claude setup-token interactively and returns the resulting token
func RunOAuthSetupToken(env PoolEnv) (string, error) {
	claudePath := findClaudeCodeCLI("claude")
	if claudePath == "" {
		return "", claudeCLIMissingError{}
	}
	cmd := exec.CommandContext(env.ctx(), claudePath, "setup-token")
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		if errors.Is(err, context.Canceled) {
			return "", nil
		}
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) {
			return "", err
		}
	}
	if creds := ReadClaudeCodeCredentials(env); creds != nil && IsClaudeCodeTokenValid(creds) {
		return anyString(creds["accessToken"]), nil
	}
	for _, n := range []string{"CLAUDE_CODE_OAUTH_TOKEN", "ANTHROPIC_TOKEN"} {
		if v := strings.TrimSpace(env.secretStr(n)); v != "" {
			return v, nil
		}
	}
	return "", nil
}

// HermesOAuthFile is the icebath owned Anthropic PKCE credential file of the home
func HermesOAuthFile(env PoolEnv) string {
	return filepath.Join(env.Store.Home(), ".anthropic_oauth.json")
}

// RootHermesOAuthFile is the global root PKCE file inside a named profile, empty in classic mode
func RootHermesOAuthFile(env PoolEnv) string {
	root := home.RootOf(env.Store.Home())
	if samePath(root, env.Store.Home()) {
		return ""
	}
	return filepath.Join(root, ".anthropic_oauth.json")
}

func generatePKCE() (string, string) {
	var raw [32]byte
	fillRandom(raw[:])
	verifier := base64.RawURLEncoding.EncodeToString(raw[:])
	sum := sha256.Sum256([]byte(verifier))
	return verifier, base64.RawURLEncoding.EncodeToString(sum[:])
}

// RunHermesOAuthLoginPure runs the icebath native PKCE flow and returns the credential state
func RunHermesOAuthLoginPure(env PoolEnv, prompt func(label string) (string, bool)) map[string]any {
	verifier, challenge := generatePKCE()
	var stateRaw [32]byte
	fillRandom(stateRaw[:])
	oauthState := base64.RawURLEncoding.EncodeToString(stateRaw[:])
	params := url.Values{}
	params.Set("code", "true")
	params.Set("client_id", anthropicOAuthClientID)
	params.Set("response_type", "code")
	params.Set("redirect_uri", anthropicOAuthRedirectURI)
	params.Set("scope", anthropicOAuthScopes)
	params.Set("code_challenge", challenge)
	params.Set("code_challenge_method", "S256")
	params.Set("state", oauthState)
	authURL := "https://claude.ai/oauth/authorize?" + params.Encode()
	fmt.Println(strings.Join([]string{
		"", "Authorize Icebath with your Claude Pro/Max subscription.", "",
		"╭─ Claude Pro/Max Authorization ────────────────────╮",
		"│                                                   │",
		"│  Open this link in your browser:                  │",
		"╰───────────────────────────────────────────────────╯",
		"", "  " + authURL, "",
	}, "\n"))
	if canOpenGraphicalBrowser() {
		if openBrowserURL(authURL) == nil {
			fmt.Println("  (Browser opened automatically)")
		}
	}
	fmt.Print("\nAfter authorizing, you'll see a code. Paste it below.\n\n")
	authCode, ok := prompt("Authorization code: ")
	if !ok {
		return nil
	}
	authCode = strings.TrimSpace(authCode)
	if authCode == "" {
		fmt.Println("No code entered.")
		return nil
	}
	code, receivedState, _ := strings.Cut(authCode, "#")
	if receivedState != oauthState {
		env.log().Warn("OAuth state mismatch — possible CSRF, aborting")
		return nil
	}
	exchange, _ := json.Marshal(map[string]string{"grant_type": "authorization_code", "client_id": anthropicOAuthClientID, "code": code,
		"state": receivedState, "redirect_uri": anthropicOAuthRedirectURI, "code_verifier": verifier})
	result, err := postAnthropicOAuthToken(env, exchange, "application/json", 15*time.Second, "exchange", anthropicOAuthTokenUserAgent)
	if err != nil {
		fmt.Printf("Token exchange failed: %v\n", err)
		return nil
	}
	if anyString(result["access_token"]) == "" {
		fmt.Println("No access token in response.")
		return nil
	}
	return oauthTokenState(result, "")
}

// ReadHermesOAuthCredentials reads the icebath managed PKCE credentials of the home
func ReadHermesOAuthCredentials(env PoolEnv) map[string]any {
	data := loadJSONIfExists(env, HermesOAuthFile(env), "Icebath OAuth credentials")
	if data == nil || anyString(data["accessToken"]) == "" {
		return nil
	}
	return data
}

// WriteHermesOAuthCredentials commits refreshed PKCE tokens to the home file or the borrowed root target
func WriteHermesOAuthCredentials(env PoolEnv, accessToken, refreshToken string, expiresAtMS int64, target string) error {
	if target == "" {
		target = HermesOAuthFile(env)
	}
	return commitPrivateJSON(env, target, map[string]any{"accessToken": accessToken, "refreshToken": refreshToken, "expiresAt": expiresAtMS}, "Icebath OAuth credentials")
}

// Opens a URL in the user's default browser through the platform opener
func openBrowserURL(target string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", target)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", target)
	default:
		cmd = exec.Command("xdg-open", target)
	}
	return cmd.Start()
}

// Opens a URL in the default browser through the platform opener for callers outside the package
func OpenBrowserURL(target string) error { return openBrowserURL(target) }

// Fills the buffer from the OS entropy source, panicking when the kernel refuses
func fillRandom(buf []byte) {
	if _, err := rand.Read(buf); err != nil {
		panic(err)
	}
}
