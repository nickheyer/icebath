package connections

import (
	"context"
	"errors"
	"strings"

	"github.com/nickheyer/icebath/pkg/desktop/backend"
)

// Key of the object an expected 404 resolves with, the bridge turning it back into the 404 rejection
const APIExpected404 = "__hermesExpected404__"

// ProfileDeleteDecision is the teardown a profile DELETE calls for and the profile it reports
type ProfileDeleteDecision struct {
	Action  string
	Profile string
}

// DecideProfileDeleteAction picks no teardown, the primary teardown or a pool teardown for a deleted profile
func DecideProfileDeleteAction(profile string, primaryProfileKey func() string) ProfileDeleteDecision {
	if profile == "" || profile == "default" || !desktopProfileNameRE.MatchString(profile) {
		return ProfileDeleteDecision{Action: "noop"}
	}
	if profile == primaryProfileKey() {
		return ProfileDeleteDecision{Action: "teardown-primary", Profile: profile}
	}
	return ProfileDeleteDecision{Action: "teardown-pool", Profile: profile}
}

// ResolveRouteProfile routes the next request to the primary once a profile was torn down
func ResolveRouteProfile(tornDownProfile string, profile string) string {
	if tornDownProfile != "" {
		return ""
	}
	return profile
}

// Profile request view of a renderer api request
func profileRequestOf(req map[string]any) *ProfileRequest {
	return &ProfileRequest{Method: req["method"], Path: req["path"], Body: req["body"]}
}

// Tears down the backends of a profile about to be deleted, the deleted name returned
func (s *Service) prepareProfileDeleteRequest(req map[string]any) (string, error) {
	decision := DecideProfileDeleteAction(ProfileNameFromDeleteRequest(profileRequestOf(req)), s.local.backend.PrimaryProfileKey)
	switch decision.Action {
	case "noop":
		return "", nil
	case "teardown-primary":
		if _, err := s.ProfileRemember("default"); err != nil {
			return "", err
		}
		errs := make([]error, 2)
		parallel(2, func(i int) {
			if i == 0 {
				errs[0] = s.local.backend.TeardownPrimary(false)
			} else {
				errs[1] = s.local.backend.TeardownPoolBackend(decision.Profile)
			}
		})
		if err := errors.Join(errs...); err != nil {
			return "", firstError(errs)
		}
		return decision.Profile, nil
	}
	if err := s.local.backend.TeardownPoolBackend(decision.Profile); err != nil {
		return "", err
	}
	return decision.Profile, nil
}

func firstError(errs []error) error {
	for _, err := range errs {
		if err != nil {
			return err
		}
	}
	return nil
}

// Rename lifecycle of a profile PATCH, completed after the request and rolled back on its failure
type profileRenameLifecycle struct {
	kind     string
	rename   ProfileRename
	complete func() error
	rollback func() error
}

// Prepares a profile rename, moving the primary to default until the PATCH settles
func (s *Service) prepareProfileRenameRequest(req map[string]any) (*profileRenameLifecycle, error) {
	rename := ProfileRenameFromRequest(profileRequestOf(req))
	if rename == nil || !desktopProfileNameRE.MatchString(rename.OldName) || !desktopProfileNameRE.MatchString(rename.NewName) {
		return nil, nil
	}
	b := s.local.backend
	if rename.OldName != b.PrimaryProfileKey() {
		if err := b.TeardownPoolBackend(rename.OldName); err != nil {
			return nil, err
		}
		return &profileRenameLifecycle{kind: "pool", rename: *rename, complete: func() error { return nil }, rollback: func() error { return nil }}, nil
	}
	remember := func(name string) error {
		_, err := s.ProfileRemember(name)
		return err
	}
	restart := func() error {
		_, err := b.EnsureBackend("", backend.EnsureOptions{})
		return err
	}
	if err := remember("default"); err != nil {
		return nil, err
	}
	if err := b.TeardownPrimary(false); err != nil {
		if rerr := remember(rename.OldName); rerr != nil {
			return nil, rerr
		}
		_ = restart()
		return nil, err
	}
	return &profileRenameLifecycle{
		kind:   "primary",
		rename: *rename,
		complete: func() error {
			if err := remember(rename.NewName); err != nil {
				return err
			}
			defer s.reloadMainWindow()
			return b.TeardownPrimary(false)
		},
		rollback: func() error {
			if err := remember(rename.OldName); err != nil {
				return err
			}
			terr := b.TeardownPrimary(false)
			if rerr := restart(); rerr != nil {
				return rerr
			}
			return terr
		},
	}, nil
}

// Kind of the registry connection with the id, an error when the registry holds none
func (s *Service) registryConnectionKind(connectionID string) (string, error) {
	registry, err := s.lockedConnectionsRegistry()
	if err != nil {
		return "", err
	}
	source := registry.Find(connectionID)
	if source == nil {
		return "", errors.New(`No connection with id "` + connectionID + `".`)
	}
	return source.Kind, nil
}

// Stops the pooled backend and SSH scope of one connection qualified profile
func (s *Service) teardownConnectionScopedProfileBackend(connectionID, profile string) error {
	key := backend.BackendScopeKey(connectionID, profile)
	errs := make([]error, 2)
	parallel(2, func(i int) {
		if i == 0 {
			errs[0] = s.local.backend.StopPoolBackend(key)
		} else {
			errs[1] = s.TeardownSSH(key)
		}
	})
	return firstError(errs)
}

// Runs a registry profile DELETE under the deletion gate after stopping what serves the profile
func (s *Service) dispatchConnectionScopedProfileDelete(ctx context.Context, req map[string]any, connectionID string) (any, error) {
	targetProfile := ProfileNameFromDeleteRequest(profileRequestOf(req))
	logical := jsTrim(jsNullishString(req["profile"]))
	if logical == "" {
		logical = targetProfile
	}
	connectionID = jsTrim(connectionID)
	if targetProfile == "" || connectionID == "" {
		return nil, errors.New("Connection-scoped profile deletion requires a connection and profile.")
	}
	if targetProfile == "default" {
		return nil, errors.New("The default profile cannot be deleted.")
	}
	if !desktopProfileNameRE.MatchString(targetProfile) {
		return nil, errors.New("Invalid profile name: " + targetProfile)
	}
	release := s.local.deletion.Acquire(targetProfile)
	defer release()
	kind, err := s.registryConnectionKind(connectionID)
	if err != nil {
		return nil, err
	}
	if kind == "local" {
		if _, err := s.prepareProfileDeleteRequest(req); err != nil {
			return nil, err
		}
	} else if err := s.teardownConnectionScopedProfileBackend(connectionID, logical); err != nil {
		return nil, err
	}
	return s.dispatchRegistryAPIRequest(ctx, req, connectionID, nil, targetProfile)
}

// Sends a registry pinned request to that connection backend, tagging session rows with the connection
func (s *Service) dispatchRegistryAPIRequest(ctx context.Context, req map[string]any, connectionID string, routeProfile *string, requestProfile string) (any, error) {
	profile := ""
	if routeProfile != nil {
		profile = *routeProfile
	}
	var connection backend.Connection
	var err error
	if jsTruthy(req["passive"]) {
		connection, err = s.ensureRegistryBackend(connectionID, profile, "", true)
	} else {
		connection, err = s.local.backend.RunDialClaim(backend.BackendScopeKey(connectionID, profile), func() (backend.Connection, error) {
			return s.EnsureRegistryBackend(connectionID, profile)
		})
	}
	if err != nil {
		return nil, err
	}
	method := orGetMethod(jsOrString(req["method"]))
	requestPath := PathForRegistryBackendRequest(jsString(req["path"]), requestProfile, registryScopeOf(connection), method)
	response, err := s.fetchJSONForBackend(ctx, connection, requestPath, backendFetchOptions{
		Method: jsOrString(req["method"]), Body: req["body"], HasBody: hasKey(req, "body"), Upload: uploadOf(req["upload"]),
		TimeoutMs: float64(resolveTimeoutMs(ecmaToNumber(req["timeoutMs"]), DefaultFetchTimeoutMs)),
	})
	if err != nil {
		return nil, err
	}
	s.profiles.mu.Lock()
	err = s.profiles.afterProfileRequest(&connectionID, profileRequestOf(req), response, jsOrString(connection["mode"]))
	s.profiles.mu.Unlock()
	if err != nil {
		return nil, err
	}
	if strings.ToUpper(method) == "GET" {
		return TagRegistrySessionResponse(requestPath, response, connectionID), nil
	}
	return response, nil
}

func hasKey(m map[string]any, k string) bool {
	_, ok := m[k]
	return ok
}

// How a registry descriptor serves profile scoped paths
func registryScopeOf(connection backend.Connection) RegistryBackendRequestScope {
	return RegistryBackendRequestScope{Mode: jsOrString(connection["mode"]), RemoteProfile: jsOrString(connection["remoteProfile"]),
		SharedPrimary: connection["sharedPrimary"] == true, SharedRemote: connection["sharedRemote"] == true}
}

// Routes a renderer REST request to the backend its registry pin, session owner or profile route names
func (s *Service) handleHermesAPIRequest(ctx context.Context, req map[string]any) (any, error) {
	registryConnectionID := APIRequestRegistryConnectionID(req)
	if registryConnectionID != "" {
		if IsAllProfilesSessionListRequest(jsOrString(req["method"]), jsNullishString(req["path"])) {
			aggregate, handled, err := s.interceptSessionRequestForRemote(ctx, req, registryConnectionID)
			if err != nil {
				return nil, err
			}
			if handled {
				return aggregate, nil
			}
		}
		profile := jsNullishString(req["profile"])
		return s.dispatchRegistryAPIRequest(ctx, req, registryConnectionID, &profile, profile)
	}
	rerouted, handled, err := s.interceptSessionRequestForRemote(ctx, req, "")
	if err != nil {
		return nil, err
	}
	if handled {
		return rerouted, nil
	}
	rename, err := s.prepareProfileRenameRequest(req)
	if err != nil {
		return nil, err
	}
	tornDown, err := s.prepareProfileDeleteRequest(req)
	if err != nil {
		return nil, err
	}
	profile := jsNullishString(req["profile"])
	routeRequest := &backend.RouteRequest{Method: jsOrString(req["method"]), Path: jsNullishString(req["path"])}
	apiRoute := ResolveProfileAPIRequest(req["profile"], jsString(req["path"]), s.profileRouteOptions(profile, routeRequest))
	routeProfile := ResolveRouteProfile(tornDown, apiRoute.BackendProfile)
	if rename != nil {
		routeProfile = ""
	}
	ensureOpts := backend.EnsureOptions{Passive: jsTruthy(req["passive"]), Request: routeRequest}
	timeoutMs := float64(resolveTimeoutMs(ecmaToNumber(req["timeoutMs"]), DefaultFetchTimeoutMs))
	send := func(current backend.Connection) (any, error) {
		return s.fetchJSONForBackend(ctx, current, apiRoute.RequestPath, backendFetchOptions{
			Method: jsOrString(req["method"]), Body: req["body"], HasBody: hasKey(req, "body"), Upload: uploadOf(req["upload"]), TimeoutMs: timeoutMs,
		})
	}
	connection, err := s.local.backend.EnsureBackend(routeProfile, ensureOpts)
	var response any
	if err == nil {
		if connection.Endpoint() != nil {
			response, err = backend.RedialLocalGateway(func() (backend.Connection, error) { return s.local.backend.EnsureBackend(routeProfile, ensureOpts) },
				func() error { return s.local.backend.ForgetLocalGatewayDescriptor(routeProfile) }, send)
		} else {
			response, err = send(connection)
		}
	}
	if err != nil {
		if rename != nil {
			if rerr := rename.rollback(); rerr != nil {
				s.log("Failed to restore primary profile after rename error: " + rerr.Error())
			}
		}
		return nil, err
	}
	s.profiles.mu.Lock()
	aerr := s.profiles.afterProfileRequest(nil, profileRequestOf(req), response, jsOrString(connection["mode"]))
	s.profiles.mu.Unlock()
	if rename != nil {
		if cerr := rename.complete(); cerr != nil {
			return nil, cerr
		}
	}
	if aerr != nil {
		return nil, aerr
	}
	return response, nil
}

// Line desktop.log records for a failed api request, clamped
func formatAPIRequestFailure(req map[string]any, err error) string {
	method := strings.ToUpper(orGetMethod(jsNullishString(req["method"])))
	path := "(no path)"
	if p, ok := req["path"]; ok && p != nil {
		path = jsString(p)
	}
	path = jsStringSlice(path, 500)
	return jsStringSlice("[hermes:api "+method+" "+path+"] "+errorDetail(err), 6000)
}

// Api handles one renderer REST request, an expected 404 resolving with the sentinel the bridge unwraps
func (s *Service) Api(request any) (any, error) {
	req := jsObject(request)
	if req == nil {
		req = map[string]any{}
	}
	pr := profileRequestOf(req)
	deleting := ProfileNameFromDeleteRequest(pr)
	mutating := deleting
	if mutating == "" {
		if rename := ProfileRenameFromRequest(pr); rename != nil {
			mutating = rename.OldName
		}
	}
	registryConnectionID := APIRequestRegistryConnectionID(req)
	ctx := context.Background()
	response, err := func() (any, error) {
		if deleting != "" && registryConnectionID != "" {
			return s.dispatchConnectionScopedProfileDelete(ctx, req, registryConnectionID)
		}
		if mutating == "" {
			return s.handleHermesAPIRequest(ctx, req)
		}
		release := s.local.deletion.Acquire(mutating)
		defer release()
		return s.handleHermesAPIRequest(ctx, req)
	}()
	if err == nil {
		return response, nil
	}
	if IsNotFoundAPIError(err) {
		return map[string]any{APIExpected404: err.Error()}, nil
	}
	s.log(formatAPIRequestFailure(req, err))
	s.flushLog()
	return nil, err
}
