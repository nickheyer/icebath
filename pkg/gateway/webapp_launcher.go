package gateway

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"net"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/hostinfo"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pyshlex"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/pm"
	"github.com/nickheyer/icebath/pkg/profiles"
	"github.com/nickheyer/icebath/pkg/provider"
)

const (
	webappRestartCommand  = "icebath gateway restart"
	webappRetryAfterFix   = "The gateway reads these settings when it starts: after fixing them, run " + webappRestartCommand + "."
	webappStartedBeforeOn = "The gateway started before the Webapp was turned on. Restart it to serve the Webapp: " +
		webappRestartCommand
	webappOlderGateway = "The running gateway is older than the Webapp and does not know its commands. Restart it " +
		"onto this version of Icebath: " + webappRestartCommand
	webappPlainHTTPNotice = "This address is plain HTTP, so the browser turns off what needs a secure connection: the bot " +
		"screen, the microphone (dictation, live voice and the remote wake word), reading the clipboard " +
		"(terminal paste, pasting an image) and system notifications. Open the Webapp over HTTPS " +
		"(webapp.public_url) or through an SSH tunnel to 127.0.0.1 to use them."
	webappStartWaitAttempts = 60
	webappStartWaitInterval = 500 * time.Millisecond
	// Page a private launch link reaches the browser in, moving on to the link at once
	webappLaunchPage = `<!doctype html><meta charset="utf-8"><meta name="referrer" content="no-referrer">` +
		`<meta http-equiv="Content-Security-Policy" content="default-src 'none'">` +
		`<meta http-equiv="refresh" content="0;url={url}"><title>Opening Icebath</title>` +
		`<p><a href="{url}">Open Icebath Webapp</a></p>` + "\n"
	// Record of the Nous sign-in Web Access registered for a home's Webapp
	webappRegistrationPath = "webapp/sign-in-registration.json"
)

// Hostnames the dashboard treats as loopback binds
var loopbackHostValues = map[string]bool{"localhost": true, "127.0.0.1": true, "::1": true}

// Webapp renderer build steps of the self update package, assigned by the command wiring
var WebappRendererBuild struct {
	Prepare       func(ctx context.Context, projectRoot string, force, skipBuild, explicit bool) (string, error)
	PackagedDist  func() string
	ExceptionName func(err error) string
}

// Steps of the webapp command
type webappLauncherSteps struct {
	build            func(ctx context.Context, ns map[string]any) error
	ensure           func(ctx context.Context, h string) error
	control          func(h, verb string, params map[string]any) (map[string]any, error)
	open             func(target string) bool
	gatewayState     func(ctx context.Context, h string) string
	sleep            func(time.Duration)
	isWSL            func() bool
	readableByOthers func(path string) bool
	lanAddress       func() string
}

// Steps the webapp command runs, assigned at package init
var wl webappLauncherSteps

func init() {
	wl = webappLauncherSteps{
		build:            webappBuild,
		ensure:           webappEnsure,
		control:          webappControl,
		open:             webappOpen,
		gatewayState:     webappGatewayState,
		sleep:            time.Sleep,
		isWSL:            hostinfo.IsWSL,
		readableByOthers: webappReadableByOtherUsers,
		lanAddress:       lanAddress,
	}
}

// What a home's gateway lock and control socket say, absent, starting, ready or why it cannot be reached
func webappGatewayState(ctx context.Context, h string) string {
	return DiscoverGatewayEndpoint(ctx, h, 2*time.Second).State
}

// Gateway for a home that did not start or does not answer on its control socket
type gatewayUnreachable struct{ msg string }

func (e *gatewayUnreachable) Error() string { return e.msg }

// Home whose gateway serves a home, the default gateway of a served named profile else its own
func webappGatewayHome(ctx context.Context, h string) (string, error) {
	root, err := ImpliedHostRoot(ctx, h)
	if err != nil {
		return "", err
	}
	if root != "" {
		return root, nil
	}
	return h, nil
}

// Refusal of a deleted or never created named profile home, empty for any other home
func deletedProfileRefusal(h string) string {
	profile := home.NamedProfileHome(h)
	if profile == "" || home.ProfileLive(profile) {
		return ""
	}
	return profiles.MissingProfileError(filepath.Base(profile)).Error()
}

// Builds the Webapp renderer the namespace asks for
func webappBuild(ctx context.Context, ns map[string]any) error {
	force := dashArgBool(ns, "force_build")
	_, err := WebappRendererBuild.Prepare(ctx, pm.RepoRoot(), force, dashArgBool(ns, "skip_build"), force || dashArgBool(ns, "build_only"))
	return err
}

// Starts the gateway for a home, unreachable when it cannot be attached
func webappEnsure(ctx context.Context, h string) error {
	result, err := EnsureGatewayRuntime(ctx, h, DefaultEnsureTimeout.Seconds(), false)
	if err != nil {
		return err
	}
	if !Attachable(result) {
		var parts []string
		detail := result.Detail
		if detail == "" {
			detail = result.ReasonCode
		}
		for _, part := range []string{result.State, detail} {
			if part != "" {
				parts = append(parts, part)
			}
		}
		return &gatewayUnreachable{fmt.Sprintf("The gateway did not start (%s).", strings.Join(parts, ": "))}
	}
	return nil
}

// Sends one control verb to a home's gateway and returns its decoded answer, unreachable on any failure
func webappControl(h, verb string, params map[string]any) (map[string]any, error) {
	if params == nil {
		params = map[string]any{}
	}
	request, err := json.Marshal(map[string]any{"protocol": 1, "id": 1, "verb": verb, "params": params})
	if err != nil {
		return nil, &gatewayUnreachable{err.Error()}
	}
	data, err := privateControlQuery(h, append(request, '\n'), 5*time.Second)
	if err != nil {
		return nil, &gatewayUnreachable{err.Error()}
	}
	line := string(data)
	if i := strings.IndexByte(line, '\n'); i >= 0 {
		line = line[:i]
	}
	decoded, err := pyjson.Loads(line)
	if err != nil {
		return nil, &gatewayUnreachable{err.Error()}
	}
	answer, ok := pyjson.Plain(decoded).(map[string]any)
	if !ok {
		return nil, &gatewayUnreachable{fmt.Sprintf("'%s' object has no attribute 'get'", pyrepr.TypeName(pyjson.Plain(decoded)))}
	}
	return answer, nil
}

// Opens a URL in a graphical browser, false when none can open it
func webappOpen(target string) bool {
	if !provider.CanOpenGraphicalBrowser() {
		return false
	}
	return provider.OpenBrowserURL(target) == nil
}

// Opens a private launch link through a user only page, false and no page left when that fails
func webappOpenPrivately(target, link string) bool {
	if runtime.GOOS == "windows" || wl.isWSL() {
		return false
	}
	abs, err := filepath.Abs(target)
	if err != nil {
		return false
	}
	folder := filepath.Join(abs, filepath.Dir(WebappTokenPath))
	if err := os.MkdirAll(folder, 0o700); err != nil {
		return false
	}
	webappCloseToOtherUsers(folder)
	if wl.readableByOthers(folder) {
		return false
	}
	earlier, _ := filepath.Glob(filepath.Join(folder, "launch-*.html"))
	for _, page := range earlier {
		if err := os.Remove(page); err != nil && !errors.Is(err, os.ErrNotExist) {
			return false
		}
	}
	token := make([]byte, 8)
	if _, err := rand.Read(token); err != nil {
		return false
	}
	page := filepath.Join(folder, "launch-"+hex.EncodeToString(token)+".html")
	fh, err := os.OpenFile(page, os.O_WRONLY|os.O_CREATE|os.O_EXCL|noFollowFlag, 0o600)
	if err != nil {
		return false
	}
	_, err = fh.WriteString(strings.ReplaceAll(webappLaunchPage, "{url}", html.EscapeString(link)))
	if closeErr := fh.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return false
	}
	uri, err := pypath.AsURI(page)
	if err == nil && wl.open(uri) {
		return true
	}
	_ = os.Remove(page)
	return false
}

// Profile the gateway opens the Webapp on when it serves the home as one of its named profiles
func webappProfileChoice(h, target string) map[string]any {
	a, errA := pypath.Resolve(h)
	b, errB := pypath.Resolve(target)
	if errA == nil && errB == nil && a == b {
		return map[string]any{}
	}
	name, ok := ProfileNameForHome(h)
	if ok && name != "" && name != "default" {
		return map[string]any{"profile": name}
	}
	return map[string]any{}
}

// Rerun command of this webapp invocation, keeping an explicit profile that differs from the sticky one
func webappRerunCommand(explicitProfile string, flags ...string) string {
	parts := []string{"icebath"}
	if explicitProfile != "" && explicitProfile != profiles.GetActiveProfile("") {
		parts = append(parts, "-p", pyshlex.Quote(explicitProfile))
	}
	parts = append(parts, "webapp")
	return strings.Join(append(parts, flags...), " ")
}

// Whether a gateway answer is the unknown verb reply of a gateway older than the Webapp
func olderGateway(response map[string]any) bool {
	return !pyrepr.Truthy(response["ok"]) && strings.HasPrefix(webappErrorText(response["error"], ""), "unknown verb:")
}

// Python str of an error field, the fallback for a falsy one
func webappErrorText(v any, fallback string) string {
	if !pyrepr.Truthy(v) {
		return fallback
	}
	return pyrepr.Str(v)
}

// User facing reason of a refused gateway request
func webappRefusal(response map[string]any) string {
	if olderGateway(response) {
		return webappOlderGateway
	}
	errText := webappErrorText(response["error"], "The gateway refused the request.")
	kind, message, _ := strings.Cut(errText, ": ")
	if strings.HasSuffix(kind, "Error") && message != "" {
		return message
	}
	return errText
}

// Why the home's config keeps the Webapp off and what to do, then the next step
func webappOffMessage(ctx context.Context, target, then string) string {
	settings := WebappSettingsOf(ctx, target)
	if settings.Reason != "" {
		return fmt.Sprintf("The Webapp is off: %s. Fix it, then %s.", settings.Reason, then)
	}
	if settings.Enabled {
		return webappStartedBeforeOn
	}
	return fmt.Sprintf("The Webapp is off. Set webapp.enabled: true in %s, then %s.", filepath.Join(target, "config.yaml"), then)
}

// Plain HTTP note for a non loopback http URL, empty otherwise
func webappPlainHTTPNote(raw string) string {
	parts, err := url.Parse(raw)
	if err != nil || parts.Scheme != "http" {
		return ""
	}
	host := strings.TrimRight(strings.ToLower(parts.Hostname()), ".")
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return ""
	}
	addr, err := netip.ParseAddr(host)
	if err != nil {
		return webappPlainHTTPNotice
	}
	if addr.IsLoopback() {
		return ""
	}
	return webappPlainHTTPNotice
}

// Exact hostname a public URL declares, empty when unset or malformed
func publicURLHosts(publicURL string) []string {
	if publicURL == "" {
		return nil
	}
	parts, err := url.Parse(publicURL)
	if err != nil || parts.Hostname() == "" {
		return nil
	}
	return []string{strings.ToLower(parts.Hostname())}
}

// True for any non loopback bind
func shouldRequireAuth(host string) bool { return !loopbackHostValues[host] }

// True when a declared public host that is not loopback engages the sign in gate
func publicHostsEngageGate(hosts []string) bool {
	for _, h := range hosts {
		if !loopbackHostValues[h] {
			return true
		}
	}
	return false
}

// Primary LAN IPv4 address of this machine from a routing probe that sends nothing, empty when none
func lanAddress() string {
	conn, err := net.Dial("udp4", "10.255.255.255:1")
	if err != nil {
		return ""
	}
	defer conn.Close()
	addr, ok := conn.LocalAddr().(*net.UDPAddr)
	if !ok {
		return ""
	}
	address := addr.IP.String()
	if strings.HasPrefix(address, "127.") || address == "0.0.0.0" {
		return ""
	}
	return address
}

// Registration record of a home, empty when there is none or it cannot be read
func webappRegistration(h string) map[string]any {
	raw, err := os.ReadFile(filepath.Join(h, webappRegistrationPath))
	if err != nil {
		return map[string]any{}
	}
	text, err := pystr.DecodeUTF8Sig(raw)
	if err != nil {
		return map[string]any{}
	}
	decoded, err := pyjson.Loads(text)
	if err != nil {
		return map[string]any{}
	}
	record, ok := pyjson.Plain(decoded).(map[string]any)
	if !ok {
		return map[string]any{}
	}
	return record
}

// LAN origin Web Access registered Nous sign in for at this port, minus one standing for no port
func registeredLANOrigin(h string, port int) string {
	callback, ok := webappRegistration(h)["callback"].(string)
	if !ok {
		return ""
	}
	parts, err := url.Parse(callback)
	if err != nil {
		return ""
	}
	recorded := 0
	if p := parts.Port(); p != "" {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 || n > 65535 {
			return ""
		}
		recorded = n
	} else {
		recorded = -1
	}
	if parts.Scheme != "http" || parts.Path != "/auth/callback" || parts.RawQuery != "" || parts.Fragment != "" || parts.ForceQuery {
		return ""
	}
	hostname := strings.ToLower(parts.Hostname())
	if parts.User != nil || recorded != port || hostname != wl.lanAddress() {
		return ""
	}
	shown := strconv.Itoa(port)
	if port == -1 {
		shown = "None"
	}
	return fmt.Sprintf("http://%s:%s", hostname, shown)
}

// Browser address of the served Webapp, its public URL, else a registered LAN origin, else the served URL
func webappBrowserAddress(target string, settings WebappSettings, servedURL string) string {
	gated := shouldRequireAuth(settings.Host) || publicHostsEngageGate(publicURLHosts(settings.PublicURL))
	lan := ""
	if gated {
		port := -1
		if parts, err := url.Parse(servedURL); err == nil && parts.Port() != "" {
			if n, err := strconv.Atoi(parts.Port()); err == nil {
				port = n
			}
		}
		lan = registeredLANOrigin(target, port)
	}
	if settings.PublicURL != "" {
		return settings.PublicURL
	}
	if lan != "" {
		return lan
	}
	return servedURL
}

// Result object of a gateway answer
func webappResult(response map[string]any) map[string]any {
	result, _ := response["result"].(map[string]any)
	return result
}

// Prints whether the gateway serves the Webapp and returns the exit code
func webappStatus(ctx context.Context, w io.Writer, target string) int {
	response, err := wl.control(target, "webapp-status", nil)
	if err != nil {
		if WebappSettingsOf(ctx, target).Enabled {
			fmt.Fprintln(w, "The gateway is not running. Run icebath webapp to start it.")
		} else {
			fmt.Fprintln(w, webappOffMessage(ctx, target, "run icebath webapp"))
		}
		return 1
	}
	if !pyrepr.Truthy(response["ok"]) {
		fmt.Fprintln(w, webappRefusal(response))
		return 1
	}
	status := webappResult(response)
	switch status["state"] {
	case "serving":
		served := pyrepr.Str(status["url"])
		fmt.Fprintf(w, "Serving at %s\n", served)
		settings := WebappSettingsOf(ctx, target)
		if note := webappPlainHTTPNote(webappBrowserAddress(target, settings, served)); note != "" {
			fmt.Fprintln(w, note)
		}
		if !settings.Enabled {
			fmt.Fprintf(w, "%s no longer turns the Webapp on: it stops at the next gateway restart (%s).\n",
				filepath.Join(target, "config.yaml"), webappRestartCommand)
		}
	case "starting":
		fmt.Fprintln(w, "The gateway is still starting the Webapp. Run icebath webapp --status again in a moment.")
	case "off":
		fmt.Fprintln(w, webappOffMessage(ctx, target, "run "+webappRestartCommand))
	default:
		fmt.Fprintf(w, "Unavailable: %s\n", pyrepr.Str(status["reason"]))
		if webappRestartApplies(status["reason"]) {
			fmt.Fprintln(w, webappRetryAfterFix)
		}
	}
	return 0
}

// Whether fixing the unavailable reason takes a gateway restart, never for a missing or stale bundle
func webappRestartApplies(reason any) bool {
	return !strings.HasPrefix(pyrepr.Str(reason), WebappNotBuilt)
}

// Notice that the running gateway still serves a Webapp its config turned off, empty otherwise
func webappStillServed(target string) string {
	response, err := wl.control(target, "webapp-status", nil)
	if err != nil {
		return ""
	}
	status := map[string]any{}
	if pyrepr.Truthy(response["ok"]) {
		if result := webappResult(response); result != nil {
			status = result
		}
	}
	if status["state"] != "serving" {
		return ""
	}
	return fmt.Sprintf("%s no longer turns the Webapp on, but the running gateway still serves it at %s until it restarts. "+
		"Run %s to apply the config.", filepath.Join(target, "config.yaml"), pyrepr.Str(status["url"]), webappRestartCommand)
}

// What a gateway's sign out answer says it did
func webappSignedOutSummary(result map[string]any) string {
	if serving, present := result["serving"]; present && !pyrepr.Truthy(serving) {
		return "The gateway is not serving the Webapp, so no browser is connected to it. Replaced its stored " +
			"browser credential: every browser must sign in again once it serves the Webapp."
	}
	closedRaw, present := result["connections_closed"]
	if !present {
		return fmt.Sprintf("Replaced the Webapp's browser credential. Restart the gateway (%s) to sign "+
			"out browsers that signed in with a password or an account too.", webappRestartCommand)
	}
	closed := pyrepr.Str(closedRaw)
	plural := "s"
	if n, ok := closedRaw.(float64); ok && n == 1 {
		plural = ""
	} else if n, ok := closedRaw.(int); ok && n == 1 {
		plural = ""
	} else if n, ok := closedRaw.(int64); ok && n == 1 {
		plural = ""
	}
	connections := fmt.Sprintf("closed %s browser connection%s", closed, plural)
	if pyrepr.Truthy(result["sign_in_gate"]) {
		return fmt.Sprintf("Signed out every browser: %s. Every browser login behind sign-in has ended, "+
			"and the account tokens it held no longer work here: browsers must sign in again. Native apps "+
			"signed in to this gateway with their own sign-in stay signed in.", connections)
	}
	return fmt.Sprintf("Signed out of every Webapp tab: %s. Run icebath webapp to open it again.", connections)
}

// Signs out every Webapp browser, waiting for a starting gateway, and returns the exit code
func webappSignOut(ctx context.Context, w io.Writer, target string) (int, error) {
	for attempt := 0; attempt <= webappStartWaitAttempts; attempt++ {
		response, err := wl.control(target, "webapp-sign-out", nil)
		if err != nil {
			response = nil
		}
		if response != nil && pyrepr.Truthy(response["ok"]) {
			result := webappResult(response)
			if result == nil {
				result = map[string]any{}
			}
			fmt.Fprintln(w, webappSignedOutSummary(result))
			return 0, nil
		}
		state := wl.gatewayState(ctx, target)
		if response == nil && state == "absent" {
			if _, err := BrowserSessionToken(ctx, target, true); err != nil {
				var credErr *BrowserCredentialError
				if errors.As(err, &credErr) {
					fmt.Fprintf(w, "Nobody was signed out: %s\n", err.Error())
					return 1, nil
				}
				return 0, err
			}
			fmt.Fprintln(w, "No gateway is running for this profile. Replaced the stored Webapp credential: every browser "+
				"must sign in again when the gateway next serves the Webapp.")
			return 0, nil
		}
		if state != "starting" {
			reason := fmt.Sprintf("the gateway did not answer (state: %s)", state)
			if response != nil {
				reason = webappRefusal(response)
			}
			fmt.Fprintf(w, "Nobody was signed out: %s\n", reason)
			return 1, nil
		}
		if attempt < webappStartWaitAttempts {
			wl.sleep(webappStartWaitInterval)
		}
	}
	fmt.Fprintf(w, "The gateway is still starting, so nobody was signed out. Run icebath webapp --sign-out again once "+
		"it is up (%s if it never comes up).\n", webappRestartCommand)
	return 1, nil
}

// Explains why the gateway refused the launch and returns the exit code
func webappExplainRefusedLaunch(w io.Writer, target string, response map[string]any) int {
	status := map[string]any{}
	if answer, err := wl.control(target, "webapp-status", nil); err == nil {
		if result := webappResult(answer); result != nil {
			status = result
		}
	}
	if status["state"] == "off" {
		fmt.Fprintln(w, webappStartedBeforeOn)
	} else {
		fmt.Fprintln(w, webappRefusal(response))
		if status["state"] == "unavailable" && webappRestartApplies(status["reason"]) {
			fmt.Fprintln(w, webappRetryAfterFix)
		}
	}
	return 1
}

// Opens the browser hosted Desktop the gateway serves, building the bundle and starting the gateway when needed
func CmdWebapp(ctx context.Context, h string, ns map[string]any, explicitProfile string, w, errW io.Writer) (int, error) {
	if refusal := deletedProfileRefusal(h); refusal != "" {
		fmt.Fprintf(errW, "Error: %s\n", refusal)
		return 1, nil
	}
	target, err := webappGatewayHome(ctx, h)
	if err != nil {
		return 0, err
	}
	if dashArgBool(ns, "status") {
		return webappStatus(ctx, w, target), nil
	}
	if dashArgBool(ns, "sign_out") {
		return webappSignOut(ctx, w, target)
	}
	buildOnly := dashArgBool(ns, "build_only")
	if !buildOnly && !WebappSettingsOf(ctx, target).Enabled {
		message := webappStillServed(target)
		if message == "" {
			message = webappOffMessage(ctx, target, "run icebath webapp again")
		}
		fmt.Fprintln(w, message)
		return 1, nil
	}
	if err := wl.build(ctx, ns); err != nil {
		if WebappRendererBuild.ExceptionName(err) != "WebappBuildError" {
			return 0, err
		}
		fmt.Fprintf(w, "✗ %s\n", err.Error())
		if WebappRendererBuild.PackagedDist() == "" {
			fmt.Fprintln(w, "  Build it explicitly:")
			fmt.Fprintln(w, "    icebath webapp --build-only")
		}
		return 1, nil
	}
	if buildOnly {
		fmt.Fprintln(w, "Webapp bundle ready.")
		return 0, nil
	}
	response, err := func() (map[string]any, error) {
		if err := wl.ensure(ctx, target); err != nil {
			return nil, err
		}
		return wl.control(target, "webapp-launch", webappProfileChoice(h, target))
	}()
	if err != nil {
		var unreachable *gatewayUnreachable
		if errors.As(err, &unreachable) {
			fmt.Fprintln(w, err.Error())
			return 1, nil
		}
		return 0, err
	}
	if !pyrepr.Truthy(response["ok"]) {
		return webappExplainRefusedLaunch(w, target, response), nil
	}
	link := webappResult(response)
	linkURL := pyrepr.Str(link["url"])
	if note := webappPlainHTTPNote(linkURL); note != "" {
		fmt.Fprintln(w, note)
	}
	noOpen := dashArgBool(ns, "no_open")
	if pyrepr.Truthy(link["sign_in"]) {
		if !noOpen && wl.open(linkURL) {
			fmt.Fprintln(w, "Opened the Webapp in your browser. Sign in with your dashboard account.")
		} else {
			fmt.Fprintf(w, "Open the Webapp and sign in with your dashboard account: %s\n", linkURL)
		}
		return 0, nil
	}
	if pyrepr.Truthy(link["private"]) {
		if !noOpen && webappOpenPrivately(target, linkURL) {
			fmt.Fprintln(w, "Opened the Webapp in your browser.")
			fmt.Fprintf(w, "If no browser opened, run `%s` for a link.\n", webappRerunCommand(explicitProfile, "--no-open"))
			return 0, nil
		}
		fmt.Fprintf(w, "Open this link in your browser within %s seconds. It works once and signs the browser in as you, so keep it private:\n",
			pyrepr.Str(link["expires_in_seconds"]))
		fmt.Fprintf(w, "  %s\n", linkURL)
		return 0, nil
	}
	if !noOpen && wl.open(linkURL) {
		fmt.Fprintln(w, "Opened the Webapp in your browser.")
		return 0, nil
	}
	fmt.Fprintf(w, "Open the Webapp with this one-use link (valid for %s seconds):\n", pyrepr.Str(link["expires_in_seconds"]))
	fmt.Fprintf(w, "  %s\n", linkURL)
	return 0, nil
}
