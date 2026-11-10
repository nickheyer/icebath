package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
)

// Bound on waiting for a peer's refresh, long enough for a slow token endpoint round trip
var refreshFenceTimeout = 60 * time.Second

// Non blocking lock attempt the fence polls, swapped by tests to inject lock syscall failures
var fenceLock = lockFile

// RefreshFenceTimeout aborts a refresh whose single use token this process cannot prove it owns
type RefreshFenceTimeout struct{ Msg string }

func (e *RefreshFenceTimeout) Error() string { return e.Msg }

// Wall clock in seconds the token expiry math reads, swapped by tests
var oauthNow = func() float64 { return float64(time.Now().UnixNano()) / 1e9 }

// Lock file beside a token file that fences one refresh generation
func refreshLockPath(path string) string { return path + ".refresh.lock" }

// Takes the cross process refresh fence, polling a non blocking lock until the timeout and failing closed
func acquireRefreshFence(ctx context.Context, path string, timeout time.Duration) (*os.File, error) {
	lockPath := refreshLockPath(path)
	if _, err := home.Mkdir(ctx, filepath.Dir(lockPath)); err != nil {
		return nil, &RefreshFenceTimeout{fmt.Sprintf("refresh fence unavailable (%s): %v", filepath.Base(lockPath), err)}
	}
	secureParentDir(lockPath)
	f, err := os.OpenFile(lockPath, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, &RefreshFenceTimeout{fmt.Sprintf("refresh fence unavailable (%s): %v", filepath.Base(lockPath), err)}
	}
	deadline := time.Now().Add(timeout)
	for {
		ok, lerr := fenceLock(f)
		if lerr != nil {
			f.Close()
			return nil, &RefreshFenceTimeout{fmt.Sprintf("refresh fence unavailable on this filesystem: %v", lerr)}
		}
		if ok {
			return f, nil
		}
		if !time.Now().Before(deadline) {
			f.Close()
			return nil, &RefreshFenceTimeout{fmt.Sprintf("refresh fence held by a peer for %.0fs (%s)", timeout.Seconds(), filepath.Base(lockPath))}
		}
		select {
		case <-ctx.Done():
			f.Close()
			return nil, ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// Unlocks and closes a fence descriptor, never failing
func releaseRefreshFence(f *os.File) {
	if f == nil {
		return
	}
	_ = unlockFile(f)
	_ = f.Close()
}

// Per profile token directory under the home
func oauthTokenDir(homeDir string) string { return filepath.Join(homeDir, "mcp-tokens") }

// Server name made safe for a filename, word characters and hyphens kept
func safeFilename(name string) string {
	var b strings.Builder
	for _, r := range name {
		if unicode.IsLetter(r) || unicode.IsNumber(r) || r == '_' || r == '-' {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	out := pyHead(strings.Trim(b.String(), "_"), 128)
	if out == "" {
		return "default"
	}
	return out
}

// Tightens a credential file's parent to 0700, refusing the root, top level dirs and the install tree
func secureParentDir(path string) { home.SecureParentDir(path) }

// Decoded JSON of a file, nil when missing, unreadable or invalid with a warning for the latter two
func readOAuthJSON(path string) any {
	data, err := os.ReadFile(path)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			mcpLogger().Warn(fmt.Sprintf("Failed to read %s: %v", path, err))
		}
		return nil
	}
	doc, err := decodeJSONDocument(data)
	if err != nil {
		mcpLogger().Warn(fmt.Sprintf("Failed to read %s: %v", path, err))
		return nil
	}
	return doc
}

// Writes OAuth state at 0600 from creation under a 0700 parent inside a live home
func writeOAuthJSON(ctx context.Context, path string, data any) error {
	if _, err := home.Mkdir(ctx, filepath.Dir(path)); err != nil {
		return err
	}
	secureParentDir(path)
	raw, err := indentJSON(data)
	if err != nil {
		return err
	}
	return config.AtomicWrite(path, raw, 0o600)
}

// TokenStorage persists one server's OAuth tokens, client record, metadata and CIMD refusal marker
type TokenStorage struct {
	serverName string
	homeDir    string

	mu           sync.Mutex
	loadedIssuer string
	boundIssuer  string
}

// Storage for a server under homeDir, the context's home when homeDir is empty
func NewTokenStorage(ctx context.Context, serverName, homeDir string) *TokenStorage {
	if homeDir == "" {
		homeDir = home.Dir(ctx)
	}
	return &TokenStorage{serverName: safeFilename(serverName), homeDir: homeDir}
}

// Sanitized server name the files are keyed by
func (s *TokenStorage) ServerName() string { return s.serverName }

// Home directory the storage lives under
func (s *TokenStorage) HomeDir() string { return s.homeDir }

// File of this server with the given suffix
func (s *TokenStorage) path(suffix string) string {
	return filepath.Join(oauthTokenDir(s.homeDir), s.serverName+suffix)
}

// Token file path
func (s *TokenStorage) TokensPath() string { return s.path(".json") }

// Client registration file path
func (s *TokenStorage) ClientInfoPath() string { return s.path(".client.json") }

// Authorization server metadata file path
func (s *TokenStorage) MetaPath() string { return s.path(".meta.json") }

// CIMD refusal marker path
func (s *TokenStorage) CIMDRejectedPath() string { return s.path(".cimd-off") }

// Token, client and metadata paths, the state a snapshot covers
func (s *TokenStorage) statePaths() []string {
	return []string{s.TokensPath(), s.ClientInfoPath(), s.MetaPath()}
}

// Issuer the token file recorded at the last load, empty when none
func (s *TokenStorage) LoadedIssuer() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.loadedIssuer
}

// Sets the loaded issuer record
func (s *TokenStorage) setLoadedIssuer(issuer string) {
	s.mu.Lock()
	s.loadedIssuer = issuer
	s.mu.Unlock()
}

// Reads a file into a model, nil when absent, not an object or invalid, warning without echoing values
func (s *TokenStorage) loadModel(path, label string, fixup func(m map[string]any) error, validate func(m map[string]any) (any, error)) (any, error) {
	doc := readOAuthJSON(path)
	if doc == nil {
		return nil, nil
	}
	m, ok := documentMap(doc)
	if !ok {
		mcpLogger().Warn(fmt.Sprintf("Corrupt %s at %s -- ignoring: expected a JSON object, got %s", label, path, pyJSONTypeName(doc)))
		return nil, nil
	}
	if fixup != nil {
		if err := fixup(m); err != nil {
			return nil, err
		}
	}
	model, err := validate(m)
	if err != nil {
		detail := err.Error()
		var ve *ValidationError
		if errors.As(err, &ve) {
			detail = "validation failed for " + ve.Fields()
		}
		mcpLogger().Warn(fmt.Sprintf("Corrupt %s at %s -- ignoring: %s", label, path, detail))
		return nil, nil
	}
	return model, nil
}

// Rewrites expires_in as seconds remaining from the stored absolute expiry, legacy files using the file mtime
func (s *TokenStorage) rebaseExpiresIn(m map[string]any) error {
	absolute, had := m["expires_at"]
	delete(m, "expires_at")
	if had && absolute != nil {
		at, ok := jsonNumeric(absolute)
		if !ok {
			return fmt.Errorf("unsupported operand type(s) for -: '%s' and 'float'", pyJSONTypeName(absolute))
		}
		m["expires_in"] = pyIntOfFloat(math.Max(at-oauthNow(), 0))
		return nil
	}
	if v, ok := m["expires_in"]; ok && v != nil {
		n, good := pyIntOfJSON(v)
		if !good {
			return nil
		}
		st, err := os.Stat(s.TokensPath())
		if err != nil {
			return nil
		}
		implied := float64(st.ModTime().UnixNano())/1e9 + float64(n)
		m["expires_in"] = pyIntOfFloat(math.Max(implied-oauthNow(), 0))
	}
	return nil
}

// Truncates toward zero like int() of a float, as a JSON integer
func pyIntOfFloat(f float64) any { return json.Number(strconv.FormatInt(int64(f), 10)) }

// Numeric value of a JSON number or boolean, the operands Python arithmetic accepts
func jsonNumeric(v any) (float64, bool) {
	switch x := v.(type) {
	case json.Number:
		f, err := strconv.ParseFloat(string(x), 64)
		return f, err == nil
	case bool:
		if x {
			return 1, true
		}
		return 0, true
	}
	return 0, false
}

// Python int() of a decoded JSON value, false where int raises
func pyIntOfJSON(v any) (int64, bool) {
	switch x := v.(type) {
	case json.Number:
		if n, err := strconv.ParseInt(string(x), 10, 64); err == nil {
			return n, true
		}
		f, err := strconv.ParseFloat(string(x), 64)
		if err != nil || math.IsInf(f, 0) || math.IsNaN(f) {
			return 0, false
		}
		return int64(f), true
	case bool, string:
		n, ok := pyInt(x)
		return int64(n), ok
	}
	return 0, false
}

// Stored tokens with expiry rebased and the issuer record popped, nil when absent or corrupt
func (s *TokenStorage) GetTokens() (*OAuthToken, error) {
	s.setLoadedIssuer("")
	model, err := s.loadModel(s.TokensPath(), "tokens", func(m map[string]any) error {
		issuer, _ := m["hermes_issuer"].(string)
		delete(m, "hermes_issuer")
		s.setLoadedIssuer(issuer)
		return s.rebaseExpiresIn(m)
	}, func(m map[string]any) (any, error) { return validateOAuthToken(m) })
	if err != nil || model == nil {
		return nil, err
	}
	return model.(*OAuthToken), nil
}

// Persists tokens with an absolute expiry and the bound issuer stamped
func (s *TokenStorage) SetTokens(ctx context.Context, t *OAuthToken) error {
	payload := t.dump()
	if t.ExpiresIn != nil {
		payload.set("expires_at", oauthNow()+float64(*t.ExpiresIn))
	}
	s.mu.Lock()
	bound := s.boundIssuer
	if bound != "" {
		payload.set("hermes_issuer", bound)
		s.loadedIssuer = bound
	}
	s.mu.Unlock()
	if err := writeOAuthJSON(home.WithOverride(ctx, s.homeDir), s.TokensPath(), payload); err != nil {
		return err
	}
	mcpLogger().Debug(fmt.Sprintf("OAuth tokens saved for %s", s.serverName))
	return nil
}

// Sets the issuer stamped on future token writes, empty clearing it
func (s *TokenStorage) BindIssuer(issuer string) {
	s.mu.Lock()
	s.boundIssuer = issuer
	s.mu.Unlock()
}

// Backfills the issuer onto a token file written before issuer binding existed
func (s *TokenStorage) StampIssuer(ctx context.Context, issuer string) {
	obj, ok := readOAuthJSON(s.TokensPath()).(*jsonObject)
	if !ok || jsonTruthy(obj.get("hermes_issuer")) {
		return
	}
	obj.set("hermes_issuer", issuer)
	if err := writeOAuthJSON(home.WithOverride(ctx, s.homeDir), s.TokensPath(), obj); err != nil {
		mcpLogger().Debug(fmt.Sprintf("Could not stamp issuer on tokens for %s: %v", s.serverName, err))
		return
	}
	s.setLoadedIssuer(issuer)
}

// Drops the refresh token and its issuer record from disk, keeping the access token
func (s *TokenStorage) StripRefreshToken(ctx context.Context) {
	obj, ok := readOAuthJSON(s.TokensPath()).(*jsonObject)
	if !ok || !jsonTruthy(obj.get("refresh_token")) {
		return
	}
	obj.pop("refresh_token")
	obj.pop("hermes_issuer")
	s.setLoadedIssuer("")
	if err := writeOAuthJSON(home.WithOverride(ctx, s.homeDir), s.TokensPath(), obj); err != nil {
		mcpLogger().Warn(fmt.Sprintf("Could not strip refresh token for %s: %v", s.serverName, err))
		return
	}
	mcpLogger().Info(fmt.Sprintf("Removed issuer-mismatched refresh token for %s (re-authorization will be required "+
		"when the access token expires)", s.serverName))
}

// Sets client_secret_post when a secret is present without a usable auth method
func coerceSecretAuthMethod(m map[string]any) bool {
	method := m["token_endpoint_auth_method"]
	if jsonTruthy(m["client_secret"]) && (method == nil || method == "none" || method == "") {
		m["token_endpoint_auth_method"] = "client_secret_post"
		return true
	}
	return false
}

// Stored client registration, persisting a coerced auth method so later flows skip the coercion
func (s *TokenStorage) GetClientInfo(ctx context.Context) (*OAuthClientInfo, error) {
	coerced := false
	model, err := s.loadModel(s.ClientInfoPath(), "client info", func(m map[string]any) error {
		coerced = coerceSecretAuthMethod(m)
		return nil
	}, func(m map[string]any) (any, error) { return validateClientInfo(m) })
	if err != nil || model == nil {
		return nil, err
	}
	info := model.(*OAuthClientInfo)
	if coerced {
		if err := writeOAuthJSON(home.WithOverride(ctx, s.homeDir), s.ClientInfoPath(), info.dump()); err != nil {
			return nil, err
		}
	}
	return info, nil
}

// Persists the client registration with the secret auth method coercion applied
func (s *TokenStorage) SetClientInfo(ctx context.Context, info *OAuthClientInfo) error {
	data := info.dump()
	if coerceSecretAuthMethod(data.toMap()) {
		data.set("token_endpoint_auth_method", "client_secret_post")
	}
	if err := writeOAuthJSON(home.WithOverride(ctx, s.homeDir), s.ClientInfoPath(), data); err != nil {
		return err
	}
	mcpLogger().Debug(fmt.Sprintf("OAuth client info saved for %s", s.serverName))
	return nil
}

// Persists server metadata so a restarted process can refresh without rediscovery
func (s *TokenStorage) SaveOAuthMetadata(ctx context.Context, md *OAuthMetadata) error {
	if err := writeOAuthJSON(home.WithOverride(ctx, s.homeDir), s.MetaPath(), md.dump()); err != nil {
		return err
	}
	mcpLogger().Debug(fmt.Sprintf("OAuth metadata saved for %s", s.serverName))
	return nil
}

// Stored server metadata, the device variant when it names a device endpoint, nil when absent or corrupt
func (s *TokenStorage) LoadOAuthMetadata() *OAuthMetadata {
	model, _ := s.loadModel(s.MetaPath(), "OAuth metadata", nil, func(m map[string]any) (any, error) { return validateOAuthMetadata(m, true) })
	if model == nil {
		return nil
	}
	return model.(*OAuthMetadata)
}

// Durably records that this server refused the CIMD document
func (s *TokenStorage) MarkCIMDRejected(ctx context.Context) {
	path := s.CIMDRejectedPath()
	if _, err := home.Mkdir(home.WithOverride(ctx, s.homeDir), filepath.Dir(path)); err != nil {
		mcpLogger().Debug(fmt.Sprintf("Could not record CIMD rejection at %s: %v", path, err))
		return
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, 0o666)
	if err != nil {
		mcpLogger().Debug(fmt.Sprintf("Could not record CIMD rejection at %s: %v", path, err))
		return
	}
	f.Close()
	now := time.Now()
	_ = os.Chtimes(path, now, now)
}

// Reports whether this server refused the CIMD document before
func (s *TokenStorage) CIMDRejected() bool {
	_, err := os.Stat(s.CIMDRejectedPath())
	return err == nil
}

// Deletes the stored state, sparing the metadata when asked and always the refresh lock
func (s *TokenStorage) Remove(keepMetadata bool) error {
	paths := []string{s.TokensPath(), s.ClientInfoPath(), s.CIMDRejectedPath()}
	if !keepMetadata {
		paths = append(paths, s.MetaPath())
	}
	for _, p := range paths {
		if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	return nil
}

// Bytes of the existing token, client and metadata files keyed by filename
func (s *TokenStorage) Snapshot() map[string][]byte {
	snap := map[string][]byte{}
	for _, p := range s.statePaths() {
		if data, err := os.ReadFile(p); err == nil {
			snap[filepath.Base(p)] = data
		}
	}
	return snap
}

// Reverts to a snapshot, skipped when asked to keep newer state another writer produced
func (s *TokenStorage) Restore(ctx context.Context, snap map[string][]byte, onlyIfAbsent bool) error {
	if onlyIfAbsent {
		for _, p := range s.statePaths() {
			if _, err := os.Stat(p); err == nil {
				mcpLogger().Info(fmt.Sprintf("Skipping OAuth rollback for %s because newer state exists", s.serverName))
				return nil
			}
		}
	}
	if err := s.Remove(false); err != nil {
		return err
	}
	if len(snap) == 0 {
		return nil
	}
	dir := oauthTokenDir(s.homeDir)
	if _, err := home.Mkdir(home.WithOverride(ctx, s.homeDir), dir); err != nil {
		return err
	}
	for name, data := range snap {
		f, err := os.OpenFile(filepath.Join(dir, name), os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
		if err == nil {
			_, err = f.Write(data)
			if cerr := f.Close(); err == nil {
				err = cerr
			}
		}
		if err != nil {
			mcpLogger().Warn(fmt.Sprintf("Failed to restore OAuth state %s: %v", name, err))
		}
	}
	return nil
}

// Discards a dead dynamic registration and stale metadata keeping one backup, true when a client file went
func (s *TokenStorage) PoisonClientRegistration() bool {
	clientPath := s.ClientInfoPath()
	data, err := os.ReadFile(clientPath)
	if err != nil {
		if _, statErr := os.Stat(clientPath); statErr != nil {
			return false
		}
		mcpLogger().Warn(fmt.Sprintf("Could not back up client info at %s: %v", clientPath, err))
	} else if werr := os.WriteFile(clientPath+".bak", data, 0o666); werr != nil {
		mcpLogger().Warn(fmt.Sprintf("Could not back up client info at %s: %v", clientPath, werr))
	}
	_ = os.Remove(clientPath)
	_ = os.Remove(s.MetaPath())
	mcpLogger().Warn(fmt.Sprintf("MCP OAuth '%s': cached client registration rejected as invalid_client; "+
		"removed client.json + meta.json (backup at %s) to force re-registration", s.serverName, filepath.Base(clientPath+".bak")))
	return true
}

// Reports whether a token file exists, expired or not
func (s *TokenStorage) HasCachedTokens() bool {
	_, err := os.Stat(s.TokensPath())
	return err == nil
}

// fileStamp is what a rewrite of a file changes, nil fields standing for a missing file
type fileStamp struct {
	MtimeNs int64
	Size    int64
	Ino     uint64
}

// CredentialStamp is the stat of the token and client files any reauthorization rewrites
type CredentialStamp [2]*fileStamp

// Reports whether two stamps describe the same files
func (c CredentialStamp) Equal(o CredentialStamp) bool {
	for i := range c {
		if (c[i] == nil) != (o[i] == nil) || (c[i] != nil && *c[i] != *o[i]) {
			return false
		}
	}
	return true
}

// Stat of the token and client files without reading their bytes
func (s *TokenStorage) CredentialStamp() CredentialStamp {
	var stamp CredentialStamp
	for i, p := range []string{s.TokensPath(), s.ClientInfoPath()} {
		st, err := os.Stat(p)
		if err != nil {
			continue
		}
		stamp[i] = &fileStamp{MtimeNs: st.ModTime().UnixNano(), Size: st.Size(), Ino: fileInode(p, st)}
	}
	return stamp
}

// Deletes every stored OAuth file of a server
func RemoveOAuthTokens(ctx context.Context, serverName, homeDir string) error {
	if err := NewTokenStorage(ctx, serverName, homeDir).Remove(false); err != nil {
		return err
	}
	mcpLogger().Info(fmt.Sprintf("OAuth tokens removed for '%s'", serverName))
	return nil
}
