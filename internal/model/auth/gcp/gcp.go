// Google Cloud credentials and Vertex AI endpoint resolution from service account, user, federated and metadata sources
package gcp

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	DefaultRegion      = "global"
	CloudPlatformScope = "https://www.googleapis.com/auth/cloud-platform"

	tokenEndpoint     = "https://oauth2.googleapis.com/token"
	defaultUniverse   = "googleapis.com"
	iamScope          = "https://www.googleapis.com/auth/iam"
	iamTokenEndpoint  = "https://iamcredentials.googleapis.com/v1/projects/-/serviceAccounts/%s:generateAccessToken"
	resourceManager   = "https://cloudresourcemanager.googleapis.com/v1/projects/"
	cloudSDKClientID  = "764086051850-6qr4p6gpi6hn506pt8ejuq83di341hur.apps.googleusercontent.com"
	tokenLifetime     = time.Hour
	refreshThreshold  = 3*time.Minute + 45*time.Second
	vertexRefreshSkew = 5 * time.Minute
	requestTimeout    = 120 * time.Second
	replyLimit        = 1 << 20
	fileLimit         = 1 << 20

	boundaryVariable       = "GOOGLE_AUTH_TRUST_BOUNDARY_ENABLED"
	allowedLocationsHeader = "x-allowed-locations"
	noOpLocations          = "0x0"
	serviceAccountBoundary = "https://iamcredentials.%s/v1/projects/-/serviceAccounts/%s/allowedLocations"
	workforceBoundary      = "https://iamcredentials.%s/v1/locations/global/workforcePools/%s/allowedLocations"
	workloadBoundary       = "https://iamcredentials.%s/v1/projects/%s/locations/global/workloadIdentityPools/%s/allowedLocations"
)

var errBoundaryRefresh = errors.New("trust boundary lookup refused")

const (
	kindServiceAccount = "service_account"
	kindUser           = "authorized_user"
	kindExternal       = "external_account"
	kindExternalUser   = "external_account_authorized_user"
	kindImpersonated   = "impersonated_service_account"
	kindGDCH           = "gdch_service_account"
	kindCompute        = "compute_engine"
	kindDefault        = "application_default"
)

var (
	ErrNoCredentials    = errors.New("no Google credentials were found, set VERTEX_CREDENTIALS_PATH or GOOGLE_APPLICATION_CREDENTIALS to a service account key or run `gcloud auth application-default login`")
	ErrAmbientRefused   = errors.New("Vertex ADC skipped for this profile: GOOGLE_APPLICATION_CREDENTIALS is set in the process environment (from another profile's .env) but not in this profile's own config. Set VERTEX_CREDENTIALS_PATH in this profile's .env instead of relying on ADC.")
	ErrTokenUnavailable = errors.New("Vertex AI OAuth token could not be minted (see the Vertex credential settings)")
	ErrReauthRequired   = errors.New("reauthentication is needed, run `gcloud auth application-default login` to reauthenticate")
	ErrNoProject        = errors.New("no Google Cloud project could be determined, set VERTEX_PROJECT_ID or vertex.project_id")
)

type Error struct {
	Kind      string
	Message   string
	Code      string
	Status    int
	Retryable bool
	Err       error
}

func (e *Error) Error() string {
	text := "google " + e.Kind + " credentials: " + e.Message
	if e.Status != 0 {
		text += " (HTTP " + strconv.Itoa(e.Status) + ")"
	}
	return text
}

func (e *Error) Unwrap() error { return e.Err }

type Env func(name string) string

func ProcessEnv(name string) string { return os.Getenv(name) }

type minter func(ctx context.Context) (string, time.Time, error)

type credentials struct {
	kind           string
	universe       string
	quotaProject   string
	mint           minter
	lookup         func(ctx context.Context) (string, error)
	allowed        func(ctx context.Context, token string) (string, error)
	universeLookup func(ctx context.Context) (string, error)

	mu       sync.Mutex
	token    string
	expiry   time.Time
	boundary *string
}

func boundaryEnabled() (bool, error) {
	value, ok := os.LookupEnv(boundaryVariable)
	if !ok {
		return false, nil
	}
	switch strings.ToLower(value) {
	case "true", "1":
		return true, nil
	case "false", "0":
		return false, nil
	}
	return false, &Error{Kind: kindDefault, Message: `Environment variable "` + boundaryVariable + `" must be one of "true", "false", "1", or "0".`}
}

func (c *credentials) refreshBoundaryLocked(ctx context.Context) error {
	if c.allowed == nil {
		return nil
	}
	enabled, err := boundaryEnabled()
	if err != nil || !enabled {
		return err
	}
	universe := c.domain()
	if c.universeLookup != nil {
		if universe, err = c.universeLookup(ctx); err != nil {
			return err
		}
	}
	if universe != defaultUniverse || (c.boundary != nil && *c.boundary == noOpLocations) {
		return nil
	}
	locations, err := c.allowed(ctx, c.token)
	if err != nil {
		if errors.Is(err, errBoundaryRefresh) && c.boundary != nil {
			slog.Debug("Using cached trust boundary due to refresh error.", "error", err)
			return nil
		}
		return err
	}
	c.boundary = &locations
	return nil
}

func (c *credentials) String() string { return "google " + c.kind + " credentials" }

func (c *credentials) GoString() string { return c.String() }

func (c *credentials) LogValue() slog.Value { return slog.StringValue(c.String()) }

func (c *credentials) refreshLocked(ctx context.Context) error {
	token, expiry, err := c.mint(ctx)
	if err != nil {
		return err
	}
	if token == "" {
		return &Error{Kind: c.kind, Message: "token endpoint reply carried no access token"}
	}
	c.token, c.expiry = token, expiry
	return c.refreshBoundaryLocked(ctx)
}

func (c *credentials) current() (string, time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.token, c.expiry
}

func (c *credentials) ensure(ctx context.Context, stale func(remaining time.Duration) bool) (string, time.Time, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.token == "" || (!c.expiry.IsZero() && stale(time.Until(c.expiry))) {
		if err := c.refreshLocked(ctx); err != nil {
			return "", time.Time{}, err
		}
	}
	return c.token, c.expiry, nil
}

func libraryStale(remaining time.Duration) bool { return remaining <= refreshThreshold }

func vertexStale(remaining time.Duration) bool { return remaining < vertexRefreshSkew }

func (c *credentials) authorize(ctx context.Context, h http.Header) error {
	token, _, err := c.ensure(ctx, libraryStale)
	if err != nil {
		return err
	}
	h.Set("Authorization", "Bearer "+token)
	c.mu.Lock()
	boundary := c.boundary
	c.mu.Unlock()
	if boundary != nil {
		if *boundary == noOpLocations {
			h.Set(allowedLocationsHeader, "")
		} else {
			h.Set(allowedLocationsHeader, *boundary)
		}
	}
	if c.quotaProject != "" {
		h.Set("x-goog-user-project", c.quotaProject)
	}
	return nil
}

func (c *credentials) domain() string {
	if c.universe == "" {
		return defaultUniverse
	}
	return c.universe
}
