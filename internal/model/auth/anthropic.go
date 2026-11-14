package auth

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/nickheyer/icebath/internal/fileutil"
	"github.com/nickheyer/icebath/internal/home"
)

const (
	anthropicOAuthClientID    = "9d1c250a-e61b-44d9-88ed-5944d1962f5e"
	anthropicTokenUserAgent   = "axios/1.7.9"
	anthropicOAuthRedirectURI = "https://console.anthropic.com/oauth/code/callback"
	anthropicOAuthScopes      = "org:create_api_key user:profile user:inference"
	claudeCodeKeychainService = "Claude Code-credentials"
	spentRotationMaxTracked   = 64
	sourceClaudeCode          = "claude_code"
)

var anthropicOAuthTokenURLs = []string{
	"https://platform.claude.com/v1/oauth/token", "https://console.anthropic.com/v1/oauth/token",
}

func IsAnthropicOAuthToken(key string) bool {
	if key == "" {
		return false
	}
	for _, p := range []string{"sk-ant-api", "sk-ant-usr", "sk-ant-svc", "sk-ant-admin"} {
		if strings.HasPrefix(key, p) {
			return false
		}
	}
	return strings.HasPrefix(key, "sk-ant-") || strings.HasPrefix(key, "eyJ") || strings.HasPrefix(key, "cc-")
}

func IsNativeAnthropicHost(baseURL string) bool {
	text := strings.TrimSpace(baseURL)
	if text == "" {
		return true
	}
	if !strings.Contains(text, "://") {
		text = "//" + text
	}
	parsed, err := url.Parse(text)
	return err == nil && strings.ToLower(parsed.Hostname()) == "api.anthropic.com"
}

func AnthropicRouteIsOAuth(ctx context.Context, baseURL string, c Credential, provider string) bool {
	if !IsNativeAnthropicHost(baseURL) && strings.ToLower(strings.TrimSpace(provider)) != ProviderAnthropic {
		return false
	}
	key := c.Key.Reveal()
	if c.Tokens != nil {
		token, err := c.Tokens.Token(ctx)
		if err != nil {
			return false
		}
		key = token.Value.Reveal()
	}
	return IsAnthropicOAuthToken(key)
}

type CredentialPersistError struct {
	Path  string
	Cause error
}

func (e *CredentialPersistError) Error() string {
	return fmt.Sprintf("failed to durably persist rotated Anthropic credentials to %s: %v", e.Path, e.Cause)
}

func (e *CredentialPersistError) Unwrap() error { return e.Cause }

func loadJSONIfExists(path, what string) *doc {
	data, err := os.ReadFile(path)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			slog.Debug(fmt.Sprintf("Failed to read %s", what), "error", err)
		}
		return nil
	}
	parsed, err := parseObject(bytes.TrimPrefix(data, utf8BOM))
	if err != nil {
		slog.Debug(fmt.Sprintf("Failed to read %s", what), "error", err)
		return nil
	}
	return parsed
}

var utf8BOM = []byte("\xef\xbb\xbf")

func commitPrivateJSON(ctx context.Context, path string, payload *doc, what string) error {
	base := filepath.Base(path)
	err := fileutil.WriteJSON(ctx, path, payload, fileutil.JSONStyle{Indent: 2}, fileutil.Private(),
		fileutil.TempPrefix("."+strings.TrimSuffix(base, filepath.Ext(base))+"_"))
	if err != nil {
		slog.Error(fmt.Sprintf("Failed to write refreshed %s to %s", what, path), "error", err)
		return &CredentialPersistError{Path: path, Cause: err}
	}
	return nil
}

const spentRotationSidecarComment = "Non-secret one-way fingerprints of Anthropic OAuth credentials whose rotation was " +
	"consumed server-side but never durably committed. Written by icebath so sibling " +
	"processes sharing this credential source fail closed instead of replaying a spent " +
	"single-use refresh token."

func spentRotationSidecarPath(sourcePath string) string {
	return sourcePath + ".icebath-spent-rotations.json"
}

func (s *Store) claudeCodeCredentialsPath(ctx context.Context) string {
	if override := strings.TrimSpace(s.lookupEnv(ctx, "CLAUDE_CONFIG_DIR")); override != "" {
		return filepath.Join(home.ExpandUser(override), ".credentials.json")
	}
	userHome, err := home.UserHome()
	if err != nil {
		userHome = "~"
	}
	return filepath.Join(userHome, ".claude", ".credentials.json")
}

func (s *Store) spentRotationSourcePath(ctx context.Context, source string) string {
	switch source {
	case sourceClaudeCode:
		return s.claudeCodeCredentialsPath(ctx)
	case sourceIcebathPKCE:
		return home.AnthropicOAuth.In(s.home)
	}
	return ""
}

func readSpentRotationSidecar(sourcePath string) []string {
	if sourcePath == "" {
		return nil
	}
	data, err := os.ReadFile(spentRotationSidecarPath(sourcePath))
	if err != nil {
		return nil
	}
	raw, err := parseObject(bytes.TrimPrefix(data, utf8BOM))
	if err != nil {
		return nil
	}
	out := []string{}
	for _, v := range raw.list("fingerprints") {
		if fp, ok := v.(string); ok && fp != "" && !slices.Contains(out, fp) {
			out = append(out, fp)
		}
	}
	return out
}

func appendSpentRotationSidecar(ctx context.Context, sourcePath string, fingerprints []string) {
	sidecar := spentRotationSidecarPath(sourcePath)
	merged := readSpentRotationSidecar(sourcePath)
	for _, fp := range fingerprints {
		if !slices.Contains(merged, fp) {
			merged = append(merged, fp)
		}
	}
	sort.Strings(merged)
	if limit := spentRotationMaxTracked * 4; len(merged) > limit {
		merged = merged[len(merged)-limit:]
	}
	list := make([]any, 0, len(merged))
	for _, fp := range merged {
		list = append(list, fp)
	}
	payload := newDoc()
	payload.set("version", intNum(1))
	payload.set("comment", spentRotationSidecarComment)
	payload.set("fingerprints", list)
	if err := os.MkdirAll(filepath.Dir(sidecar), 0o755); err != nil {
		slog.Debug(fmt.Sprintf("Failed to persist spent-rotation fingerprints to %s", sidecar), "error", err)
		return
	}
	if err := fileutil.WriteJSON(ctx, sidecar, payload, fileutil.JSONStyle{Indent: 2}); err != nil {
		slog.Debug(fmt.Sprintf("Failed to persist spent-rotation fingerprints to %s", sidecar), "error", err)
	}
}

func (s *Store) markRotationConsumedUncommitted(ctx context.Context, sourcePath string, secrets ...string) {
	recorded := []string{}
	for _, secret := range secrets {
		if fp := FingerprintSecret(strings.TrimSpace(secret)); fp != "" {
			recorded = append(recorded, fp)
		}
	}
	s.mu.Lock()
	for _, fp := range recorded {
		s.spentRotations = slices.DeleteFunc(s.spentRotations, func(v string) bool { return v == fp })
		s.spentRotations = append(s.spentRotations, fp)
		if len(s.spentRotations) > spentRotationMaxTracked {
			s.spentRotations = s.spentRotations[len(s.spentRotations)-spentRotationMaxTracked:]
		}
	}
	s.mu.Unlock()
	if len(recorded) > 0 && sourcePath != "" {
		appendSpentRotationSidecar(ctx, sourcePath, recorded)
	}
}

func (s *Store) isRotationConsumedUncommitted(secret, sourcePath string) bool {
	fp := FingerprintSecret(strings.TrimSpace(secret))
	if fp == "" {
		return false
	}
	s.mu.Lock()
	known := slices.Contains(s.spentRotations, fp)
	s.mu.Unlock()
	return known || slices.Contains(readSpentRotationSidecar(sourcePath), fp)
}

type claudeCreds struct {
	accessToken  string
	refreshToken string
	expiresAt    int64
	source       string
}

func claudeOAuthRecord(data *doc, source string) *claudeCreds {
	oauth := data.obj("claudeAiOauth")
	access := oauth.str("accessToken")
	if access == "" {
		return nil
	}
	expires, _ := intOf(oauth.value("expiresAt"))
	return &claudeCreds{accessToken: access, refreshToken: oauth.str("refreshToken"), expiresAt: expires, source: source}
}

func (s *Store) readClaudeCodeKeychainPayload(ctx context.Context) *doc {
	if runtime.GOOS != "darwin" {
		return nil
	}
	callCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	raw, err := s.keychain.ReadForeign(callCtx, claudeCodeKeychainService)
	if err != nil || strings.TrimSpace(raw) == "" {
		slog.Debug("Keychain: no readable Claude Code entry", "error", err)
		return nil
	}
	payload, err := parseObject([]byte(strings.TrimSpace(raw)))
	if err != nil {
		slog.Debug("Keychain: credentials payload is not valid JSON")
		return nil
	}
	return payload
}

func (s *Store) readClaudeCodeCredentials(ctx context.Context) *claudeCreds {
	if !s.adoptExternalLogins(ctx) {
		return nil
	}
	var keychain, file *claudeCreds
	if payload := s.readClaudeCodeKeychainPayload(ctx); payload != nil {
		keychain = claudeOAuthRecord(payload, "macos_keychain")
	}
	if data := loadJSONIfExists(s.claudeCodeCredentialsPath(ctx), "~/.claude/.credentials.json"); data != nil {
		file = claudeOAuthRecord(data, "claude_code_credentials_file")
	}
	if keychain == nil || file == nil {
		if keychain != nil {
			return keychain
		}
		return file
	}
	kcValid, fileValid := claudeCodeTokenValid(keychain), claudeCodeTokenValid(file)
	if kcValid != fileValid {
		if kcValid {
			return keychain
		}
		return file
	}
	if keychain.expiresAt >= file.expiresAt {
		return keychain
	}
	return file
}

func claudeCodeTokenValid(c *claudeCreds) bool {
	if c.expiresAt != 0 {
		return time.Now().UnixMilli() < c.expiresAt-60_000
	}
	return c.accessToken != ""
}

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

func (e *AnthropicOAuthError) ReloginRequired() bool {
	return (e.Status == 400 || e.Status == 401) && slices.Contains(oauthGrantDeadCodes, e.Code)
}

func IsTerminalAnthropicRefreshError(err error) bool {
	var oe *AnthropicOAuthError
	return errors.As(err, &oe) && oe.ReloginRequired()
}

func (s *Store) postAnthropicToken(ctx context.Context, data []byte, contentType string, timeout time.Duration, what string) (*doc, error) {
	var last error
	for _, endpoint := range anthropicOAuthTokenURLs {
		r, err := s.exchange(ctx, nil, http.MethodPost, endpoint, map[string]string{"Content-Type": contentType, "User-Agent": anthropicTokenUserAgent}, data, timeout)
		if err != nil {
			last = err
			slog.Debug(fmt.Sprintf("Anthropic token %s failed at %s", what, endpoint), "error", transportFailure(err))
			continue
		}
		if r.status >= 400 {
			oe := &AnthropicOAuthError{Status: r.status, What: what}
			if payload, perr := r.object(); perr == nil {
				oe.Code, oe.Description = textOf(payload.value("error")), textOf(payload.value("error_description"))
			}
			last = oe
			slog.Debug(fmt.Sprintf("Anthropic token %s failed at %s: %v", what, endpoint, oe))
			if oe.ReloginRequired() {
				break
			}
			continue
		}
		payload, perr := r.object()
		if perr != nil {
			last = perr
			continue
		}
		return payload, nil
	}
	if last == nil {
		last = fmt.Errorf("Anthropic token %s failed", what)
	}
	return nil, last
}

type AnthropicTokens struct {
	AccessToken  Secret
	RefreshToken Secret
	ExpiresAtMS  int64
}

func anthropicTokenState(result *doc, fallbackRefresh string) AnthropicTokens {
	expiresIn := int64(3600)
	if result.has("expires_in") {
		expiresIn, _ = intOf(result.value("expires_in"))
	}
	refresh := fallbackRefresh
	if result.has("refresh_token") {
		refresh = result.str("refresh_token")
	}
	return AnthropicTokens{
		AccessToken: NewSecret(result.str("access_token")), RefreshToken: NewSecret(refresh),
		ExpiresAtMS: time.Now().UnixMilli() + expiresIn*1000,
	}
}

func (s *Store) refreshAnthropicPure(ctx context.Context, refreshToken string, useJSON bool) (AnthropicTokens, error) {
	if refreshToken == "" {
		return AnthropicTokens{}, errors.New("refresh_token is required")
	}
	var body []byte
	contentType := "application/x-www-form-urlencoded"
	if useJSON {
		payload := newDoc()
		payload.set("grant_type", "refresh_token")
		payload.set("refresh_token", refreshToken)
		payload.set("client_id", anthropicOAuthClientID)
		body, _ = payload.MarshalJSON()
		contentType = "application/json"
	} else {
		body = []byte(encodeForm(formOf("grant_type", "refresh_token", "refresh_token", refreshToken, "client_id", anthropicOAuthClientID)))
	}
	result, err := s.postAnthropicToken(ctx, body, contentType, 10*time.Second, "refresh")
	if err != nil {
		return AnthropicTokens{}, err
	}
	if !truthy(result.value("access_token")) {
		return AnthropicTokens{}, errors.New("Anthropic refresh response was missing access_token")
	}
	return anthropicTokenState(result, refreshToken), nil
}

func (s *Store) RefreshAnthropicOAuth(ctx context.Context, refreshToken Secret, useJSON bool) (AnthropicTokens, error) {
	return s.refreshAnthropicPure(s.bind(ctx), refreshToken.Reveal(), useJSON)
}

func (s *Store) mirrorClaudeCodeToKeychain(ctx context.Context, access, refresh string, expiresAtMS int64, spentRefresh string) {
	if runtime.GOOS != "darwin" {
		return
	}
	callCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	account, raw, err := s.keychain.FindForeign(callCtx, claudeCodeKeychainService)
	if err != nil || account == "" || raw == "" {
		return
	}
	existing, err := parseObject([]byte(raw))
	if err != nil {
		return
	}
	oauth := existing.obj("claudeAiOauth")
	if oauth == nil || oauth.str("refreshToken") != spentRefresh {
		slog.Debug("Keychain mirror not applied: item does not hold the pair that was just rotated")
		return
	}
	merged := existing.clone()
	rotated := oauth.clone()
	rotated.set("accessToken", access)
	rotated.set("refreshToken", refresh)
	rotated.set("expiresAt", intNum(expiresAtMS))
	merged.set("claudeAiOauth", rotated)
	encoded, err := fileutil.EncodeJSON(merged, fileutil.JSONStyle{Tight: true, ASCII: true})
	if err != nil {
		slog.Debug("Keychain mirror not applied", "error", err)
		return
	}
	if err := s.keychain.UpdateForeign(callCtx, claudeCodeKeychainService, account, encoded); err != nil {
		slog.Debug("Keychain mirror failed", "error", err)
	}
}

func (s *Store) writeClaudeCodeCredentials(ctx context.Context, access, refresh string, expiresAtMS int64, scopes []any, spentRefresh string) error {
	path := s.claudeCodeCredentialsPath(ctx)
	existing := newDoc()
	if data, err := os.ReadFile(path); err == nil {
		parsed, perr := parseObject(bytes.TrimPrefix(data, utf8BOM))
		if perr != nil {
			slog.Error(fmt.Sprintf("Failed to write refreshed credentials to %s", path), "error", perr)
			return &CredentialPersistError{Path: path, Cause: perr}
		}
		existing = parsed
	} else if !errors.Is(err, os.ErrNotExist) {
		slog.Error(fmt.Sprintf("Failed to write refreshed credentials to %s", path), "error", err)
		return &CredentialPersistError{Path: path, Cause: err}
	}
	oauth := newDoc()
	oauth.set("accessToken", access)
	oauth.set("refreshToken", refresh)
	oauth.set("expiresAt", intNum(expiresAtMS))
	if scopes != nil {
		oauth.set("scopes", scopes)
	} else if prior := existing.obj("claudeAiOauth"); prior.has("scopes") {
		oauth.set("scopes", prior.value("scopes"))
	}
	existing.set("claudeAiOauth", oauth)
	if err := commitPrivateJSON(ctx, path, existing, "credentials"); err != nil {
		return err
	}
	s.mirrorClaudeCodeToKeychain(ctx, access, refresh, expiresAtMS, spentRefresh)
	return nil
}

func (s *Store) refreshClaudeCodeToken(ctx context.Context, creds *claudeCreds) string {
	timeout := seconds(s.envFloat(ctx, "ICEBATH_ANTHROPIC_REFRESH_TIMEOUT_SECONDS", 20))
	credPath := s.claudeCodeCredentialsPath(ctx)
	ctx, unlock, err := s.lock(ctx, credPath, max(LockTimeout, timeout+5*time.Second))
	if err != nil {
		slog.Debug("Failed to acquire Claude Code refresh lock", "error", err)
		return ""
	}
	defer unlock()
	current := s.readClaudeCodeCredentials(ctx)
	if current == nil {
		current = &claudeCreds{}
	}
	if current.accessToken != "" && current.accessToken != creds.accessToken && current.expiresAt > 0 && claudeCodeTokenValid(current) {
		slog.Debug("Adopted Claude Code's already-refreshed OAuth token")
		return current.accessToken
	}
	refreshToken := current.refreshToken
	if refreshToken == "" {
		refreshToken = creds.refreshToken
	}
	if refreshToken == "" {
		slog.Debug("No refresh token available — cannot refresh")
		return ""
	}
	if s.isRotationConsumedUncommitted(refreshToken, credPath) {
		slog.Debug("Refresh token was already consumed by an uncommitted rotation - refusing to replay it; run 'icebath auth add anthropic'")
		return ""
	}
	sum := sha256.Sum256([]byte(refreshToken))
	fingerprint := hex.EncodeToString(sum[:])[:32]
	s.mu.Lock()
	dead := slices.Contains(s.deadRefreshTokens, fingerprint)
	s.mu.Unlock()
	if dead {
		slog.Debug("Claude Code refresh token was already rejected as terminally invalid - not replaying it")
		return ""
	}
	refreshed, err := s.refreshAnthropicPure(ctx, refreshToken, false)
	if err != nil {
		if IsTerminalAnthropicRefreshError(err) {
			s.mu.Lock()
			s.deadRefreshTokens = append(s.deadRefreshTokens, fingerprint)
			s.mu.Unlock()
			slog.Warn(fmt.Sprintf("Claude Code OAuth refresh token is terminally invalid (%v); icebath cannot use this "+
				"login. Run 'icebath auth add anthropic' to give icebath its own login.", err))
		} else {
			slog.Debug("Failed to refresh Claude Code token", "error", transportFailure(err))
		}
		return ""
	}
	if err := s.writeClaudeCodeCredentials(ctx, refreshed.AccessToken.Reveal(), refreshed.RefreshToken.Reveal(), refreshed.ExpiresAtMS, nil, refreshToken); err != nil {
		slog.Error(fmt.Sprintf("Anthropic OAuth refresh rotated the single-use token but could not commit it to %s (%v) — "+
			"treating the refresh as failed; run 'icebath auth add anthropic' to give icebath its own login", credPath, err))
		s.markRotationConsumedUncommitted(ctx, credPath, refreshToken, creds.accessToken, current.accessToken, current.refreshToken)
		return ""
	}
	slog.Debug("Successfully refreshed Claude Code OAuth token")
	return refreshed.AccessToken.Reveal()
}

func (s *Store) resolveClaudeCodeToken(ctx context.Context, creds *claudeCreds) string {
	if creds == nil {
		creds = s.readClaudeCodeCredentials(ctx)
	}
	if creds == nil {
		return ""
	}
	if s.isRotationConsumedUncommitted(creds.accessToken, s.claudeCodeCredentialsPath(ctx)) {
		slog.Debug("Claude Code credentials hold a rotated-but-uncommitted token - refusing")
		return ""
	}
	if claudeCodeTokenValid(creds) {
		slog.Debug("Using Claude Code credentials (auto-detected)")
		return creds.accessToken
	}
	slog.Debug("Claude Code credentials expired — attempting refresh")
	refreshed := s.refreshClaudeCodeToken(ctx, creds)
	if refreshed == "" {
		slog.Debug("Token refresh failed — run 'icebath auth add anthropic' to give icebath its own login")
	}
	return refreshed
}

func (s *Store) firstEnv(ctx context.Context, names ...string) string {
	for _, name := range names {
		if v := strings.TrimSpace(s.lookupEnv(ctx, name)); v != "" {
			return v
		}
	}
	return ""
}

func (s *Store) resolveAnthropicPoolToken(ctx context.Context, skipBorrowed bool) string {
	pool, err := s.Pool(ctx, ProviderAnthropic)
	if err != nil {
		slog.Debug("Failed to read Anthropic credential_pool", "error", err)
		return ""
	}
	lctx, unlock := pool.lock(ctx)
	entries, _, err := pool.availableEntries(lctx, false, false, "")
	unlock()
	if err != nil {
		slog.Debug("Failed to read Anthropic credential_pool", "error", err)
		return ""
	}
	for _, entry := range entries {
		if skipBorrowed && entry.Source == sourceClaudeCode {
			continue
		}
		token := strings.TrimSpace(entry.AccessToken.Reveal())
		if entry.AuthType != AuthTypeOAuth || token == "" {
			continue
		}
		sourcePath := s.spentRotationSourcePath(ctx, entry.Source)
		if s.isRotationConsumedUncommitted(token, sourcePath) || s.isRotationConsumedUncommitted(entry.RefreshToken.Reveal(), sourcePath) {
			slog.Debug(fmt.Sprintf("Skipping Anthropic pool entry %s: rotated-but-uncommitted credential", entry.ID))
			continue
		}
		return token
	}
	return ""
}

func (s *Store) availableAnthropicToken(ctx context.Context, token, model string) (Secret, bool) {
	if token == "" {
		return Secret{}, false
	}
	if model == "" {
		return NewSecret(token), true
	}
	if pool, err := s.Pool(ctx, ProviderAnthropic); err == nil {
		if pool.TokenIsBlocked(NewSecret(token), model) {
			return Secret{}, false
		}
	} else {
		slog.Debug("Failed to check Anthropic model cooldown", "error", err)
	}
	return NewSecret(token), true
}

func (s *Store) ResolveAnthropicToken(ctx context.Context, model string) (Secret, bool) {
	ctx = s.bind(ctx)
	var creds *claudeCreds
	read := false
	readCreds := func() *claudeCreds {
		if !read {
			creds, read = s.readClaudeCodeCredentials(ctx), true
		}
		return creds
	}
	if token := s.firstEnv(ctx, "ANTHROPIC_TOKEN", "CLAUDE_CODE_OAUTH_TOKEN"); token != "" {
		if c := readCreds(); IsAnthropicOAuthToken(token) && c != nil && c.refreshToken != "" {
			if resolved := s.resolveClaudeCodeToken(ctx, c); resolved != "" && resolved != token {
				slog.Debug("Preferring Claude Code credential file over static env OAuth token so refresh can proceed")
				return s.availableAnthropicToken(ctx, resolved, model)
			}
		}
		return s.availableAnthropicToken(ctx, token, model)
	}
	if key := s.firstEnv(ctx, "ANTHROPIC_API_KEY"); key != "" {
		return s.availableAnthropicToken(ctx, key, model)
	}
	token := s.resolveAnthropicPoolToken(ctx, true)
	if token == "" {
		token = s.resolveClaudeCodeToken(ctx, readCreds())
	}
	return s.availableAnthropicToken(ctx, token, model)
}

var ErrClaudeCLIMissing = errors.New("The 'claude' CLI is not installed. Install it with: npm install -g @anthropic-ai/claude-code")

func (s *Store) RunSetupToken(ctx context.Context) (Secret, bool, error) {
	ctx = s.bind(ctx)
	claudePath, ok := s.findClaudeCodeCLI(ctx, "claude")
	if !ok {
		return Secret{}, false, ErrClaudeCLIMissing
	}
	cmd := exec.CommandContext(ctx, claudePath, "setup-token")
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	cmd.Env = s.childEnv(ctx)
	if err := cmd.Run(); err != nil && ctx.Err() != nil {
		return Secret{}, false, nil
	}
	if creds := s.readClaudeCodeCredentials(ctx); creds != nil && claudeCodeTokenValid(creds) {
		return NewSecret(creds.accessToken), true, nil
	}
	if token := s.firstEnv(ctx, "CLAUDE_CODE_OAUTH_TOKEN", "ANTHROPIC_TOKEN"); token != "" {
		return NewSecret(token), true, nil
	}
	return Secret{}, false, nil
}

func (s *Store) rootAnthropicOAuthFile() string {
	if s.rootAuth == "" {
		return ""
	}
	return home.AnthropicOAuth.In(s.root)
}

func (s *Store) AnthropicLogin(ctx context.Context, c Console) (AnthropicTokens, bool) {
	ctx = s.bind(ctx)
	raw, err := randomURLToken(32)
	if err != nil {
		return AnthropicTokens{}, false
	}
	verifier := raw
	challenge := pkceCodeChallenge(verifier)
	state, err := randomURLToken(32)
	if err != nil {
		return AnthropicTokens{}, false
	}
	authURL := "https://claude.ai/oauth/authorize?" + encodeForm(formOf(
		"code", "true", "client_id", anthropicOAuthClientID, "response_type", "code", "redirect_uri", anthropicOAuthRedirectURI,
		"scope", anthropicOAuthScopes, "code_challenge", challenge, "code_challenge_method", "S256", "state", state))
	c.println(strings.Join([]string{
		"", "Authorize icebath with your Claude Pro/Max subscription.", "",
		"╭─ Claude Pro/Max Authorization ────────────────────╮",
		"│                                                   │",
		"│  Open this link in your browser:                  │",
		"╰───────────────────────────────────────────────────╯",
		"", "  " + authURL, "",
	}, "\n"))
	if s.canOpenGraphicalBrowser(ctx) {
		if _, err := s.tryOpenBrowser(ctx, authURL); err == nil {
			c.println("  (Browser opened automatically)")
		}
	}
	c.println("\nAfter authorizing, you'll see a code. Paste it below.\n")
	c.printf("Authorization code: ")
	if c.In == nil {
		return AnthropicTokens{}, false
	}
	line, rerr := bufio.NewReader(c.In).ReadString('\n')
	if rerr != nil && line == "" {
		return AnthropicTokens{}, false
	}
	authCode := strings.TrimSpace(line)
	if authCode == "" {
		c.println("No code entered.")
		return AnthropicTokens{}, false
	}
	code, received, _ := strings.Cut(authCode, "#")
	if rest, _, found := strings.Cut(received, "#"); found {
		received = rest
	}
	if received != state {
		slog.Warn("OAuth state mismatch — possible CSRF, aborting")
		return AnthropicTokens{}, false
	}
	payload := newDoc()
	payload.set("grant_type", "authorization_code")
	payload.set("client_id", anthropicOAuthClientID)
	payload.set("code", code)
	payload.set("state", received)
	payload.set("redirect_uri", anthropicOAuthRedirectURI)
	payload.set("code_verifier", verifier)
	body, _ := payload.MarshalJSON()
	result, err := s.postAnthropicToken(ctx, body, "application/json", 15*time.Second, "exchange")
	if err != nil {
		detail := transportFailure(err)
		var oe *AnthropicOAuthError
		if errors.As(err, &oe) {
			detail = oe.Error()
		}
		c.printf("Token exchange failed: %s\n", detail)
		return AnthropicTokens{}, false
	}
	if !truthy(result.value("access_token")) {
		c.println("No access token in response.")
		return AnthropicTokens{}, false
	}
	return anthropicTokenState(result, ""), true
}

func (s *Store) readIcebathOAuthCredentials() *doc {
	data := loadJSONIfExists(home.AnthropicOAuth.In(s.home), "icebath OAuth credentials")
	if data == nil || !truthy(data.value("accessToken")) {
		return nil
	}
	return data
}

func (s *Store) HasAnthropicOAuthFileCredentials(ctx context.Context) bool {
	ctx = s.bind(ctx)
	if s.readIcebathOAuthCredentials() != nil {
		return true
	}
	creds := s.readClaudeCodeCredentials(ctx)
	return creds != nil && creds.accessToken != ""
}

func (s *Store) writeAnthropicOAuthFile(ctx context.Context, access, refresh, expiresAtMS any, target string) error {
	if target == "" {
		target = home.AnthropicOAuth.In(s.home)
	}
	payload := newDoc()
	payload.set("accessToken", access)
	payload.set("refreshToken", refresh)
	payload.set("expiresAt", expiresAtMS)
	return commitPrivateJSON(ctx, target, payload, "icebath OAuth credentials")
}

func (s *Store) SaveAnthropicLogin(ctx context.Context, tokens AnthropicTokens) error {
	return s.writeAnthropicOAuthFile(s.bind(ctx), tokens.AccessToken.Reveal(), tokens.RefreshToken.Reveal(), intNum(tokens.ExpiresAtMS), "")
}
