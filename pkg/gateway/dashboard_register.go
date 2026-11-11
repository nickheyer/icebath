package gateway

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/internal/pyurl"
	"github.com/nickheyer/icebath/pkg/provider"
)

const defaultDashboardPortal = "https://portal.nousresearch.com"

var (
	dashboardNameAdjectives = []string{
		"amber", "bold", "brave", "bright", "calm", "clever", "cosmic", "crisp",
		"dreamy", "eager", "electric", "fancy", "gentle", "golden", "happy",
		"hidden", "jolly", "keen", "lively", "lucid", "lunar", "mellow", "merry",
		"mighty", "nimble", "noble", "polished", "quiet", "quirky", "rapid",
		"serene", "sharp", "shiny", "silent", "snappy", "solar", "spry", "stellar",
		"sunny", "swift", "tidy", "vivid", "vibrant", "witty", "zesty"}
	dashboardNameNouns = []string{
		"albatross", "antelope", "badger", "beacon", "comet", "condor", "cypress",
		"dolphin", "ember", "falcon", "ferret", "galaxy", "glacier", "harbor",
		"heron", "ibex", "jaguar", "kestrel", "lantern", "lynx", "meadow", "nebula",
		"ocelot", "orchid", "otter", "panther", "petrel", "quasar", "raven", "reef",
		"sparrow", "summit", "tundra", "vortex", "walrus", "willow", "yarrow",
		"kepler", "tesla", "curie", "hopper", "turing", "lovelace"}
)

// Host facts of dashboard registration, swapped by tests
var (
	registerResolveAccessToken = func(ctx context.Context, h string) (string, error) {
		return provider.ResolveNousAccessToken(provider.PoolEnv{Store: provider.NewStore(h), Ctx: ctx}, provider.NousAccessTokenOptions{})
	}
	registerNousAuthState = func(h string) (map[string]any, error) { return provider.NewStore(h).GetProviderAuthState("nous") }
	registerIsManaged     = func(ctx context.Context, h string) bool { return config.ManagedSystem(ctx, h) != "" }
	registerResolvePortal = resolvePortalBaseURL
	registerGetEnvValue   = config.GetEnvValue
	registerSaveEnvValue  = config.SaveEnvValue
	registerHTTPDo        = func(req *http.Request, timeout time.Duration) (*http.Response, error) {
		return (&http.Client{Timeout: timeout}).Do(req)
	}
)

// Failure of a portal registration carrying the user facing message
type dashboardRegisterError struct{ message string }

func (e *dashboardRegisterError) Error() string { return e.message }

// Docker style adjective and noun label for a first registration
func generateDashboardName() string {
	return dashboardNameAdjectives[rand.IntN(len(dashboardNameAdjectives))] + "_" + dashboardNameNouns[rand.IntN(len(dashboardNameNouns))]
}

// Portal base URL from the override, else the stored login portal, else production
func resolvePortalBaseURL(h, override string) string {
	if pystr.Strip(override) != "" {
		return strings.TrimRight(override, "/")
	}
	state, err := registerNousAuthState(h)
	if err != nil {
		return defaultDashboardPortal
	}
	chosen := provider.DefaultNousPortalURL
	if base, ok := state["portal_base_url"].(string); ok && pystr.Strip(base) != "" {
		chosen = base
	}
	return strings.TrimRight(chosen, "/")
}

// Posts the registration to the portal self hosted client endpoint and returns its JSON body
func registerSelfHostedClient(ctx context.Context, accessToken, portalBaseURL, name, customRedirectURI, existingClientID string, timeout time.Duration) (*pyjson.Object, error) {
	body := pyjson.NewObject()
	for _, field := range [][2]string{{"name", name}, {"custom_redirect_uri", customRedirectURI}, {"client_id", existingClientID}} {
		if field[1] != "" {
			body.Set(field[0], field[1])
		}
	}
	data, err := pyjson.Dumps(body)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(portalBaseURL, "/")+"/api/oauth/self-hosted-client", strings.NewReader(data))
	if err != nil {
		return nil, &dashboardRegisterError{fmt.Sprintf("Could not reach Nous Portal at %s: %s", portalBaseURL, err)}
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := registerHTTPDo(req, timeout)
	if err != nil {
		reason := err
		var urlErr *url.Error
		if errors.As(err, &urlErr) {
			reason = urlErr.Err
		}
		return nil, &dashboardRegisterError{fmt.Sprintf("Could not reach Nous Portal at %s: %s", portalBaseURL, reason)}
	}
	defer resp.Body.Close()
	raw, readErr := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		detail := ""
		if errBody, err := pyjson.Loads(pystr.DecodeUTF8Replace(raw)); err == nil && readErr == nil {
			if obj, ok := errBody.(*pyjson.Object); ok {
				for _, key := range []string{"error_description", "error"} {
					if v := obj.GetOr(key, nil); pyTruthy(v) {
						detail = pyrepr.Str(v)
						break
					}
				}
			}
		}
		message := fmt.Sprintf("Portal returned HTTP %d", resp.StatusCode)
		switch {
		case resp.StatusCode == 401:
			message = "Nous Portal rejected the access token (401). Try `icebath auth add nous` to re-authenticate."
		case resp.StatusCode == 403:
			message = detail
			if message == "" {
				message = "Your account is not permitted to register a self-hosted dashboard."
			}
		case detail != "":
			message += ": " + detail
		}
		return nil, &dashboardRegisterError{message}
	}
	if readErr != nil {
		return nil, readErr
	}
	payload, err := pyjson.Loads(string(raw))
	if err != nil {
		return nil, err
	}
	obj, ok := payload.(*pyjson.Object)
	if !ok || !pyTruthy(obj.GetOr("client_id", nil)) {
		return nil, &dashboardRegisterError{"Portal returned an unexpected response (no client_id)."}
	}
	return obj, nil
}

// Prints what was written to the env file and how to engage the login gate
func printPostRegisterHint(w io.Writer, h, clientID, portalBaseURL, customRedirectURI string, wrotePortalURL bool, publicURL string) {
	fmt.Fprintf(w, "\n  Wrote to %s:\n    ICEBATH_DASHBOARD_OAUTH_CLIENT_ID=%s\n", home.EnvPath(h), clientID)
	if wrotePortalURL {
		fmt.Fprintln(w, "    ICEBATH_DASHBOARD_PORTAL_URL="+portalBaseURL)
	}
	if publicURL != "" {
		fmt.Fprintln(w, "    ICEBATH_DASHBOARD_PUBLIC_URL="+publicURL)
	}
	fmt.Fprint(w, "\n  Heads up — Nous login only *engages* on a non-loopback bind. A plain\n"+
		"  `icebath dashboard` (localhost) leaves the gate off and serves locally\n"+
		"  without auth, which is fine for your own machine.\n\n")
	if customRedirectURI != "" {
		host := "your-host"
		if parsed, err := pyurl.Parse(customRedirectURI); err == nil && parsed.Hostname() != "" {
			host = parsed.Hostname()
		}
		fmt.Fprintln(w, "  To require Nous login on your registered host, run the dashboard\n"+
			"  bound publicly (it must be reachable at https://"+host+") and log in\n"+
			"  at its /login page.")
	} else {
		fmt.Fprintln(w, "  To require Nous login (e.g. exposing on your LAN or a public host):\n"+
			"    icebath dashboard --host 0.0.0.0\n"+
			"  …then log in at the dashboard's /login page.")
	}
	fmt.Fprintln(w, "\n  If the dashboard is already running, restart it to pick up the new env.\n"+
		"  Manage or revoke this dashboard at "+portalBaseURL+"/local-dashboards")
}

// Stored env file value of key, not ok when unset or unreadable
func registerEnvValue(ctx context.Context, h, key string) (string, bool) {
	v, ok, err := registerGetEnvValue(ctx, h, key)
	if err != nil {
		return "", false
	}
	return v, ok
}

// Persists one env file assignment and reports whether the write succeeded
func saveEnvQuietly(h, key, value string) bool {
	return registerSaveEnvValue(h, key, value) == nil
}

// Scheme and authority of an http or https redirect URI, empty for anything else
func publicURLFromRedirect(redirectURI string) string {
	parsed, err := pyurl.Parse(redirectURI)
	if err != nil {
		return ""
	}
	if (parsed.Scheme == "http" || parsed.Scheme == "https") && parsed.Netloc != "" {
		return parsed.Scheme + "://" + parsed.Netloc
	}
	return ""
}

// Registers a self hosted dashboard OAuth client with Nous Portal and writes its client id to the env file
func CmdDashboardRegister(ctx context.Context, ns map[string]any, w io.Writer) (int, error) {
	h := home.Dir(ctx)
	if registerIsManaged(ctx, h) {
		fmt.Fprintln(w, "✗ `icebath dashboard register` is not available in a managed/hosted install.\n"+
			"  The dashboard OAuth client is provisioned by the hosting platform.")
		return 1, nil
	}
	accessToken, err := registerResolveAccessToken(ctx, h)
	if err != nil {
		var authErr *provider.AuthError
		if errors.As(err, &authErr) && authErr.ReloginRequired {
			fmt.Fprintln(w, "✗ You're not logged into Nous Portal.\n"+
				"  Run `icebath setup` (or `icebath auth add nous`) first, then retry.")
		} else {
			fmt.Fprintf(w, "✗ Could not resolve a Nous Portal access token: %s\n", err)
		}
		return 1, nil
	}
	portalOverride := dashArgString(ns, "portal_url")
	if portalOverride == "" {
		portalOverride = os.Getenv("ICEBATH_DASHBOARD_PORTAL_URL")
	}
	customPortalSupplied := pystr.Strip(portalOverride) != ""
	portalBaseURL := registerResolvePortal(h, portalOverride)
	existingClientID := ""
	if stored, ok := registerEnvValue(ctx, h, "ICEBATH_DASHBOARD_OAUTH_CLIENT_ID"); ok {
		existingClientID = pystr.Strip(stored)
	}
	name := dashArgString(ns, "name")
	if name == "" && existingClientID == "" {
		name = generateDashboardName()
	}
	customRedirectURI := dashArgString(ns, "redirect_uri")
	result, err := registerSelfHostedClient(ctx, accessToken, portalBaseURL, name, customRedirectURI, existingClientID, 15*time.Second)
	if err != nil {
		var regErr *dashboardRegisterError
		if !errors.As(err, &regErr) {
			return 1, err
		}
		fmt.Fprintf(w, "✗ Registration failed: %s\n", regErr.message)
		return 1, nil
	}
	clientID := pyrepr.Str(result.GetOr("client_id", nil))
	registeredName := name
	if v := result.GetOr("name", nil); pyTruthy(v) {
		registeredName = pyrepr.Str(v)
	}
	verb := "Registered"
	if existingClientID != "" && clientID == existingClientID {
		verb = "Updated"
	}
	fmt.Fprintf(w, "✓ %s dashboard \"%s\"\n", verb, registeredName)
	if err := registerSaveEnvValue(h, "ICEBATH_DASHBOARD_OAUTH_CLIENT_ID", clientID); err != nil {
		fmt.Fprintf(w, "✗ Failed to write ICEBATH_DASHBOARD_OAUTH_CLIENT_ID to .env: %s\n  Set it manually:  ICEBATH_DASHBOARD_OAUTH_CLIENT_ID=%s\n", err, clientID)
		return 1, nil
	}
	existingPortal, havePortal := registerEnvValue(ctx, h, "ICEBATH_DASHBOARD_PORTAL_URL")
	shouldWritePortal := false
	if customPortalSupplied {
		shouldWritePortal = !havePortal || existingPortal != portalBaseURL
	} else {
		shouldWritePortal = existingPortal == "" && strings.TrimRight(portalBaseURL, "/") != defaultDashboardPortal
	}
	wrotePortalURL := shouldWritePortal && saveEnvQuietly(h, "ICEBATH_DASHBOARD_PORTAL_URL", portalBaseURL)
	publicURL := publicURLFromRedirect(customRedirectURI)
	wrotePublicURL := false
	if publicURL != "" {
		if existing, ok := registerEnvValue(ctx, h, "ICEBATH_DASHBOARD_PUBLIC_URL"); !ok || existing != publicURL {
			wrotePublicURL = saveEnvQuietly(h, "ICEBATH_DASHBOARD_PUBLIC_URL", publicURL)
		}
	}
	shownPublicURL := ""
	if wrotePublicURL {
		shownPublicURL = publicURL
	}
	printPostRegisterHint(w, h, clientID, portalBaseURL, customRedirectURI, wrotePortalURL, shownPublicURL)
	return 0, nil
}
