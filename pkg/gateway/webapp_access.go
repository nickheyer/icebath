package gateway

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyfs"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/internal/pyurl"
	"github.com/nickheyer/icebath/pkg/pm"
	"github.com/nickheyer/icebath/pkg/provider"
)

// Remote access cannot be set up as asked, its message the user facing reason
type WebappAccessError struct{ Msg string }

func (e *WebappAccessError) Error() string      { return e.Msg }
func (e *WebappAccessError) PyTypeName() string { return "WebappAccessError" }

var webappAccessLoopbackHosts = map[string]bool{"127.0.0.1": true, "::1": true, "localhost": true}

// Host facts of Web Access, swapped by tests
var (
	webappAccessSignedIn = webappNousSignedIn
	webappAccessPortFree = webappPortFree
	webappAccessFileLock = provider.FileLock
)

const (
	webappPortSearch       = 100
	webappPortClaimLock    = "webapp/port-claim.lock"
	webappPortClaimTimeout = 10 * time.Second
)

// Https public URL without a trailing slash that the gateway reads and other devices reach
func webappAccessPublicURL(raw string) (string, error) {
	value := pystr.Strip(raw)
	url, _ := webappPublicURL(value)
	if url == "" && strings.ContainsAny(value, "?#@") {
		return "", &WebappAccessError{"The public URL cannot carry credentials, a query or a fragment."}
	}
	parts, err := pyurl.Split(url)
	if err != nil || parts.Scheme != "https" || webappAccessLoopbackHosts[parts.Hostname()] {
		return "", &WebappAccessError{"The public URL must be an https:// address other devices use."}
	}
	return url, nil
}

// Home whose gateway serves the home in scope, its config holding the Webapp settings
func WebappGatewayHome(ctx context.Context) (string, error) {
	return webappGatewayHome(ctx, home.Dir(ctx))
}

// Whether the home in scope holds a Nous account to register sign in under, read without a refresh
func webappNousSignedIn(ctx context.Context) bool {
	status := provider.GetNousAuthStatusLocal(provider.PoolEnv{Store: provider.NewStore(home.Dir(ctx)), Ctx: ctx})
	return pyrepr.Truthy(status["logged_in"]) && !pyrepr.Truthy(status["free_tier"])
}

// Portal client id and portal Web Access registered for a home Webapp, false for none or a dashboard client
func webappSignInClient(h string) (string, string, bool) {
	record := webappRegistration(h)
	portal, ok := record["portal"].(string)
	if record["own_client"] != true || !ok || portal == "" {
		return "", "", false
	}
	clientID, ok := record["client_id"].(string)
	if !ok || !strings.HasPrefix(clientID, "agent:") {
		return "", "", false
	}
	return clientID, portal, true
}

// Registers Web Access own portal client with the callback, nothing posted when it already carries it
func webappRegisterNousSignIn(ctx context.Context, callbackURL string) error {
	h := home.Dir(ctx)
	portal := resolvePortalBaseURL(h, os.Getenv("ICEBATH_DASHBOARD_PORTAL_URL"))
	existing := ""
	if id, p, ok := webappSignInClient(h); ok && p == portal {
		existing = id
	}
	if existing != "" && webappRegistration(h)["callback"] == callbackURL {
		return nil
	}
	fail := func(err error) error {
		return &WebappAccessError{fmt.Sprintf("Couldn't register Nous sign-in: %s", err.Error())}
	}
	token, err := registerResolveAccessToken(ctx, h)
	if err != nil {
		return fail(err)
	}
	name := ""
	if existing == "" {
		name = "webapp_" + generateDashboardName()
	}
	result, err := registerSelfHostedClient(ctx, token, portal, name, callbackURL, existing, 15*time.Second)
	if err != nil {
		return fail(err)
	}
	path := filepath.Join(h, filepath.FromSlash(webappRegistrationPath))
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	record := pyjson.ObjectOf("own_client", true, "portal", portal, "client_id", pyrepr.Str(result.GetOr("client_id", nil)), "callback", callbackURL)
	text, err := pyjson.Dumps(record)
	if err != nil {
		return err
	}
	return pyfs.AtomicWriteTextMode(ctx, path, text+"\n", ".tmp_", 0o600)
}

// Bind host, port to write or zero, URL to pin and sign in callback of an access mode
func WebappAccessPlan(ctx context.Context, mode, publicURL string) (string, int, string, string, error) {
	signedOut := &WebappAccessError{"Sign in to your Nous account under Providers first."}
	if !webappAccessSignedIn(ctx) {
		return "", 0, "", "", signedOut
	}
	var host, pinned, url string
	var port int
	switch mode {
	case "lan":
		address := wl.lanAddress()
		if address == "" {
			return "", 0, "", "", &WebappAccessError{"This machine has no network address other devices can reach."}
		}
		host = "0.0.0.0"
		gh, err := WebappGatewayHome(ctx)
		if err != nil {
			return "", 0, "", "", err
		}
		serving, write, err := webappPortToServe(ctx, gh, host)
		if err != nil {
			return "", 0, "", "", err
		}
		port, url = write, fmt.Sprintf("http://%s:%d", address, serving)
	case "public":
		host = "127.0.0.1"
		u, err := webappAccessPublicURL(publicURL)
		if err != nil {
			return "", 0, "", "", err
		}
		url, pinned = u, u
		gh, err := WebappGatewayHome(ctx)
		if err != nil {
			return "", 0, "", "", err
		}
		if _, port, err = webappPortToServe(ctx, gh, host); err != nil {
			return "", 0, "", "", err
		}
	default:
		return "", 0, "", "", &WebappAccessError{fmt.Sprintf("Unknown access mode %s.", pyrepr.Repr(mode))}
	}
	if !webappAccessSignedIn(ctx) {
		return "", 0, "", "", signedOut
	}
	return host, port, pinned, url + "/auth/callback", nil
}

// Port the home gateway Webapp serves on host and the port to write, a standalone profile claiming a free one
func webappPortToServe(ctx context.Context, h, host string) (int, int, error) {
	profile := home.NamedProfileHome(h)
	own, err := webappOwnPort(h)
	if err != nil {
		return 0, 0, err
	}
	if own != nil || profile == "" {
		return WebappSettingsOf(ctx, h).Port, 0, nil
	}
	lock := filepath.Join(filepath.Dir(filepath.Dir(profile)), filepath.FromSlash(webappPortClaimLock))
	if err := webappCreateUserOnly(lock); err != nil {
		return 0, 0, err
	}
	release, err := webappAccessFileLock(lock, webappPortClaimTimeout, "Timed out waiting for another Icebath profile to choose its Webapp port")
	if err != nil {
		var timeout *provider.FileLockTimeoutError
		if errors.As(err, &timeout) {
			return 0, 0, &WebappAccessError{err.Error() + ". Start again in a moment."}
		}
		return 0, 0, err
	}
	port, err := webappClaimFreePort(ctx, h, host)
	release()
	if err != nil || port != 0 {
		return port, port, err
	}
	return 0, 0, &WebappAccessError{fmt.Sprintf("No free port for the Webapp from %d to %d. Set webapp.port in %s to a free port, then start again.",
		webappDefaultPort+1, webappDefaultPort+webappPortSearch, filepath.Join(h, "config.yaml"))}
}

// First port above the default no home claims and nothing holds, saved to the home in scope, zero for none
func webappClaimFreePort(ctx context.Context, h, host string) (int, error) {
	claimed, err := webappClaimedPorts(h)
	if err != nil {
		return 0, err
	}
	for port := webappDefaultPort + 1; port < webappDefaultPort+1+webappPortSearch; port++ {
		if !claimed[port] && webappAccessPortFree(host, port) {
			return port, webappSavePort(ctx, port)
		}
	}
	return 0, nil
}

// Creates the lock file user only in a user only folder closed again to other users
func webappCreateUserOnly(lock string) error {
	if err := os.MkdirAll(filepath.Dir(lock), 0o700); err != nil {
		return err
	}
	webappCloseToOtherUsers(filepath.Dir(lock))
	f, err := os.OpenFile(lock, os.O_WRONLY|os.O_CREATE, 0o600)
	if err != nil {
		return err
	}
	return f.Close()
}

// Claims the port as the webapp port of the home in scope, changing nothing else
func webappSavePort(ctx context.Context, port int) error {
	h := home.Dir(ctx)
	cfg, err := config.LoadContext(ctx, h, config.EnvRefLookupContext(ctx))
	if err != nil {
		return err
	}
	block := map[string]any{}
	if existing, ok := cfg["webapp"].(map[string]any); ok {
		for k, v := range existing {
			block[k] = v
		}
	}
	block["port"] = port
	cfg["webapp"] = block
	return config.SaveConfig(ctx, h, cfg, config.SaveOptions{})
}

// Webapp port configured for the home gateway, valid or not, nil when unset or the config is unreadable
func webappOwnPort(h string) (any, error) {
	document, err := config.LoadUserEffective(filepath.Join(h, "config.yaml"), config.EnvRefLookup)
	if err != nil {
		if pypath.IsOSError(err) {
			return nil, nil
		}
		return nil, err
	}
	block, ok := document["webapp"].(map[string]any)
	if !ok {
		return nil, nil
	}
	return block["port"], nil
}

// Integer of a config value as isinstance int reads it, a bool counting as one
func webappConfigInt(v any) (int, bool, bool) {
	switch n := v.(type) {
	case bool:
		if n {
			return 1, true, true
		}
		return 0, true, true
	case int:
		return n, true, false
	case int64:
		return int(n), true, false
	case uint64:
		return int(n), true, false
	}
	return 0, false, false
}

// Webapp ports the other homes beside the home claim, the default profile and every other profile own port
func webappClaimedPorts(h string) (map[int]bool, error) {
	profile := home.NamedProfileHome(h)
	claimed := map[int]bool{}
	if profile == "" {
		return claimed, nil
	}
	profilesDir := filepath.Dir(profile)
	rootPort, err := webappOwnPort(filepath.Dir(profilesDir))
	if err != nil {
		return nil, err
	}
	if n, ok, _ := webappConfigInt(rootPort); ok {
		claimed[n] = true
	} else {
		claimed[webappDefaultPort] = true
	}
	if st, err := os.Stat(profilesDir); err != nil || !st.IsDir() {
		return claimed, nil
	}
	entries, err := os.ReadDir(profilesDir)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	sort.Strings(names)
	for _, name := range names {
		other := filepath.Join(profilesDir, name)
		if filepath.Clean(other) == filepath.Clean(profile) {
			continue
		}
		if st, err := os.Stat(other); err != nil || !st.IsDir() {
			continue
		}
		port, err := webappOwnPort(other)
		if err != nil {
			return nil, err
		}
		if n, ok, isBool := webappConfigInt(port); ok && !isBool {
			claimed[n] = true
		}
	}
	return claimed, nil
}

// Whether a listener could bind host and port now, as the gateway listener does
func webappPortFree(host string, port int) bool {
	network := "tcp4"
	if strings.Contains(host, ":") {
		network = "tcp6"
	}
	l, err := net.Listen(network, net.JoinHostPort(host, strconv.Itoa(port)))
	if err != nil {
		return false
	}
	_ = l.Close()
	return true
}

// Builds the Webapp bundle the gateway serves, a build failure answered with how to see its output
func WebappAccessBuildBundle(ctx context.Context) error {
	_, err := WebappRendererBuild.Prepare(ctx, pm.RepoRoot(), false, false, false)
	if err != nil && WebappRendererBuild.ExceptionName(err) == "WebappBuildError" {
		return &WebappAccessError{strings.TrimRight(err.Error(), ".") + ". Run icebath webapp --build-only to see the full output, then start again."}
	}
	return err
}

// Registers Nous sign in for the callback on Web Access own portal client unless it already carries it
func WebappAccessRegister(ctx context.Context, callbackURL string) error {
	return webappRegisterNousSignIn(ctx, callbackURL)
}

// Serves the Webapp on host and the planned port from the gateway next start
func WebappAccessServe(ctx context.Context, host, publicURL string, port int) error {
	return webappWriteConfig(ctx, true, host, publicURL, port)
}

// Stops serving the Webapp from the gateway next start
func WebappAccessTurnOff(ctx context.Context) error {
	return webappWriteConfig(ctx, false, "", "", 0)
}

// Writes the webapp block of the home in scope and nothing outside it
func webappWriteConfig(ctx context.Context, enabled bool, host, publicURL string, port int) error {
	h := home.Dir(ctx)
	cfg, err := config.LoadContext(ctx, h, config.EnvRefLookupContext(ctx))
	if err != nil {
		return err
	}
	block, ok := cfg["webapp"].(map[string]any)
	if !ok {
		block = map[string]any{}
	}
	block["enabled"] = enabled
	if enabled {
		block["host"] = host
	}
	if port != 0 {
		block["port"] = port
	}
	if enabled && publicURL != "" {
		block["public_url"] = publicURL
	} else if enabled {
		delete(block, "public_url")
	}
	cfg["webapp"] = block
	return config.SaveConfig(ctx, h, cfg, config.SaveOptions{})
}

// What the home gateway serves now, stopped when none answers and off for a gateway older than the Webapp
func webappAccessGatewayState(h string) map[string]any {
	response, err := wl.control(h, "webapp-status", nil)
	if err != nil {
		return map[string]any{"state": "stopped", "url": nil, "reason": nil}
	}
	if olderGateway(response) {
		return map[string]any{"state": "off", "url": nil, "reason": webappOlderGateway}
	}
	if !pyrepr.Truthy(response["ok"]) {
		reason := ""
		if pyrepr.Truthy(response["error"]) {
			reason = pyrepr.Str(response["error"])
		}
		return map[string]any{"state": "unavailable", "url": nil, "reason": reason}
	}
	result, ok := response["result"].(map[string]any)
	if !ok {
		return map[string]any{}
	}
	out := make(map[string]any, len(result))
	for k, v := range result {
		out[k] = v
	}
	return out
}

// Who the configured Webapp serves, public, lan or local, nil when it is off
func webappAccessMode(settings WebappSettings) any {
	if !settings.Enabled {
		return nil
	}
	if settings.PublicURL != "" {
		return "public"
	}
	if webappIsWildcard(settings.Host) {
		return "lan"
	}
	return "local"
}

// Everything the Web Access pane shows for the gateway home in scope
func WebappAccessStatus(ctx context.Context) (*pyjson.Object, error) {
	gh, err := WebappGatewayHome(ctx)
	if err != nil {
		return nil, err
	}
	settings := WebappSettingsOf(ctx, gh)
	var lan, public any
	if address := wl.lanAddress(); address != "" {
		lan = address
	}
	if settings.PublicURL != "" {
		public = settings.PublicURL
	}
	return pyjson.ObjectOf("enabled", settings.Enabled, "mode", webappAccessMode(settings), "port", settings.Port,
		"gateway", webappAccessGatewayState(gh), "lan_address", lan, "public_url", public,
		"nous_signed_in", webappAccessSignedIn(ctx)), nil
}
