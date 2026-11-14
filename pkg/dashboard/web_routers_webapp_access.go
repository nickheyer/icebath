package dashboard

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

var (
	webappAccessChangeMu   sync.Mutex
	webappAccessChangePoll = 50 * time.Millisecond
)

var webappAccessStartModel = &Model{ClassName: "StartBody", Fields: []ModelField{
	Field("mode", Str),
	FieldDefault("public_url", Optional(Str), nil),
}}

// Refuses every caller but native Desktop on this machine, admitted with a native grant
func ownerOnThisMachine() *Dependency {
	return &Dependency{Name: "owner_on_this_machine", Call: func(_ context.Context, c *Call) error {
		if !pyTruthy(c.Conn().State().Get("native_http_principal")) {
			return NewHTTPException(403, "Web Access can only be changed from the Icebath app on this computer.")
		}
		return nil
	}}
}

// Profile name of the gateway serving the profile, resolved inside that profile scope
func webappGatewayProfile(ctx context.Context, profile string) (string, error) {
	sctx, _, release, err := profileScope(ctx, profile)
	if err != nil {
		return "", err
	}
	defer release()
	gh, err := gateway.WebappGatewayHome(sctx)
	if err != nil {
		return "", err
	}
	if name, ok := gateway.ProfileNameForHome(gh); ok && name != "" {
		return name, nil
	}
	return profile, nil
}

// 400 with the reason for a Web Access refusal, any other error unchanged
func webappAccessHTTPError(err error) error {
	var ae *gateway.WebappAccessError
	if errors.As(err, &ae) {
		return NewHTTPException(400, ae.Error())
	}
	return err
}

// Result of fn run in the gateway profile scope, the config lock held only for a config write
func webappAccessScoped[T any](ctx context.Context, profile string, configWrite bool, fn func(ctx context.Context, gateway string) (T, error)) (T, error) {
	var zero T
	name, err := webappGatewayProfile(ctx, profile)
	if err != nil {
		return zero, webappAccessHTTPError(err)
	}
	var sctx context.Context
	var release func()
	if configWrite {
		sctx, release, err = configWriteScope(ctx, name)
	} else {
		sctx, _, release, err = profileScope(ctx, name)
	}
	if err != nil {
		return zero, webappAccessHTTPError(err)
	}
	defer release()
	out, err := fn(sctx, name)
	return out, webappAccessHTTPError(err)
}

// Result of fn in the home and secret scope of the gateway profile without the skills lock
func webappAccessInGatewayHome[T any](ctx context.Context, gatewayProfile string, fn func(ctx context.Context) (T, error)) (T, error) {
	out, err := configScopedToThread(ctx, gatewayProfile, fn)
	return out, webappAccessHTTPError(err)
}

// Takes the one change lock by polling, a request cancelled while waiting never taking it
func webappAccessOneChange(ctx context.Context) (func(), error) {
	for !webappAccessChangeMu.TryLock() {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(webappAccessChangePoll):
		}
	}
	return webappAccessChangeMu.Unlock, nil
}

// Status after asking the gateway profile gateway to restart onto the saved config
func webappAccessRestart(ctx context.Context, gatewayProfile, what string) (*pyjson.Object, error) {
	restart, err := restartGatewayAfter(ctx, gatewayProfile, what, "Web Access")
	if err != nil {
		return nil, err
	}
	status, err := webappAccessInGatewayHome(ctx, gatewayProfile, gateway.WebappAccessStatus)
	if err != nil {
		return nil, err
	}
	for _, k := range restart.Keys {
		status.Set(k, restart.Vals[k])
	}
	return status, nil
}

// Router of the Desktop Web Access pane, every route behind the owner on this machine check
func webappAccessRouter() *APIRouter {
	r := NewAPIRouter("", ownerOnThisMachine())
	profileParam := QueryParam("profile", Optional(Str), nil)
	r.Get("/api/webapp-access/status", &Endpoint{
		Name:   "webapp_access_status",
		Params: []Param{profileParam},
		Handle: func(ctx context.Context, c *Call) (any, error) {
			gw, err := webappAccessScoped(ctx, c.Str("profile"), false, func(_ context.Context, gw string) (string, error) { return gw, nil })
			if err != nil {
				return nil, err
			}
			return webappAccessInGatewayHome(ctx, gw, func(sctx context.Context) (*pyjson.Object, error) {
				status, err := gateway.WebappAccessStatus(sctx)
				if err != nil {
					return nil, err
				}
				failure, err := gatewayRestartFailure(sctx, gw)
				if err != nil {
					return nil, err
				}
				if failure == nil {
					status.Set("restart_failure", nil)
				} else {
					status.Set("restart_failure", failure)
				}
				return status, nil
			})
		},
	})
	r.Post("/api/webapp-access/start", &Endpoint{
		Name:   "start_webapp_access",
		Params: []Param{profileParam},
		Body:   []BodyParam{Body("body", webappAccessStartModel)},
		Handle: func(ctx context.Context, c *Call) (any, error) {
			profile, err := destructiveProfile(ctx, c.Str("profile"), "POST /api/webapp-access/start")
			if err != nil {
				return nil, err
			}
			body := c.Object("body")
			mode, _ := body.GetOr("mode", "").(string)
			publicURL, _ := body.GetOr("public_url", nil).(string)
			done, err := webappAccessOneChange(ctx)
			if err != nil {
				return nil, err
			}
			defer done()
			type plan struct {
				gateway, host, publicURL, callback string
				port                               int
			}
			p, err := webappAccessScoped(ctx, profile, true, func(sctx context.Context, gw string) (plan, error) {
				host, port, pinned, callback, err := gateway.WebappAccessPlan(sctx, mode, publicURL)
				return plan{gw, host, pinned, callback, port}, err
			})
			if err != nil {
				return nil, err
			}
			if _, err := webappAccessInGatewayHome(ctx, p.gateway, func(sctx context.Context) (any, error) {
				return nil, gateway.WebappAccessBuildBundle(sctx)
			}); err != nil {
				return nil, err
			}
			if _, err := webappAccessInGatewayHome(ctx, p.gateway, func(sctx context.Context) (any, error) {
				return nil, gateway.WebappAccessRegister(sctx, p.callback)
			}); err != nil {
				return nil, err
			}
			if _, err := webappAccessScoped(ctx, profile, true, func(sctx context.Context, _ string) (any, error) {
				return nil, gateway.WebappAccessServe(sctx, p.host, p.publicURL, p.port)
			}); err != nil {
				return nil, err
			}
			return webappAccessRestart(ctx, p.gateway, "turning on Web Access")
		},
	})
	r.Post("/api/webapp-access/stop", &Endpoint{
		Name:   "stop_webapp_access",
		Params: []Param{profileParam},
		Handle: func(ctx context.Context, c *Call) (any, error) {
			profile, err := destructiveProfile(ctx, c.Str("profile"), "POST /api/webapp-access/stop")
			if err != nil {
				return nil, err
			}
			done, err := webappAccessOneChange(ctx)
			if err != nil {
				return nil, err
			}
			defer done()
			gw, err := webappAccessScoped(ctx, profile, true, func(sctx context.Context, gw string) (string, error) {
				return gw, gateway.WebappAccessTurnOff(sctx)
			})
			if err != nil {
				return nil, err
			}
			return webappAccessRestart(ctx, gw, "turning off Web Access")
		},
	})
	r.Post("/api/webapp-access/restart", &Endpoint{
		Name:   "restart_webapp_access",
		Params: []Param{profileParam},
		Handle: func(ctx context.Context, c *Call) (any, error) {
			profile, err := destructiveProfile(ctx, c.Str("profile"), "POST /api/webapp-access/restart")
			if err != nil {
				return nil, err
			}
			done, err := webappAccessOneChange(ctx)
			if err != nil {
				return nil, err
			}
			defer done()
			gw, err := webappAccessScoped(ctx, profile, false, func(_ context.Context, gw string) (string, error) { return gw, nil })
			if err != nil {
				return nil, err
			}
			return webappAccessRestart(ctx, gw, "applying Web Access")
		},
	})
	return r
}
