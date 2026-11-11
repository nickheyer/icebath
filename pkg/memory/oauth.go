package memory

import (
	"context"
	"errors"
	"fmt"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"net/http"
	"strings"
	"unicode"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/profiles"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Path prefix the memory provider OAuth routes live under
const OAuthRoutePrefix = "/api/memory/providers"

// Routes for memory provider OAuth connect that the dashboard web server mounts
var OAuthRouter http.Handler

// Profile names that collide with the installation or a common system binary
var reservedProfileNames = map[string]bool{"icebath": true, "default": true, "test": true, "tmp": true, "root": true, "sudo": true}

// One HTTP failure with the detail the JSON body carries
type httpError struct {
	status int
	detail string
}

func (e *httpError) Error() string { return e.detail }

// Canonical profile id, default matched case insensitively and everything else lower cased
func normalizeProfileName(name string) (string, error) {
	stripped := pyStrip(name)
	if stripped == "" {
		return "", errors.New("profile name cannot be empty")
	}
	if strings.EqualFold(stripped, "default") {
		return "default", nil
	}
	return strings.ToLower(stripped), nil
}

// Refuses an invalid or reserved profile id, default passing
func validateProfileName(name string) error {
	if name == "default" {
		return nil
	}
	if !home.ProfileIDRe.MatchString(name) {
		return profiles.InvalidProfileNameError(name)
	}
	if reservedProfileNames[name] {
		return fmt.Errorf("Profile name %s is reserved — it collides with either the icebath installation itself or a "+
			"common system binary.  Pick a different name.", pyRepr(name))
	}
	return nil
}

// Home directory of a profile name, the root for default
func profileDir(name string) (string, error) {
	canon, err := normalizeProfileName(name)
	if err != nil {
		return "", err
	}
	if canon == "default" {
		return home.Root(), nil
	}
	if !home.ProfileIDRe.MatchString(canon) {
		return "", profiles.InvalidProfileNameError(canon)
	}
	return home.ProfileHome(home.Root(), canon), nil
}

// Whether a live profile directory exists for the name
func profileExists(name string) bool {
	canon, err := normalizeProfileName(name)
	if err != nil {
		return false
	}
	dir, err := profileDir(canon)
	if err != nil {
		return false
	}
	if canon == "default" {
		return true
	}
	return home.ProfileLive(dir)
}

// Request context scoped to the requested profile home, unchanged for none or current
func scopeToProfile(ctx context.Context, profile string) (context.Context, error) {
	requested := pyStrip(profile)
	if requested == "" || strings.ToLower(requested) == "current" {
		return ctx, nil
	}
	if err := validateProfileName(requested); err != nil {
		return nil, &httpError{status: http.StatusBadRequest, detail: err.Error()}
	}
	if !profileExists(requested) {
		return nil, &httpError{status: http.StatusNotFound, detail: fmt.Sprintf("Profile '%s' does not exist.", requested)}
	}
	dir, err := profileDir(requested)
	if err != nil {
		return nil, &httpError{status: http.StatusBadRequest, detail: err.Error()}
	}
	return home.WithOverride(ctx, dir), nil
}

// Python str isidentifier, a letter or underscore followed by letters, digits, marks, connectors or underscores
func pyIsIdentifier(s string) bool {
	if s == "" {
		return false
	}
	for i, r := range s {
		start := unicode.IsLetter(r) || r == '_' || unicode.Is(unicode.Nl, r)
		if i == 0 {
			if !start {
				return false
			}
			continue
		}
		if !start && !unicode.IsDigit(r) && !unicode.In(r, unicode.Mn, unicode.Mc, unicode.Nd, unicode.Pc) {
			return false
		}
	}
	return true
}

// OAuth flow of the provider installed for the scoped home, a not found error otherwise
func resolveOAuthFlow(ctx context.Context, providerName string) (OAuthFlow, error) {
	if !pyIsIdentifier(providerName) {
		return nil, &httpError{status: http.StatusNotFound, detail: fmt.Sprintf("unknown memory provider %s", pyRepr(providerName))}
	}
	flow, err := ProviderOAuthFlow(ctx, nil, providerName)
	if OAuthFlowUnsupported(err) {
		return nil, &httpError{status: http.StatusNotFound, detail: providerName + " does not support OAuth connect"}
	}
	return flow, err
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(tools.JSON(body)))
}

// Last value of the profile query parameter, empty when absent
func profileParam(r *http.Request) string {
	values := r.URL.Query()["profile"]
	if len(values) == 0 {
		return ""
	}
	return values[len(values)-1]
}

// Runs one flow call scoped to the requested profile, mapping failures to the route's status and detail
func serveFlow(w http.ResponseWriter, r *http.Request, failure string, call func(ctx context.Context, flow OAuthFlow) (any, error)) {
	providerName := r.PathValue("provider")
	result, err := func() (result any, err error) {
		defer func() {
			if rec := recover(); rec != nil {
				err = fmt.Errorf("%v", rec)
			}
		}()
		ctx, err := scopeToProfile(r.Context(), profileParam(r))
		if err != nil {
			return nil, err
		}
		flow, err := resolveOAuthFlow(ctx, providerName)
		if err != nil {
			return nil, err
		}
		return call(ctx, flow)
	}()
	if err != nil {
		var he *httpError
		if errors.As(err, &he) {
			writeJSON(w, he.status, map[string]any{"detail": he.detail})
			return
		}
		writeJSON(w, http.StatusInternalServerError, map[string]any{"detail": fmt.Sprintf(failure, providerName, pyrepr.ExceptionMessage(err))})
		return
	}
	writeJSON(w, http.StatusOK, result)
}

// Router with the start and status routes under the memory providers prefix
func newOAuthRouter() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST "+OAuthRoutePrefix+"/{provider}/oauth/start", func(w http.ResponseWriter, r *http.Request) {
		serveFlow(w, r, "Failed to start %s OAuth: %s", func(ctx context.Context, flow OAuthFlow) (any, error) {
			return flow.StartLoopbackFlowBackground(ctx)
		})
	})
	mux.HandleFunc("GET "+OAuthRoutePrefix+"/{provider}/oauth/status", func(w http.ResponseWriter, r *http.Request) {
		serveFlow(w, r, "Failed to read %s OAuth status: %s", func(ctx context.Context, flow OAuthFlow) (any, error) {
			return flow.FlowStatus(ctx)
		})
	})
	return mux
}

// Builds the memory provider OAuth router the dashboard mounts
func registerMemoryOAuthRoutes() {
	OAuthRouter = newOAuthRouter()
}
