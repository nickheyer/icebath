package gatewayrun

import (
	"context"
	"fmt"
	"net/url"
	"regexp"
	"runtime"
	"strconv"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/hostinfo"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

// Webapp listener status the webapp verbs answer with, url and reason nil when unset
type webappListenerStatus struct {
	State  string
	URL    *string
	Reason *string
}

// Status of a listener still starting, what webapp-status answers until it settles
func webappStarting() *webappListenerStatus { return &webappListenerStatus{State: "starting"} }

const webappStillStarting = "The Webapp is still starting; try again in a moment."

var webappProfileIDRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}\n?$`)

// JSON object of the status, the dict the verbs return
func (s *webappListenerStatus) object() *pyjson.Object {
	var u, reason any
	if s.URL != nil {
		u = *s.URL
	}
	if s.Reason != nil {
		reason = *s.Reason
	}
	return pyjson.ObjectOf("state", s.State, "url", u, "reason", reason)
}

// Status of the Webapp listener, starting when the listener has not set one
func (r *GatewayRunner) webappListenerStatus() *webappListenerStatus {
	if s := r.webappStatus.Load(); s != nil {
		return s
	}
	return webappStarting()
}

// sys.platform of this host as the launch link check names it
func pySysPlatform() string {
	if runtime.GOOS == "windows" {
		return "win32"
	}
	return runtime.GOOS
}

// Whether a browser the server opens can redeem a one use launch link on platform
func oneUseLinksWork(platform string) bool {
	return platform == "win32" || (platform == "linux" && !hostinfo.IsWSL())
}

// Why the Webapp bundle the mounted application serves is missing or stale, empty when it is served
func bundleMissing(web gateway.GatewayDashboardWeb) string {
	if web == nil {
		return ""
	}
	dist, ok := web.WebDist()
	if !ok {
		return ""
	}
	return gateway.BundleMissingReason(dist)
}

// Mounted dashboard application of the runner's listener, nil before it mounted
func (r *GatewayRunner) sessionAPIWeb() gateway.GatewayDashboardWeb {
	if r.sessionAPI == nil || r.sessionAPI.app == nil {
		return nil
	}
	return r.sessionAPI.app.Web()
}

func runtimeError(msg string) error { return &gateway.PyError{Kind: "RuntimeError", Msg: msg} }

// Registers webapp-status, webapp-launch and webapp-sign-out on the control server for same user peers
func registerControlVerbs(ctx context.Context, r *GatewayRunner, h string, server *gateway.GatewayControlServer, linksChecked *bool) {
	if server == nil {
		server = r.sessionControlServer
	}
	if server == nil {
		return
	}
	r.webappStatus.CompareAndSwap(nil, webappStarting())
	serving := func() (*webappListenerStatus, gateway.GatewayDashboardWeb, error) {
		status := r.webappListenerStatus()
		switch status.State {
		case "off":
			return nil, nil, runtimeError("The Webapp is off. Set webapp.enabled: true in config.yaml and restart the gateway.")
		case "starting":
			return nil, nil, runtimeError(webappStillStarting)
		case "serving":
		default:
			return nil, nil, runtimeError(fmt.Sprintf("The Webapp is unavailable: %s", pyStrOrNone(status.Reason)))
		}
		web := r.sessionAPIWeb()
		if web == nil {
			return nil, nil, runtimeError(webappStillStarting)
		}
		return status, web, nil
	}
	webappStatusVerb := func(_ map[string]any, _ string) (any, error) {
		status := *r.webappListenerStatus()
		if missing := bundleMissing(r.sessionAPIWeb()); status.State == "serving" && missing != "" {
			return pyjson.ObjectOf("state", "unavailable", "url", nil, "reason", missing), nil
		}
		return status.object(), nil
	}
	webappLaunch := func(params map[string]any, _ string) (any, error) {
		profileValue, hasProfile := params["profile"]
		profile, isStr := profileValue.(string)
		if hasProfile && profileValue != nil && !(isStr && webappProfileIDRe.MatchString(profile)) {
			return nil, &gateway.PyError{Kind: "ValueError", Msg: "profile must be a profile name"}
		}
		query := ""
		if profile != "" {
			query = "?profile=" + profile
		}
		status, web, err := serving()
		if err != nil {
			return nil, err
		}
		if missing := bundleMissing(web); missing != "" {
			return nil, runtimeError(fmt.Sprintf("The Webapp is unavailable: %s", missing))
		}
		statusURL := ""
		if status.URL != nil {
			statusURL = *status.URL
		}
		if !web.PrivateLaunch() {
			address := web.ListenerPublicURL()
			if address == "" {
				address = gateway.RegisteredLANOrigin(h, urlPort(statusURL))
			}
			if address == "" {
				address = statusURL
			}
			return pyjson.ObjectOf("url", address+"/"+query, "private", false, "expires_in_seconds", nil, "sign_in", true), nil
		}
		checked := oneUseLinksWork(pySysPlatform())
		if linksChecked != nil {
			checked = *linksChecked
		}
		var ticket string
		if checked {
			ticket, err = web.MintLaunchTicket()
		} else {
			ticket, err = web.MintPrintedLaunchTicket()
		}
		if err != nil {
			return nil, err
		}
		fragment := "ticket=" + url.QueryEscape(ticket)
		if query != "" {
			fragment += "&query=" + url.QueryEscape(query)
		}
		return pyjson.ObjectOf("url", statusURL+"/webapp/launch#"+fragment, "private", !checked,
			"expires_in_seconds", int64(web.LaunchTicketTTLSeconds())), nil
	}
	webappSignOut := func(_ map[string]any, _ string) (any, error) {
		state := r.webappListenerStatus().State
		if state == "starting" {
			return nil, runtimeError(webappStillStarting)
		}
		if state != "serving" {
			if _, err := gateway.BrowserSessionToken(ctx, h, true); err != nil {
				return nil, err
			}
			return pyjson.ObjectOf("signed_out", true, "serving", false, "sign_in_gate", false, "connections_closed", int64(0)), nil
		}
		_, web, err := serving()
		if err != nil {
			return nil, err
		}
		token, err := gateway.BrowserSessionToken(ctx, h, true)
		if err != nil {
			return nil, err
		}
		ended := web.RotateBrowserCredential(token)
		return pyjson.ObjectOf("signed_out", true, "serving", true, "sign_in_gate", web.AuthRequired(),
			"connections_closed", int64(ended["sockets"])), nil
	}
	server.AddPrivateHandlers(map[string]gateway.ControlPrivateHandler{
		"webapp-status": webappStatusVerb, "webapp-launch": webappLaunch, "webapp-sign-out": webappSignOut})
}

// Port of a URL, minus one standing for none the way urlsplit gives None
func urlPort(raw string) int {
	parts, err := url.Parse(raw)
	if err != nil || parts.Port() == "" {
		return -1
	}
	n, err := strconv.Atoi(parts.Port())
	if err != nil {
		return -1
	}
	return n
}

// Python str of an optional string, None when nil
func pyStrOrNone(s *string) string {
	if s == nil {
		return "None"
	}
	return *s
}
