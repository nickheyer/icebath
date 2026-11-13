package connections

import (
	"context"
	"strings"

	"github.com/nickheyer/icebath/pkg/desktop/backend"
)

// Profiles whose stored entry routes them to a remote, SSH or cloud backend
func (s *Service) configuredRemoteProfileNames() []string {
	s.mu.Lock()
	config := s.readDesktopConnectionConfig()
	s.mu.Unlock()
	obj := config.object()
	names := []string{}
	for _, name := range config.profileOrder() {
		if ProfileHasRemoteConnection(obj, name) {
			names = append(names, name)
		}
	}
	return names
}

// Issues a method against the backend a profile resolves to, parsed JSON
func (s *Service) requestJSONForProfile(ctx context.Context, profile, path, method string, body any, hasBody bool) (any, error) {
	conn, err := s.local.backend.EnsureBackend(profile, backend.EnsureOptions{})
	if err != nil {
		return nil, err
	}
	return s.fetchJSONForBackend(ctx, conn, path, backendFetchOptions{Method: method, Body: body, HasBody: hasBody, TimeoutMs: DefaultFetchTimeoutMs})
}

func (s *Service) fetchJSONForProfile(ctx context.Context) FetchJSONForProfile {
	return func(profile, path string) (any, error) {
		return s.requestJSONForProfile(ctx, profile, path, "GET", nil, false)
	}
}

// Body of a session mutation copied with the owner profile set
func bodyWithProfile(body any, profile string, always bool) (any, bool) {
	if obj, ok := body.(map[string]any); ok {
		out := jsSpreadOf(obj)
		if profile != "" || always {
			out["profile"] = profile
		}
		return out, true
	}
	if always {
		return map[string]any{"profile": profile}, true
	}
	if _, ok := body.([]any); ok && profile != "" {
		return jsSpreadOf(body), true
	}
	return body, body != nil
}

// Answers session reads and mutations from the remote that owns them, handled false for the local path
func (s *Service) interceptSessionRequestForRemote(ctx context.Context, req map[string]any, registryConnectionID string) (any, bool, error) {
	rawPath, ok := req["path"].(string)
	if !ok {
		return nil, false, nil
	}
	method := strings.ToUpper(orGetMethod(jsOrString(req["method"])))
	pathname, params, _, ok := parsePathParams(rawPath, "http://x")
	if !ok {
		return nil, false, nil
	}
	requestProfile := jsNullishString(req["profile"])
	if method == "GET" && (pathname == "/api/profiles/sessions" || pathname == "/api/profiles/sessions/sidebar") {
		remoteProfiles := s.configuredRemoteProfileNames()
		baseCoversLocal := !s.globalRemoteActive()
		sources := s.pooledRegistrySessionSources(ctx, ShouldIncludeLocalRegistrySessionSource(registryConnectionID, baseCoversLocal))
		if len(remoteProfiles) == 0 && len(sources) == 0 {
			return nil, false, nil
		}
		if !HasPinnedRegistrySessionSource(registryConnectionID, requestProfile, sources, !s.globalRemoteActive()) {
			return nil, false, nil
		}
		if pathname == "/api/profiles/sessions" {
			requested := orDefault(jsTrim(orDefault(params.getString("profile"), "all")), "all")
			if requested != "all" {
				if !s.profileHasRemoteOverride(requested) {
					return nil, false, nil
				}
				out, err := s.remoteSessionList(ctx, requested, params)
				return out, true, err
			}
			out, err := s.mergeRemoteProfileSessions(ctx, params, remoteProfiles, sources, true, registryConnectionID)
			return out, true, err
		}
		slices := BuildSidebarSessionSliceParams(params)
		results := make([]any, 3)
		errs := make([]error, 3)
		parallel(3, func(i int) {
			p := []*searchParams{slices.Recents, slices.Cron, slices.Messaging}[i]
			results[i], errs[i] = s.fetchProfilesSessionSlice(ctx, p, remoteProfiles, sources, true, registryConnectionID)
		})
		if err := firstError(errs); err != nil {
			return nil, true, err
		}
		return AssembleSidebarSessionSlices(results[0], results[1], results[2]), true, nil
	}
	if !IsRemoteSessionRequestPath(pathname) {
		return nil, false, nil
	}
	profile := jsTrim(orDefault(params.getString("profile"), jsOrString(req["profile"])))
	if profile == "" {
		segments := strings.Split(pathname, "/")
		raw := ""
		if len(segments) > 3 {
			raw = segments[3]
		}
		id, err := jsDecodeURIComponent(raw)
		if err != nil {
			return nil, true, err
		}
		profile = s.remoteOwnerProfileForSession(ctx, id)
		if profile == "" {
			return nil, false, nil
		}
	}
	passthrough := params.copyParams()
	passthrough.deleteName("profile")
	if s.profileHasRemoteOverride(profile) {
		alias := ""
		if ssh := ProfileSSHOverride(s.lockedConnectionConfig(), profile); ssh != nil {
			alias = ssh.RemoteProfile
		}
		ownerScope := orDefault(RemoteProfileQueryScope(profile, alias), profile)
		ownerPath, err := RemoteOwnerRoutePath(pathname, params, ownerScope)
		if err != nil {
			return nil, true, err
		}
		if method == "GET" {
			out, err := s.fetchJSONForProfile(ctx)(profile, ownerPath)
			return out, true, err
		}
		body, hasBody := req["body"], hasKey(req, "body")
		if obj, ok := body.(map[string]any); ok {
			copied := jsSpreadOf(obj)
			if ownerScope != "" {
				copied["profile"] = ownerScope
			}
			body = copied
		} else if arr, ok := body.([]any); ok {
			copied := jsSpreadOf(arr)
			if ownerScope != "" {
				copied["profile"] = ownerScope
			}
			body = copied
		}
		out, err := s.requestJSONForProfile(ctx, profile, ownerPath, method, body, hasBody)
		return out, true, err
	}
	if s.globalRemoteActive() {
		passthrough.set("profile", profile)
		path := pathname + "?" + passthrough.encoded()
		if method == "GET" {
			out, err := s.fetchJSONForProfile(ctx)("", path)
			return out, true, err
		}
		body := map[string]any{"profile": profile}
		if jsIsObject(req["body"]) {
			body = jsSpreadOf(req["body"])
			body["profile"] = profile
		}
		out, err := s.requestJSONForProfile(ctx, "", path, method, body, true)
		return out, true, err
	}
	return nil, false, nil
}

// A remote profile session list tagged with the remote scope as owner
func (s *Service) remoteSessionList(ctx context.Context, profile string, params *searchParams) (any, error) {
	alias := ""
	if ssh := ProfileSSHOverride(s.lockedConnectionConfig(), profile); ssh != nil {
		alias = ssh.RemoteProfile
	}
	data, err := FetchRemoteProfileSessions(profile, params, s.fetchJSONForProfile(ctx), alias)
	if err != nil {
		return nil, err
	}
	rows := TagRemoteSessionRows(rowsOf(data), orDefault(RemoteProfileQueryScope(profile, alias), profile))
	out := jsSpreadOf(data)
	out["sessions"] = rows
	return out, nil
}

// Remote profile owning a session id, remembered for a short while, empty when none
func (s *Service) remoteOwnerProfileForSession(ctx context.Context, sessionID string) string {
	if sessionID == "" {
		return ""
	}
	remoteProfiles := s.configuredRemoteProfileNames()
	if len(remoteProfiles) == 0 {
		return ""
	}
	if cached, ok := s.local.remoteOwners.Fresh(sessionID); ok {
		return cached.Profile
	}
	owner := FindRemoteOwnerProfileForSession(sessionID, remoteProfiles, func(profile string, params *searchParams) (any, error) {
		return s.remoteSessionList(ctx, profile, params)
	})
	s.local.remoteOwners.Remember(sessionID, owner)
	return owner
}

// One session list slice with remote profiles spliced in, always data
func (s *Service) fetchProfilesSessionSlice(ctx context.Context, params *searchParams, remoteProfiles []string, sources []RegistrySessionSource, sourcesGiven bool, pinned string) (any, error) {
	requested := orDefault(jsTrim(orDefault(params.getString("profile"), "all")), "all")
	if requested != "all" {
		if s.profileHasRemoteOverride(requested) {
			return s.remoteSessionList(ctx, requested, params)
		}
		return FetchPrimaryProfileSessions(params, s.fetchJSONForProfile(ctx), false), nil
	}
	return s.mergeRemoteProfileSessions(ctx, params, remoteProfiles, sources, sourcesGiven, pinned)
}

// Unified session list over the primary aggregate, every remote profile and every connected registry gateway
func (s *Service) mergeRemoteProfileSessions(ctx context.Context, params *searchParams, remoteProfiles []string, sourcesOverride []RegistrySessionSource, sourcesGiven bool, pinned string) (any, error) {
	limit := maxF(1, orNumber(params.number("limit"), 20))
	offset := maxF(0, orNumber(params.number("offset"), 0))
	order := "last_active"
	if v, ok := params.get("order"); ok && v == "created" {
		order = "started_at"
	}
	base := FetchPrimaryProfileSessions(params, s.fetchJSONForProfile(ctx), true)
	if pinned != "" {
		TagPrimaryAggregateRows(base, func() (any, error) {
			c, err := s.local.backend.EnsureBackend("", backend.EnsureOptions{Passive: true})
			return map[string]any(c), err
		}, pinned)
	}
	remoteParams := params.copyParams()
	remoteParams.set("limit", jsNumber(limit+offset))
	remoteParams.set("offset", "0")
	remoteSet := map[string]bool{}
	for _, p := range remoteProfiles {
		remoteSet[p] = true
	}
	merged := []any{}
	for _, row := range rowsOf(base) {
		if p, ok := jsObject(row)["profile"].(string); ok && remoteSet[p] {
			continue
		}
		merged = append(merged, row)
	}
	baseObj := jsObject(base)
	profileTotals := jsSpreadOf(orEmptyObject(baseObj["profile_totals"]))
	total := orNumber(ecmaToNumber(baseObj["total"]), 0)
	removed := any(0.0)
	for _, p := range remoteProfiles {
		removed = jsAdd(removed, orZero(profileTotals[p]))
	}
	total = total - ecmaToNumber(removed)
	outcomes := SettleRemoteProfileSessions(remoteProfiles, func(name string) (any, error) {
		return s.remoteSessionList(ctx, name, remoteParams)
	}, RemoteProfileSessionBudgetMs)
	total, remoteErrors := ApplyRemoteProfileSessionOutcomes(outcomes, &merged, profileTotals, total)
	sources := sourcesOverride
	if !sourcesGiven {
		sources = s.pooledRegistrySessionSources(ctx, false)
	}
	if len(sources) > 0 {
		getRegistryJSON := func(descriptor any, path string) (any, error) {
			return s.fetchJSONForBackend(ctx, backend.Connection(jsObject(descriptor)), path, backendFetchOptions{TimeoutMs: 10000})
		}
		identity := RegistryBackendIdentity{PrimaryBackendID: s.primaryInstallID().ID(), UnknownPrimaryProfiles: remoteProfiles}
		total += MergeRegistrySessionRows(sources, remoteParams, getRegistryJSON, &merged, profileTotals, identity)
	}
	recency := func(row any) float64 {
		obj := jsObject(row)
		v, ok := obj[order]
		if !ok || v == nil {
			v = obj["started_at"]
		}
		if v == nil {
			v = 0.0
		}
		return ecmaToNumber(v)
	}
	merged = sortedByDescending(merged, recency)
	archivedOnly := params.getString("archived") == "only"
	return ComposeUnifiedSessionResponse(base, merged, offset, limit, total, profileTotals, remoteErrors, archivedOnly), nil
}

func maxF(a, b float64) float64 {
	if a != a || b != b {
		return a + b
	}
	if a > b {
		return a
	}
	return b
}

func orEmptyObject(v any) any {
	if jsTruthy(v) {
		return v
	}
	return map[string]any{}
}

// Every connected registry gateway as a session source, never dialing
func (s *Service) pooledRegistrySessionSources(ctx context.Context, includeLocal bool) []RegistrySessionSource {
	registry, err := s.lockedConnectionsRegistry()
	if err != nil {
		return nil
	}
	b := s.local.backend
	return CollectRegistrySessionSources(RegistrySessionSourceDeps{
		Connections:   registry.Connections,
		Pool:          b.PoolBackends(),
		ScopePrefix:   backend.BackendScopePrefix,
		ReusedPrimary: func(connectionID string) any { return s.reusedPrimarySSHDescriptor(registry, connectionID) },
		InstallID: func(connectionID string, descriptor any) string {
			return BoundedInstallID(func() string { return s.probeConnectionInstallID(ctx, connectionID, descriptor) }, identityProbeBudgetMs)
		},
		SharedProfiles: func(connectionID string, backends []RegistrySessionBackend) []string {
			shared := make([]SharedSSHBackend, len(backends))
			for i, b := range backends {
				shared[i] = SharedSSHBackend{Descriptor: backend.Connection(jsObject(b.Descriptor))}
				if b.ProfileLabel != nil {
					shared[i].ProfileLabel = *b.ProfileLabel
				}
			}
			return SharedSSHProfiles(connectionID, shared)
		},
	}, includeLocal)
}

// The running primary descriptor when it is this registry SSH connection, never starting one
func (s *Service) reusedPrimarySSHDescriptor(registry ConnectionRegistry, connectionID string) any {
	if connectionID != registry.Primary {
		return nil
	}
	pending := s.local.backend.PrimaryConnection()
	if pending == nil {
		return nil
	}
	descriptor, err := waitFuture(pending, 2000)
	if err != nil || descriptor == nil {
		return nil
	}
	if descriptor["mode"] == "remote" && descriptor["remoteKind"] == "ssh" && RegistrySourceOwnsPrimaryBackend(registry, connectionID, descriptor) {
		return map[string]any(descriptor)
	}
	return nil
}

// Install id a registry connection backend reports on /api/status, TTL cached
func (s *Service) probeConnectionInstallID(ctx context.Context, connectionID string, descriptor any) string {
	return ProbeCachedConnectionInstallID(connectionID, func() (any, error) {
		return s.fetchJSONForBackend(ctx, backend.Connection(jsObject(descriptor)), "/api/status", backendFetchOptions{TimeoutMs: 8000})
	})
}
