package connections

import (
	"context"
	"errors"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/desktop/backend"
)

// RosterSourceEnumerationTimeoutMs gives cookie gated remote and cloud sources the longer roster budget
func RosterSourceEnumerationTimeoutMs(kind, authMode string) int {
	if (kind == "remote" || kind == "cloud") && authMode == "oauth" {
		return 30_000
	}
	return 10_000
}

// Ticket for an OAuth remote, a Cloud outage or the reauth and transport failures explained otherwise
func (s *Service) resolveRemoteOauthTicket(ctx context.Context, baseURL string, headers map[string]string) (string, error) {
	hadNative := s.hasNativeSession(baseURL)
	ticket, err := s.mintGatewayWsTicket(ctx, baseURL, headers)
	if err == nil {
		return ticket, nil
	}
	if cloud := backend.MakeNousCloudBackendDownError(baseURL, err); cloud != nil {
		return "", cloud
	}
	return "", GatewayTicketFailure(err, OauthTicketFailureAuthMessage(hadNative), GatewayTicketTransportMessage(err))
}

// One use socket ticket, native bearer first, cookie second, transient failures retried, Cloud cookie sessions recovered
func (s *Service) mintGatewayWsTicket(ctx context.Context, baseURL string, headers map[string]string) (string, error) {
	tokens := s.nativeTokens()
	deps := MintGatewayWsTicketDeps{
		EnsureNativeAccessToken: tokens.Ensure,
		FetchJSON: func(ctx context.Context, url string, o JSONOptions) (any, error) {
			return s.fetchJSON(ctx, url, nil, o)
		},
		FetchJSONViaOauthSession: s.fetchJSONViaOauthSession,
	}
	return RecoverCloudSession(s.cloudRecovery(), baseURL, func() (string, error) {
		return WithTransientRetries(func() (string, error) {
			return MintOauthGatewayWsTicket(ctx, baseURL, deps, headers)
		}, RetryOptions{IsRetryable: func(err error) bool { return !isNativeAuthChanged(err) && !IsGatewayAuthRejection(err) }})
	})
}

// Restores a Cloud agent cookie session from the live portal session in a hidden window, never prompting
func (s *Service) restoreCloudCookieSession(baseURL string) (bool, error) {
	if !s.hasLivePortalSession() {
		return false, nil
	}
	if !s.hasPortalAccessToken() && !s.renewPortalAccessSilently(false) {
		return false, nil
	}
	if _, err := s.openOauthLoginWindow(context.Background(), baseURL, LoginWindowOptions{Silent: true, Background: true}); err != nil {
		return false, err
	}
	return true, nil
}

// Plain header map of a connection descriptor
func connectionHeaders(conn backend.Connection) map[string]string {
	out := map[string]string{}
	switch headers := conn["headers"].(type) {
	case map[string]string:
		for k, v := range headers {
			out[k] = v
		}
	case map[string]any:
		for k, v := range headers {
			if text, ok := v.(string); ok {
				out[k] = text
			}
		}
	}
	return out
}

// Connection descriptor of a URL shaped or tunnelled remote, minting a ticket for OAuth and refusing a missing token
func (s *Service) buildRemoteConnection(ctx context.Context, rawURL, authMode, token, source, remoteHost, remoteKind string, remoteIdentity any, headers any) (backend.Connection, error) {
	baseURL, err := NormalizeRemoteBaseURL(rawURL)
	if err != nil {
		return nil, err
	}
	remoteHeaders := s.decryptRemoteHeaders(headers)
	host := remoteHost
	if host == "" {
		host = HostLabelFromBaseURL(baseURL)
	}
	if remoteKind == "" {
		remoteKind = "url"
	}
	out := backend.Connection{"baseUrl": baseURL, "mode": "remote", "source": source, "remoteKind": remoteKind, "headers": remoteHeaders}
	if host != "" {
		out["remoteHost"] = host
	}
	if remoteIdentity != nil {
		out["remoteIdentity"] = remoteIdentity
	}
	if authMode == "oauth" {
		ticket, err := s.resolveRemoteOauthTicket(ctx, baseURL, remoteHeaders)
		if err != nil {
			return nil, err
		}
		wsURL, err := BuildGatewayWsURLWithTicket(baseURL, ticket)
		if err != nil {
			return nil, err
		}
		s.rememberRemoteWsHeaders(wsURL, remoteHeaders)
		out["authMode"], out["token"], out["wsUrl"] = "oauth", nil, wsURL
		return out, nil
	}
	if token == "" {
		return nil, errors.New("Remote Icebath gateway is selected, but no session token is saved. " +
			"Open Settings → Gateway and save a token, or switch back to Local.")
	}
	wsURL, err := BuildGatewayWsURL(baseURL, token)
	if err != nil {
		return nil, err
	}
	s.rememberRemoteWsHeaders(wsURL, remoteHeaders)
	out["authMode"], out["token"], out["wsUrl"] = "token", token, wsURL
	return out, nil
}

// RemoteWsURL is the socket URL of a remote descriptor, a fresh one use ticket for OAuth, the token URL otherwise
func (s *Service) RemoteWsURL(conn backend.Connection) (string, error) {
	headers := connectionHeaders(conn)
	if conn["authMode"] == "oauth" {
		baseURL := jsOrString(conn["baseUrl"])
		ticket, err := s.mintGatewayWsTicket(context.Background(), baseURL, headers)
		if err != nil {
			return "", err
		}
		wsURL, err := BuildGatewayWsURLWithTicket(baseURL, ticket)
		if err != nil {
			return "", err
		}
		s.rememberRemoteWsHeaders(wsURL, headers)
		return wsURL, nil
	}
	wsURL := jsOrString(conn["wsUrl"])
	s.rememberRemoteWsHeaders(wsURL, headers)
	return wsURL, nil
}

// REST call against a resolved descriptor, OAuth backends bearer first then cookie, others by token or native grant
func (s *Service) requestDescriptorJSON(ctx context.Context, descriptor backend.Connection, path string, o JSONOptions) (any, error) {
	baseURL := jsOrString(descriptor["baseUrl"])
	url := baseURL + path
	headers := connectionHeaders(descriptor)
	if descriptor["authMode"] == "oauth" {
		if o.Upload != nil {
			return nil, errors.New("File uploads are not supported against OAuth-gated remote backends yet.")
		}
		options := JSONOptions{Method: o.Method, Body: o.Body, HasBody: o.HasBody, TimeoutMs: o.TimeoutMs, Headers: headers}
		return RequestWithOauthFallback(ctx, baseURL, OauthRestRequestDeps[any]{
			EnsureNativeAccessToken: s.nativeTokens().Ensure,
			RequestWithBearer: func(bearer string) (any, error) {
				withBearer := options
				withBearer.Bearer = bearer
				return s.fetchJSON(ctx, url, nil, withBearer)
			},
			RequestWithCookie: func() (any, error) { return s.fetchJSONViaOauthSession(ctx, url, options) },
		})
	}
	var token *string
	if text, ok := descriptor["token"].(string); ok {
		token = &text
	}
	options := JSONOptions{Method: o.Method, Body: o.Body, HasBody: o.HasBody, Upload: o.Upload, TimeoutMs: o.TimeoutMs, Headers: headers}
	if descriptor.Endpoint() != nil {
		options.Gateway = descriptor
	}
	return s.fetchJSON(ctx, url, token, options)
}

// Readiness probe of a connection's auth mode and whether its 401 means a rejected session
func (s *Service) readinessHealthProbe(baseURL, authMode string, token *string) (func(ctx context.Context, url string, o JSONOptions) error, bool) {
	if authMode == "oauth" {
		return func(ctx context.Context, url string, o JSONOptions) error {
			_, err := RequestWithOauthFallback(ctx, baseURL, OauthRestRequestDeps[any]{
				EnsureNativeAccessToken: s.nativeTokens().Ensure,
				RequestWithBearer: func(bearer string) (any, error) {
					withBearer := o
					withBearer.Bearer = bearer
					return s.fetchJSON(ctx, url, nil, withBearer)
				},
				RequestWithCookie: func() (any, error) { return s.fetchJSONViaOauthSession(ctx, url, o) },
			})
			return err
		}, true
	}
	auth := ResolveReadinessProbeAuth(authMode, "", token)
	if auth.Kind == "token" && auth.Token != nil && *auth.Token != "" {
		return func(ctx context.Context, url string, o JSONOptions) error {
			_, err := s.fetchJSON(ctx, url, auth.Token, o)
			return err
		}, true
	}
	return func(ctx context.Context, url string, o JSONOptions) error {
		_, err := s.fetchPublicJSON(ctx, url, o)
		return err
	}, false
}

// Polls a backend ready with the credentials its connection uses
func (s *Service) waitForHermes(ctx context.Context, baseURL string, token *string, authMode string, headers map[string]string, alreadyBound bool) error {
	probe, credentialed := s.readinessHealthProbe(baseURL, authMode, token)
	withHeaders := func(o JSONOptions) JSONOptions { return requestOptionsWithHeaders(o, headers) }
	tokenText := ""
	if token != nil {
		tokenText = *token
	}
	return backend.WaitForHermesReady(baseURL, backend.ReadyOptions{
		Ctx:   ctx,
		Token: tokenText,
		FetchJSON: func(ctx context.Context, url, _ string) error {
			if credentialed {
				return probe(ctx, url, withHeaders(JSONOptions{}))
			}
			_, err := s.fetchJSON(ctx, url, token, JSONOptions{})
			return err
		},
		ProbeHealth: func(ctx context.Context, url string, timeout time.Duration) error {
			return probe(ctx, url, withHeaders(JSONOptions{TimeoutMs: float64(timeout.Milliseconds())}))
		},
		ProbeIsCredentialed: credentialed,
		AlreadyBound:        alreadyBound,
	})
}

func descriptorToken(remote backend.Connection) *string {
	if text, ok := remote["token"].(string); ok {
		return &text
	}
	return nil
}

// Boot readiness of a remote, running the silent portal cascade once for a Cloud agent whose cookie expired
func (s *Service) waitForRemoteHermes(ctx context.Context, remote backend.Connection) error {
	baseURL, authMode := jsOrString(remote["baseUrl"]), jsOrString(remote["authMode"])
	headers := connectionHeaders(remote)
	err := s.waitForHermes(ctx, baseURL, descriptorToken(remote), authMode, headers, false)
	if err == nil {
		return nil
	}
	if !ShouldAttemptCloudBootCascade(remote, err) || !s.hasLivePortalSession() {
		return err
	}
	s.log("[cloud] boot: agent session rejected but portal session is live, running silent sign-in")
	if _, cascadeErr := s.cloudAgentSilentSignIn(ctx, baseURL, true); cascadeErr != nil {
		s.log("[cloud] boot: silent sign-in did not complete: " + cascadeErr.Error())
		return err
	}
	return s.waitForHermes(ctx, baseURL, descriptorToken(remote), authMode, headers, false)
}

// CloudLoginError asks the user to sign in to Icebath Cloud again
type CloudLoginError struct{ Message string }

func (e *CloudLoginError) Error() string { return e.Message }

// NeedsCloudLogin marks the error for the Cloud sign-in prompt
func (e *CloudLoginError) NeedsCloudLogin() bool { return true }

// CloudAgentSignIn is the outcome of a silent per agent Cloud sign-in
type CloudAgentSignIn struct {
	BaseURL   string `json:"baseUrl"`
	Connected bool   `json:"connected"`
}

// Signs a Cloud agent in silently through the live portal session
func (s *Service) cloudAgentSilentSignIn(ctx context.Context, dashboardURL string, background bool) (CloudAgentSignIn, error) {
	baseURL, err := NormalizeRemoteBaseURL(dashboardURL)
	if err != nil {
		return CloudAgentSignIn{}, err
	}
	if !s.hasLivePortalSession() {
		return CloudAgentSignIn{}, &CloudLoginError{Message: "Your Icebath Cloud session has expired. Sign in to Icebath Cloud again."}
	}
	if !s.hasPortalAccessToken() {
		s.renewPortalAccessSilently(false)
	}
	if _, err := s.openOauthLoginWindow(ctx, baseURL, LoginWindowOptions{Silent: true, Background: background}); err != nil {
		return CloudAgentSignIn{}, err
	}
	return CloudAgentSignIn{BaseURL: baseURL, Connected: s.hasOauthSessionCookie(baseURL, LoginIdentity{})}, nil
}

// ServedStartupProfile is the served profile a primary boots as when active is not served, empty keeping active
func ServedStartupProfile(active, remembered string, profiles []string, served map[string]bool) string {
	names := []string{}
	for _, name := range profiles {
		if served == nil || served[name] {
			names = append(names, name)
		}
	}
	if slices.Contains(names, active) {
		return ""
	}
	if remembered != "" && slices.Contains(names, remembered) {
		return remembered
	}
	if slices.Contains(names, "default") {
		return "default"
	}
	if len(names) > 0 {
		return names[0]
	}
	return ""
}

// ServedProfileSet is the gateway's shared profile set from its status, nil when unknown
func ServedProfileSet(status any) map[string]bool {
	shared, ok := jsObject(status)["gateway_shared_with"].([]any)
	if !ok {
		return nil
	}
	out := map[string]bool{}
	for _, name := range shared {
		if text, ok := name.(string); ok && strings.TrimSpace(text) != "" {
			out[text] = true
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// Names of a complete profile listing, nil for a failed or partial read
func installedProfiles(listing any) []string {
	profiles, ok := jsObject(listing)["profiles"].([]any)
	if !ok || len(profiles) == 0 {
		return nil
	}
	names := make([]string, 0, len(profiles))
	for _, profile := range profiles {
		name, ok := jsObject(profile)["name"].(string)
		if !ok || strings.TrimSpace(name) == "" {
			return nil
		}
		names = append(names, name)
	}
	return names
}

// CreatePrimaryRemoteConnection is the renderer facing primary descriptor keeping the route identity
func CreatePrimaryRemoteConnection(remote backend.Connection, logs []string, windowState map[string]any) backend.Connection {
	authMode := remote["authMode"]
	if !jsTruthy(authMode) {
		authMode = "token"
	}
	out := backend.Connection{"baseUrl": remote["baseUrl"], "mode": "remote", "authMode": authMode,
		"headers": remote["headers"], "token": remote["token"], "wsUrl": remote["wsUrl"], "logs": logs}
	for _, key := range []string{"source", "remoteHost", "remoteKind", "remoteHermesVersion"} {
		if value, ok := remote[key]; ok && value != nil {
			out[key] = value
		}
	}
	if id := jsOrString(remote["connectionId"]); id != "" {
		out["connectionId"] = id
	}
	if ssh, ok := remote["ssh"]; ok && jsTruthy(ssh) {
		out["ssh"] = ssh
	}
	for key, value := range windowState {
		out[key] = value
	}
	return out
}

// ConnectRemote waits a saved remote ready, a primary boot restoring a remembered profile the remote does not serve
func (s *Service) ConnectRemote(remote backend.Connection, profile string, boot *backend.PrimaryRemoteBoot) (backend.Connection, error) {
	if boot == nil {
		if err := s.waitForRemoteHermes(context.Background(), remote); err != nil {
			return nil, err
		}
		return remote, nil
	}
	ctx := boot.Context
	if ctx == nil {
		ctx = context.Background()
	}
	b := s.authBackend()
	checkpoint := func() error {
		if ctx.Err() != nil {
			return context.Cause(ctx)
		}
		return boot.AssertCurrentAttempt()
	}
	restorable := func() bool {
		s.mu.Lock()
		config := s.readDesktopConnectionConfig()
		s.mu.Unlock()
		return backend.PrimaryProfileRestorable(boot.RequestedProfile, boot.InheritsPrimary, ParseLaunchProfile(os.Args), ProfileHasRemoteConnection(config.object(), "default"))
	}
	if err := checkpoint(); err != nil {
		return nil, err
	}
	b.AdvanceBootProgress("backend.remote", "Connecting to remote Icebath backend at "+jsOrString(remote["baseUrl"]), 24)
	if err := s.waitForRemoteHermes(ctx, remote); err != nil {
		return nil, err
	}
	if err := checkpoint(); err != nil {
		return nil, err
	}
	restored := ""
	source := jsOrString(remote["source"])
	remoteProfile := jsOrString(jsObject(toJSONValue(remote["ssh"]))["remoteProfile"])
	if restorable() && profile != "default" && (source == "settings" || source == "env" || source == "registry") && remoteProfile == "" {
		var listing, status any
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			listing, _ = s.requestDescriptorJSON(ctx, remote, "/api/profiles", JSONOptions{TimeoutMs: 8_000})
		}()
		go func() {
			defer wg.Done()
			status, _ = s.requestDescriptorJSON(ctx, remote, "/api/status", JSONOptions{TimeoutMs: 8_000})
		}()
		wg.Wait()
		if err := checkpoint(); err != nil {
			return nil, err
		}
		if profiles := installedProfiles(listing); profiles != nil {
			remembered := ""
			s.profiles.mu.Lock()
			if p := s.profiles.connectionProfile(jsOrString(remote["connectionId"])); p != nil {
				remembered = *p
			}
			s.profiles.mu.Unlock()
			if target := ServedStartupProfile(profile, remembered, profiles, ServedProfileSet(status)); target != "" && restorable() {
				restored = target
				boot.OnProfileRestored(restored)
			}
		}
	}
	b.UpdateBootProgress(map[string]any{"phase": "backend.ready", "message": "Remote Icebath backend is ready", "progress": 94, "running": true, "error": nil}, false)
	out := CreatePrimaryRemoteConnection(remote, b.RecentLogs(), b.MainWindowState())
	if restored != "" {
		out["profile"] = restored
	}
	return out, nil
}

// Advertised auth providers of a gateway, cached per base URL once read
func (s *Service) gatewayAuthProviders(ctx context.Context, baseURL string, headers map[string]string) []AdvertisedAuthProvider {
	st := s.auth.state()
	st.mu.Lock()
	cached, ok := st.providers[baseURL]
	st.mu.Unlock()
	if ok {
		return cached
	}
	body, err := s.fetchPublicJSON(ctx, baseURL+"/api/auth/providers", requestOptionsWithHeaders(JSONOptions{TimeoutMs: 8_000}, headers))
	if err != nil {
		return []AdvertisedAuthProvider{}
	}
	providers := []AdvertisedAuthProvider{}
	if list, ok := jsObject(body)["providers"].([]any); ok {
		for _, raw := range list {
			p := jsObject(raw)
			if p == nil {
				continue
			}
			if name := jsOrString(p["name"]); name != "" {
				providers = append(providers, AdvertisedAuthProvider{Name: name, SupportsPassword: jsTruthy(p["supports_password"])})
			}
		}
	}
	st.mu.Lock()
	st.providers[baseURL] = providers
	st.mu.Unlock()
	return providers
}

// OauthLoginResult is the outcome of a gateway sign-in
type OauthLoginResult struct {
	OK           bool   `json:"ok"`
	BaseURL      string `json:"baseUrl,omitempty"`
	Error        string `json:"error,omitempty"`
	Connected    bool   `json:"connected"`
	ConnectionID string `json:"connectionId,omitempty"`
}

// Releases the remote reauth latch once a sign-in is confirmed
func (s *Service) releaseReauthLatch() {
	if b := s.authBackend(); b != nil {
		b.ReleaseReauthLatch()
	}
}

// OauthLoginConnectionConfig signs in to a gateway, native PKCE when the providers allow it, the embedded window otherwise
func (s *Service) OauthLoginConnectionConfig(rawURL, rawOpts any) (OauthLoginResult, error) {
	ctx := context.Background()
	baseURL, err := NormalizeRemoteBaseURL(rawURL)
	if err != nil {
		return OauthLoginResult{}, err
	}
	tokens := s.nativeTokens()
	authIsCurrent, err := tokens.BeginLogin(baseURL)
	if err != nil {
		return OauthLoginResult{}, err
	}
	opts := jsObject(rawOpts)
	s.mu.Lock()
	registry, registryErr := s.readDesktopConnectionsRegistry()
	s.mu.Unlock()
	loginConnectionID := ""
	if registryErr == nil {
		loginConnectionID = ConnectionIDForPendingLogin(opts["connectionId"], opts["label"], registry)
	}
	pendingKind, _ := opts["kind"].(string)
	pendingAuthMode, _ := opts["authMode"].(string)
	identity := LoginIdentity{ConnectionID: loginConnectionID, PendingAuthMode: pendingAuthMode, PendingKind: pendingKind}
	statusBody, statusErr := s.fetchPublicJSON(ctx, baseURL+"/api/status", JSONOptions{TimeoutMs: 8_000})
	if statusErr != nil {
		statusBody = nil
	}
	providers := []AdvertisedAuthProvider{}
	if statusBody != nil && AuthModeFromStatus(statusBody) == "oauth" {
		providers = s.gatewayAuthProviders(ctx, baseURL, nil)
	}
	strategy := ResolveLoginStrategy(statusBody, false, providers)
	if !authIsCurrent() {
		return OauthLoginResult{}, NativeAuthChangedError{}
	}
	if strategy == "native" {
		err := func() error {
			set, err := s.runNativeLogin(ctx, baseURL)
			if err != nil {
				return err
			}
			if !authIsCurrent() {
				return NativeAuthChangedError{}
			}
			if err := tokens.StoreTokens(baseURL, set); err != nil {
				return err
			}
			s.releaseReauthLatch()
			return nil
		}()
		if err != nil {
			s.log("[native-oauth] native login failed (" + err.Error() + ")")
			return OauthLoginResult{OK: false, Error: err.Error(), Connected: false, ConnectionID: loginConnectionID}, nil
		}
		return OauthLoginResult{OK: true, BaseURL: baseURL, Connected: true, ConnectionID: loginConnectionID}, nil
	}
	if _, err := s.openOauthLoginWindow(ctx, baseURL, LoginWindowOptions{Identity: identity}); err != nil {
		return OauthLoginResult{}, err
	}
	connected := s.hasOauthSessionCookie(baseURL, identity)
	if !authIsCurrent() {
		return OauthLoginResult{}, NativeAuthChangedError{}
	}
	if connected {
		if err := tokens.ClearTokens(baseURL); err != nil {
			return OauthLoginResult{}, err
		}
		s.releaseReauthLatch()
	}
	return OauthLoginResult{OK: true, BaseURL: baseURL, Connected: connected, ConnectionID: loginConnectionID}, nil
}

// OauthLogoutResult is whether any session survived a sign-out
type OauthLogoutResult struct {
	OK        bool `json:"ok"`
	Connected bool `json:"connected"`
}

// OauthLogoutConnectionConfig drops the native tokens and the session cookies of a gateway
func (s *Service) OauthLogoutConnectionConfig(rawURL any) (OauthLogoutResult, error) {
	baseURL, err := NormalizeRemoteBaseURL(rawURL)
	if err != nil {
		return OauthLogoutResult{}, err
	}
	if err := s.nativeTokens().ClearTokens(baseURL); err != nil {
		return OauthLogoutResult{}, err
	}
	s.clearOauthSession(baseURL)
	return OauthLogoutResult{OK: true, Connected: s.hasLiveOauthSession(baseURL) || s.hasNativeSession(baseURL)}, nil
}
