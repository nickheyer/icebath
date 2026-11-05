package mcp

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/home"
)

// tokenFileVersion is the stat signature plus content digest a tokens file change is detected by
type tokenFileVersion struct {
	mtimeNs, size int64
	ino           uint64
	ctimeNs       int64
	digest        string
}

// Signature and digest of a file, the error type as the digest when it stats but cannot be read
func readTokenFileVersion(path string) (tokenFileVersion, error) {
	st, err := os.Stat(path)
	if err != nil {
		return tokenFileVersion{}, err
	}
	v := tokenFileVersion{mtimeNs: st.ModTime().UnixNano(), size: st.Size(), ino: fileInode(path, st), ctimeNs: fileCtimeNs(st)}
	if raw, rerr := os.ReadFile(path); rerr == nil {
		sum := sha256.Sum256(raw)
		v.digest = hex.EncodeToString(sum[:])
	} else {
		v.digest = fmt.Sprintf("<%T>", rerr)
	}
	return v, nil
}

// pending401 is one in flight 401 recovery that every request with the same failed token awaits
type pending401 struct {
	done   chan struct{}
	result bool
}

// providerEntry is the OAuth state of one server in one home
type providerEntry struct {
	serverURL   string
	oauthConfig map[string]any
	transport   HTTPOptions
	provider    *OAuthProvider

	mu          sync.Mutex
	lastMtimeNs int64
	lastVersion *tokenFileVersion
	pending401  map[string]*pending401
}

type managerKey struct{ home, name string }

// OAuthManager caches one provider per home and server, reloads tokens changed on disk and dedupes 401 recoveries
type OAuthManager struct {
	mu      sync.Mutex
	entries map[managerKey]*providerEntry
}

var (
	oauthManagerMu sync.Mutex
	oauthManager   *OAuthManager
)

// Process wide manager, created on first use
func getManager() *OAuthManager {
	oauthManagerMu.Lock()
	defer oauthManagerMu.Unlock()
	if oauthManager == nil {
		oauthManager = &OAuthManager{entries: map[managerKey]*providerEntry{}}
	}
	return oauthManager
}

// Drops the process wide manager so the next use starts clean
func resetOAuthManager() {
	oauthManagerMu.Lock()
	defer oauthManagerMu.Unlock()
	oauthManager = nil
}

// Resolved home and server name an entry is keyed by, the context home when homeDir is empty
func (m *OAuthManager) key(ctx context.Context, serverName, homeDir string) managerKey {
	if homeDir == "" {
		homeDir = home.Dir(ctx)
	}
	homeDir = expandUser(homeDir)
	if resolved, err := pyResolve(homeDir); err == nil {
		homeDir = resolved
	}
	return managerKey{home: homeDir, name: serverName}
}

// Cached provider for a server, built on first use and rebuilt when its URL changed
func (m *OAuthManager) getOrBuildProvider(ctx context.Context, serverName, serverURL string, oauthConfig map[string]any, transport HTTPOptions) (*OAuthProvider, error) {
	key := m.key(ctx, serverName, "")
	m.mu.Lock()
	defer m.mu.Unlock()
	e := m.entries[key]
	if e != nil && e.serverURL != serverURL {
		mcpLogger().Info(fmt.Sprintf("MCP OAuth '%s': URL changed from %s to %s, discarding cache", serverName, e.serverURL, serverURL))
		e = nil
	}
	if e == nil {
		e = &providerEntry{serverURL: serverURL, oauthConfig: oauthConfig, transport: transport}
		m.entries[key] = e
	}
	if e.provider == nil {
		p, err := m.buildProvider(ctx, serverName, e, key.home)
		if err != nil {
			return nil, err
		}
		e.provider = p
	}
	return e.provider, nil
}

// Provider with the manager hooks, refusing a non interactive first login without a dashboard flow
func (m *OAuthManager) buildProvider(ctx context.Context, serverName string, e *providerEntry, homeKey string) (*OAuthProvider, error) {
	cfg, storage := prepareOAuthConfig(ctx, serverName, e.serverURL, e.oauthConfig)
	if dashboardOAuthFlowFrom(ctx) == nil && !isInteractive(ctx) && !storage.HasCachedTokens() {
		return nil, &OAuthNonInteractiveError{fmt.Sprintf("MCP OAuth for '%s': non-interactive environment and no cached tokens found. "+
			"Run `icebath mcp login %s` interactively first to complete initial authorization.", serverName, serverName)}
	}
	opts, err := buildProviderOptions(ctx, cfg, storage, false)
	if err != nil {
		return nil, err
	}
	opts.ServerURL = e.serverURL
	opts.HTTP = e.transport
	p, err := newOAuthProvider(*opts)
	if err != nil {
		return nil, err
	}
	p.hooks = &managerHooks{name: serverName, home: homeKey, preregistered: pyTruthy(cfg["client_id"])}
	return p, nil
}

// Evicts the provider and deletes the server's OAuth files, returning the evicted entry
func (m *OAuthManager) remove(ctx context.Context, serverName, homeDir string) (*providerEntry, error) {
	e := m.evict(ctx, serverName, homeDir)
	if err := RemoveOAuthTokens(ctx, serverName, homeDir); err != nil {
		return e, err
	}
	mcpLogger().Info(fmt.Sprintf("MCP OAuth '%s': evicted from cache and removed from disk", serverName))
	return e, nil
}

// Puts back an entry removed for a failed reauthorization unless a newer one took its place
func (m *OAuthManager) restoreEntry(ctx context.Context, serverName string, e *providerEntry, homeDir string) {
	if e == nil {
		return
	}
	key := m.key(ctx, serverName, homeDir)
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.entries[key]; !ok {
		m.entries[key] = e
	}
}

// Drops only the in process provider, keeping persisted OAuth state
func (m *OAuthManager) evict(ctx context.Context, serverName, homeDir string) *providerEntry {
	key := m.key(ctx, serverName, homeDir)
	m.mu.Lock()
	defer m.mu.Unlock()
	e := m.entries[key]
	delete(m.entries, key)
	return e
}

// Entry and provider of a server, nil when none was built
func (m *OAuthManager) lookup(ctx context.Context, serverName, homeDir string) (*providerEntry, *OAuthProvider) {
	key := m.key(ctx, serverName, homeDir)
	m.mu.Lock()
	defer m.mu.Unlock()
	e := m.entries[key]
	if e == nil {
		return nil, nil
	}
	return e, e.provider
}

// Forces a reload when the tokens file changed since last seen, the first sighting with tokens only seeding the baseline
func (m *OAuthManager) invalidateIfDiskChanged(ctx context.Context, serverName, homeDir string) (bool, error) {
	e, p := m.lookup(ctx, serverName, homeDir)
	if e == nil || p == nil {
		return false, nil
	}
	if err := p.lock(ctx); err != nil {
		return false, err
	}
	defer p.unlock()
	e.mu.Lock()
	defer e.mu.Unlock()
	dir := homeDir
	if dir == "" {
		dir = home.Dir(ctx)
	}
	version, err := readTokenFileVersion(filepath.Join(oauthTokenDir(dir), safeFilename(serverName)+".json"))
	if err != nil {
		return false, nil
	}
	if e.lastVersion != nil && *e.lastVersion == version {
		return false, nil
	}
	e.lastVersion = &version
	old := e.lastMtimeNs
	e.lastMtimeNs = version.mtimeNs
	if old == 0 && p.currentTokens != nil {
		return false, nil
	}
	p.initialized = false
	mcpLogger().Info(fmt.Sprintf("MCP OAuth '%s': tokens file changed (mtime %d -> %d), forcing reload", serverName, old, version.mtimeNs))
	return true, nil
}

// Disk reload check a 401 recovery runs, swapped by tests to count recovery attempts
var recoverDiskCheck = (*OAuthManager).invalidateIfDiskChanged

// One recovery attempt for a pending 401, always clearing the dedup slot
func (m *OAuthManager) recover401(ctx context.Context, serverName string, e *providerEntry, p *OAuthProvider, key string, pending *pending401) {
	canRefresh, err := recoverDiskCheck(m, ctx, serverName, "")
	if err != nil {
		mcpLogger().Warn(fmt.Sprintf("MCP OAuth '%s': 401 handler failed: %s", serverName, err))
		canRefresh = false
	} else if !canRefresh {
		if lerr := p.lock(ctx); lerr == nil {
			canRefresh = p.canRefreshToken()
			p.unlock()
		}
	}
	e.mu.Lock()
	delete(e.pending401, key)
	e.mu.Unlock()
	pending.result = canRefresh
	close(pending.done)
}

// True when a fresh token may be available after a tool call 401, concurrent 401s with one token sharing a recovery
func (m *OAuthManager) handle401(ctx context.Context, serverName, failedAccessToken string) (bool, error) {
	e, p := m.lookup(ctx, serverName, "")
	if e == nil || p == nil {
		return false, nil
	}
	key := failedAccessToken
	if key == "" {
		key = "<unknown>"
	}
	e.mu.Lock()
	pending := e.pending401[key]
	if pending == nil {
		pending = &pending401{done: make(chan struct{})}
		if e.pending401 == nil {
			e.pending401 = map[string]*pending401{}
		}
		e.pending401[key] = pending
		go m.recover401(context.WithoutCancel(ctx), serverName, e, p, key, pending)
	}
	e.mu.Unlock()
	select {
	case <-pending.done:
		return pending.result, nil
	case <-ctx.Done():
		return false, ctx.Err()
	}
}

// managerHooks adds disk reload, expiry seeding, metadata prefetch, registration refusal and dead client detection
type managerHooks struct {
	name          string
	home          string
	preregistered bool
}

func (h *managerHooks) logNonfatal(what string, err error) {
	mcpLogger().Debug(fmt.Sprintf("MCP OAuth '%s': %s failed (non-fatal): %s", h.name, what, err))
}

func (h *managerHooks) beforeFlow(ctx context.Context, _ *OAuthProvider) {
	if _, err := getManager().invalidateIfDiskChanged(ctx, h.name, h.home); err != nil {
		h.logNonfatal("pre-flow disk-watch", err)
	}
}

// Seeds the expiry of loaded tokens and prefetches metadata when tokens exist without it
func (h *managerHooks) afterInitialize(ctx context.Context, p *OAuthProvider) {
	t := p.currentTokens
	if t != nil && t.ExpiresIn != nil {
		if *t.ExpiresIn <= 0 {
			past := oauthNow() - 1
			p.tokenExpiry = &past
		} else {
			p.updateTokenExpiry(t)
		}
	}
	if t != nil && p.oauthMetadata == nil {
		if err := h.prefetchOAuthMetadata(ctx, p); err != nil {
			h.logNonfatal("pre-flight metadata discovery", err)
		} else {
			p.enforceRefreshTokenIssuer()
		}
	}
}

// Discovers protected resource then authorization server metadata before the first request, persisting what it finds
func (h *managerHooks) prefetchOAuthMetadata(ctx context.Context, p *OAuthProvider) error {
	client := &http.Client{Transport: p.client.Transport, CheckRedirect: p.client.CheckRedirect, Timeout: 10 * time.Second}
	send := func(rawURL, label string) *flowResponse {
		req, err := newMetadataRequest(ctx, rawURL)
		if err == nil {
			stampDefaultUserAgent(req)
			var resp *http.Response
			if resp, err = client.Do(req); err == nil {
				data, rerr := readCapped(resp.Body, MaxHTTPBodyBytes, rawURL)
				resp.Body.Close()
				if rerr == nil {
					return &flowResponse{StatusCode: resp.StatusCode, Header: resp.Header, Body: data, Request: req}
				}
				err = rerr
			}
		}
		mcpLogger().Debug(fmt.Sprintf("MCP OAuth '%s': %s discovery to %s failed: %s", h.name, label, rawURL, err))
		return nil
	}
	for _, u := range buildProtectedResourceMetadataDiscoveryURLs("", p.serverURL) {
		r := send(u, "PRM")
		if r == nil {
			continue
		}
		if prm := handleProtectedResourceResponse(r); prm != nil {
			p.protectedResourceMetadata = prm
			if len(prm.AuthorizationServers) > 0 {
				p.authServerURL = prm.AuthorizationServers[0]
			}
			break
		}
	}
	for _, u := range buildOAuthAuthorizationServerMetadataDiscoveryURLs(p.authServerURL, p.serverURL) {
		r := send(u, "ASM")
		if r == nil {
			continue
		}
		ok, asm := handleAuthMetadataResponse(r)
		if !ok {
			break
		}
		if asm != nil {
			p.oauthMetadata = asm
			if err := p.storage.SaveOAuthMetadata(asm); err != nil {
				return err
			}
			mcpLogger().Debug(fmt.Sprintf("MCP OAuth '%s': pre-flight ASM discovered token_endpoint=%s", h.name, asm.TokenEndpoint))
			break
		}
	}
	return nil
}

// Scheme, lowercased host, port without the scheme default and path of a URL, like httpx compares them
func httpxTarget(u *url.URL) [4]string {
	scheme := strings.ToLower(u.Scheme)
	port := u.Port()
	if (scheme == "http" && port == "80") || (scheme == "https" && port == "443") || (scheme == "ws" && port == "80") ||
		(scheme == "wss" && port == "443") || (scheme == "ftp" && port == "21") {
		port = ""
	}
	path := u.EscapedPath()
	if path == "" {
		path = "/"
	}
	return [4]string{scheme, strings.ToLower(u.Hostname()), port, path}
}

// Refuses the guessed registration POST when discovered metadata advertises no registration endpoint
func (h *managerHooks) checkOutgoing(_ context.Context, p *OAuthProvider, req *http.Request) error {
	md := p.oauthMetadata
	if md == nil || derefStr(md.RegistrationEndpoint) != "" || req.Method != http.MethodPost {
		return nil
	}
	expected := pyURLJoinAbsPath(authorizationBaseURL(p.serverURL), "/register")
	eu, err := url.Parse(expected)
	if err != nil || httpxTarget(req.URL) != httpxTarget(eu) {
		return nil
	}
	return &OAuthRegistrationError{fmt.Sprintf("MCP OAuth '%s': this provider does not support automatic client registration (its authorization "+
		"server advertises no registration_endpoint), so the SDK's fallback would POST %s and fail "+
		"(Google's hosted Gmail/Drive MCP servers and similar providers return 404 for it). Create an OAuth "+
		"client for this provider and add it under config.yaml mcp_servers.<name>.oauth (client_id, "+
		"client_secret), then run `icebath mcp login %s`.", h.name, expected, h.name)}
}

var invalidClientWord = regexp.MustCompile(`\binvalid_client\b`)

// Reports whether a response is the token endpoint rejecting our client id as invalid_client
func invalidClientAtTokenEndpoint(p *OAuthProvider, r *flowResponse) bool {
	if p.oauthMetadata == nil || p.oauthMetadata.TokenEndpoint == "" || r.Request == nil {
		return false
	}
	a, err := pyURLSplit(r.Request.URL.String())
	if err != nil {
		return false
	}
	b, err := pyURLSplit(p.oauthMetadata.TokenEndpoint)
	if err != nil {
		return false
	}
	if a.Scheme != b.Scheme || strings.ToLower(a.Netloc) != strings.ToLower(b.Netloc) ||
		strings.TrimRight(a.Path, "/") != strings.TrimRight(b.Path, "/") {
		return false
	}
	return invalidClientWord.Match(bytes.ToLower(r.Body))
}

// Deletes a dynamically registered client the token endpoint rejected, retiring a rejected CIMD document too
func (h *managerHooks) inspectResponse(_ context.Context, p *OAuthProvider, r *flowResponse) {
	if h.preregistered || (r.StatusCode != http.StatusBadRequest && r.StatusCode != http.StatusUnauthorized) || !invalidClientAtTokenEndpoint(p, r) {
		return
	}
	if p.clientMetadataURL != "" && p.clientInfo != nil && p.clientInfo.ClientID == p.clientMetadataURL {
		mcpLogger().Warn(fmt.Sprintf("MCP OAuth '%s': authorization server rejected our Client ID Metadata Document (%s) "+
			"with invalid_client — falling back to dynamic client registration.", h.name, p.clientMetadataURL))
		p.clientMetadataURL = ""
		p.storage.MarkCIMDRejected()
	}
	p.storage.PoisonClientRegistration()
	p.clientInfo = nil
	p.initialized = false
}

// Saves metadata discovered during the flow when the stored copy is missing or names another token endpoint
func (h *managerHooks) flowFinished(_ context.Context, p *OAuthProvider) {
	md := p.oauthMetadata
	if md == nil {
		return
	}
	if existing := p.storage.LoadOAuthMetadata(); existing == nil || existing.TokenEndpoint != md.TokenEndpoint {
		if err := p.storage.SaveOAuthMetadata(md); err != nil {
			mcpLogger().Error(fmt.Sprintf("MCP OAuth '%s': saving discovered metadata failed: %s", h.name, err))
		}
	}
}
