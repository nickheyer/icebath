package connections

import (
	"errors"
	"regexp"
	"strings"
	"unicode/utf16"

	"github.com/nickheyer/icebath/pkg/desktop/backend"
)

var (
	pluginIDPattern      = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,127}$`)
	pluginPathForbidden  = regexp.MustCompile(`[#\\]`)
	pluginSegmentUnsafe  = regexp.MustCompile(`[/\\%]`)
	errInvalidPluginID   = errors.New("Invalid plugin id")
	errInvalidPluginPath = errors.New("Invalid plugin socket path")
)

// PluginRefusal is the reply of a plugin socket whose grant the gateway refuses for unsafe permissions
type PluginRefusal struct {
	Refused string `json:"refused"`
}

// IsMintPermissionRefusal reports a grant refused because the gateway control path is not private
func IsMintPermissionRefusal(err error) bool {
	message := err.Error()
	if strings.HasPrefix(message, "Unsafe gateway control path") || strings.HasPrefix(message, "Profile home is writable by a group") {
		return true
	}
	var reason backend.TicketReasonError
	return errors.As(err, &reason) && reason.TicketReason() == "unsafe_control_permissions"
}

// PluginWsPath is the socket path and query of a plugin under its own /api/plugins prefix
func PluginWsPath(pluginID, path any) (string, error) {
	id, ok := pluginID.(string)
	if !ok || !pluginIDPattern.MatchString(id) || strings.Contains(id, "..") {
		return "", errInvalidPluginID
	}
	prefix := "/api/plugins/" + id + "/"
	p, ok := path.(string)
	if !ok || !strings.HasPrefix(p, "/") || len(utf16.Encode([]rune(p))) > 2048 || pluginPathForbidden.MatchString(p) {
		return "", errInvalidPluginPath
	}
	beforeQuery, _, _ := strings.Cut(p, "?")
	for _, segment := range strings.Split(beforeQuery, "/") {
		decoded, err := jsDecodeURIComponent(segment)
		if err != nil || decoded == "." || decoded == ".." || pluginSegmentUnsafe.MatchString(decoded) {
			return "", errInvalidPluginPath
		}
	}
	u, err := parseURLRef(prefix[:len(prefix)-1]+p, "ws://plugin.invalid")
	if err != nil || !strings.HasPrefix(u.Pathname(), prefix) {
		return "", errInvalidPluginPath
	}
	return u.Pathname() + u.Search(), nil
}

// GetPluginWsUrl answers a one use local plugin socket URL, nil for a remote backend, a refusal for unsafe permissions
func (s *Service) GetPluginWsUrl(windowID string, payload any) (any, error) {
	req := jsObject(payload)
	path, err := PluginWsPath(req["pluginId"], req["path"])
	if err != nil {
		return nil, err
	}
	connectionID, profile := req["connectionId"], req["profile"]
	profileName := ""
	if p, ok := profile.(string); ok {
		profileName = p
	}
	ensure := func() (backend.Connection, error) {
		if jsTruthy(connectionID) {
			return s.EnsureRegistryBackend(jsString(connectionID), jsNullishString(profile))
		}
		return s.local.backend.EnsureBackend(jsNullishString(profile), backend.EnsureOptions{})
	}
	first, err := ensure()
	if err != nil {
		return nil, err
	}
	if first.Endpoint() == nil {
		return nil, nil
	}
	url, err := backend.LocalGatewayDial(backend.LocalDial{
		Ensure: ensure,
		Forget: func() error {
			if jsTruthy(connectionID) {
				return s.forgetRegistryLocalGatewayDescriptor(jsString(connectionID), jsNullishString(profile))
			}
			return s.local.backend.ForgetLocalGatewayDescriptor(jsNullishString(profile))
		},
		LaunchHome: s.icebathHome,
		Prepare: func(baseURL, ticket string) (string, error) {
			return s.local.backend.PreparePluginDial(baseURL, path, ticket, windowID)
		},
		Profile: profileName,
		Purpose: "native-http",
	})
	if err != nil {
		if IsMintPermissionRefusal(err) {
			return PluginRefusal{Refused: "permission"}, nil
		}
		return nil, err
	}
	return url, nil
}

// Drops the cached registry local descriptor whose gateway owner is gone, the primary path when the route delegates
func (s *Service) forgetRegistryLocalGatewayDescriptor(connectionID, profile string) error {
	key := orDefault(jsTrim(profile), "default")
	route := ResolveRegistryLocalRoute(key, LocalRouteOptions{GlobalRemote: s.globalRemoteActive(), ProfileRemoteOverride: s.profileHasRemoteOverride(key)})
	if route.Delegate {
		return s.local.backend.ForgetLocalGatewayDescriptor(profile)
	}
	if s.local.backend.ForgetPoolEntry(route.PoolKey) {
		s.log(`[gateway] forgot stale canonical endpoint for connection "` + orDefault(jsTrim(connectionID), "primary") + `" profile "` + key + `"; re-ensuring`)
	}
	return nil
}
