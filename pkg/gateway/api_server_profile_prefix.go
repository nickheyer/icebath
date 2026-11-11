package gateway

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/profiles"
)

const (
	// Handle Bot Mode advertises the host's default profile under
	apiDefaultProfileHandle = "icebath"
	// Body of the 404 for a prefix naming no profile here
	apiUnknownProfileError = "Unknown or unconfigured profile"
)

// Logger of the api_server profile prefix module
func apiProfilePrefixLog() *slog.Logger {
	return slog.Default().With("logger", "gateway.platforms.api_server_profile_prefix")
}

// Whether a profile prefix names the profile this gateway serves, any failure reading as no
func apiPrefixNamesServedProfile(ctx context.Context, profile string) (served bool) {
	defer func() {
		if recover() != nil {
			served = false
		}
	}()
	return profiles.ProfileMatchesHome(ctx, profile, "")
}

// Whether the default handle is free to alias the default profile, unknown counting as taken
func apiHandleIsFree() (free bool) {
	defer func() {
		if rec := recover(); rec != nil {
			apiProfilePrefixLog().Warn(fmt.Sprintf("Cannot check for a '%s' profile", apiDefaultProfileHandle), "error", rec)
			free = false
		}
	}()
	return !profiles.ProfileExists(apiDefaultProfileHandle)
}

// Profile prefix of a request as a canonical profile id, empty without a prefix
func apiPrefixProfile(request *WebRequest) (string, error) {
	raw := pystr.Strip(request.MatchInfo["profile"])
	if raw == "" {
		return "", nil
	}
	return profiles.NormalizeProfileName(raw)
}

// The 404 for a rejected prefix, a default handle profile served elsewhere answering with its own body
func APIProfileRejectionResponse(request *WebRequest) (*WebResponse, error) {
	profile, err := apiPrefixProfile(request)
	if err != nil {
		return nil, err
	}
	if profile == apiDefaultProfileHandle && !apiHandleIsFree() {
		return WebJSONResponse(pyjson.ObjectOf("error", pyjson.ObjectOf(
			"message", fmt.Sprintf("Profile '%s' exists on this host but this gateway does not serve it", apiDefaultProfileHandle),
			"code", "profile_not_served")), http.StatusNotFound, nil)
	}
	return WebJSONResponse(pyjson.ObjectOf("error", apiUnknownProfileError), http.StatusNotFound, nil)
}

// Profile a request prefix routes to, nil for none or this gateway's own profile, rejected when not served
func APIResolveRequestProfile(ctx context.Context, runner any, request *WebRequest) (*string, bool, error) {
	profile, err := apiPrefixProfile(request)
	if err != nil {
		return nil, false, err
	}
	if profile == "" {
		return nil, false, nil
	}
	alias := profile == apiDefaultProfileHandle
	multiplex := false
	if holder, ok := runner.(gatewayConfigRunner); ok && holder != nil {
		if cfg := holder.GatewayConfig(); cfg != nil && cfg.MultiplexProfiles != nil {
			multiplex = *cfg.MultiplexProfiles
		}
	}
	if !multiplex {
		served := apiPrefixNamesServedProfile(ctx, profile) ||
			(alias && apiPrefixNamesServedProfile(ctx, "default") && apiHandleIsFree())
		return nil, !served, nil
	}
	entries, err := profiles.ProfilesToServe(ctx, true, false, false)
	if err != nil {
		apiProfilePrefixLog().Warn(fmt.Sprintf("Cannot list served profiles for /p/%s/", profile), "error", err)
		return nil, true, nil
	}
	served := map[string]bool{}
	for _, entry := range entries {
		served[entry.Name] = true
	}
	if served[profile] {
		return &profile, false, nil
	}
	if alias && served["default"] && apiHandleIsFree() {
		name := "default"
		return &name, false, nil
	}
	return nil, true, nil
}
