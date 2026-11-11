package dashboard

import (
	"context"
	"fmt"
)

// What one launch surface of the web server stack may do
type SurfacePolicy struct {
	ServesSPA             bool
	HTMLToken             bool
	HostTerminal          bool
	LaunchLink            bool
	GatewayTicket         bool
	OwnOriginCookieWrites bool
	WebAccess             bool
	BrowserFiles          bool
	DesktopPlugins        bool
	UIPreferences         bool
}

// Policy of each surface, serve, dashboard, the gateway listener and the webapp
var SurfacePolicies = map[string]SurfacePolicy{
	"serve":     {HTMLToken: true},
	"dashboard": {ServesSPA: true, HTMLToken: true},
	"gateway":   {ServesSPA: true, HTMLToken: true, WebAccess: true},
	"webapp": {ServesSPA: true, HostTerminal: true, LaunchLink: true, GatewayTicket: true, OwnOriginCookieWrites: true,
		WebAccess: true, BrowserFiles: true, DesktopPlugins: true, UIPreferences: true},
}

// Field of a policy by its Python attribute name
func (p SurfacePolicy) field(name string) bool {
	switch name {
	case "serves_spa":
		return p.ServesSPA
	case "html_token":
		return p.HTMLToken
	case "host_terminal":
		return p.HostTerminal
	case "launch_link":
		return p.LaunchLink
	case "gateway_ticket":
		return p.GatewayTicket
	case "own_origin_cookie_writes":
		return p.OwnOriginCookieWrites
	case "web_access":
		return p.WebAccess
	case "browser_files":
		return p.BrowserFiles
	case "desktop_plugins":
		return p.DesktopPlugins
	case "ui_preferences":
		return p.UIPreferences
	}
	panic(fmt.Sprintf("'SurfacePolicy' object has no attribute '%s'", name))
}

// Surface the app state serves, its recorded one, else gateway when a runner is attached, else dashboard
func Surface(state *State) string {
	if recorded := state.String("ui_surface"); recorded != "" {
		return recorded
	}
	if state.Get("gateway_runner") != nil {
		return "gateway"
	}
	return "dashboard"
}

// Policy of the surface the app state serves
func PolicyOf(state *State) SurfacePolicy { return SurfacePolicies[Surface(state)] }

// Route dependency answering 404 where the serving surface leaves the policy field off
func RouteOn(field string) *Dependency {
	return &Dependency{Name: "surface_serves", Call: func(_ context.Context, c *Call) error {
		if c.Request == nil {
			return &TypeError{Msg: "route_on.<locals>.surface_serves() missing 1 required positional argument: 'request'"}
		}
		if !PolicyOf(c.Request.App().State).field(field) {
			return NewHTTPException(404, "Not Found")
		}
		return nil
	}}
}

// Whether the webapp runs on an unauthenticated bind where the launch link is the only way in
func PrivateLaunch(state *State) bool {
	return PolicyOf(state).LaunchLink && !state.Bool("auth_required")
}

// Python TypeError raised by the web layer
type TypeError struct{ Msg string }

func (e *TypeError) Error() string      { return e.Msg }
func (e *TypeError) PyTypeName() string { return "TypeError" }
