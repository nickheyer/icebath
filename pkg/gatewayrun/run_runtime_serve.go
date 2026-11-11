package gatewayrun

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"strings"
	"syscall"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
)

// Stops profile local services and turns before ownership is released, false when work outlives timeout
func retireProfileAuthority(ctx context.Context, a *gateway.SessionAuthority, timeout *float64) (bool, error) {
	wait := gateway.TurnSettleSeconds
	if timeout != nil {
		wait = *timeout
	}
	a.Retiring = true
	if r, ok := a.Runner.(*GatewayRunner); ok && r.sessionTicketStore != nil {
		r.publishServedSet()
	}
	gateway.StopAuthorityWork(a)
	gateway.StopOneshots(a)
	var rooms chan bool
	if service, ok := a.HostedRoomService.(interface{ Stop(timeout float64) bool }); ok && service != nil {
		rooms = make(chan bool, 1)
		go func() { rooms <- service.Stop(wait) }()
	}
	workersStopped := true
	if err := gateway.JoinAuthorityWork(ctx, a, wait); err != nil {
		var timeoutErr *gateway.PyError
		if !errors.As(err, &timeoutErr) || timeoutErr.Kind != "TimeoutError" {
			return false, err
		}
		runtimeLog().Error(fmt.Sprintf("Profile %s workers did not stop; ownership retained", a.ProfileID))
		workersStopped = false
	}
	roomsStopped := rooms == nil || <-rooms
	if !roomsStopped {
		runtimeLog().Error(fmt.Sprintf("Profile %s hosted rooms did not stop; ownership retained", a.ProfileID))
	}
	if !(workersStopped && roomsStopped) {
		return false, nil
	}
	rest := gateway.AuthorityTasks(a)
	gateway.CancelBotReceiptTasks(a)
	for _, done := range rest {
		select {
		case <-done:
		case <-ctx.Done():
			return false, ctx.Err()
		}
	}
	gateway.UnbindCronOwner(a)
	a.Retired = true
	gateway.ReleaseRetiredWaiters(a)
	return true, nil
}

// Builds, recovers and publishes one reserved profile's runtime the way boot does per secondary
func serveProfileRuntime(ctx context.Context, r *GatewayRunner, name, profileHome string) (*gateway.SessionAuthority, error) {
	resolved, err := pypath.Resolve(profileHome)
	if err != nil {
		return nil, err
	}
	registry := r.sessionAuthorities
	existing := registry.ForHome(resolved)
	if existing != nil && existing.Retiring {
		zero := 0.0
		retired, err := retireProfileAuthority(ctx, existing, &zero)
		if err != nil {
			return nil, err
		}
		if !retired {
			return nil, &ProfileOwnershipRetained{Msg: fmt.Sprintf("an earlier serve of %s is still stopping", resolved)}
		}
		if _, err := registry.Remove(resolved); err != nil {
			return nil, err
		}
	} else if existing != nil {
		return existing, nil
	}
	authority, err := r.buildProfileAuthority(ctx, name, resolved, false)
	if err != nil {
		return nil, err
	}
	if err := func() error {
		scoped := gateway.OwnerScope(ctx, authority, false)
		if err := dropRetiredLocalAdapter(scoped, authority); err != nil {
			return err
		}
		if err := gateway.RecoverBotMailbox(scoped, authority); err != nil {
			return err
		}
		if err := gateway.RecoverLocalSessions(scoped, authority, true); err != nil {
			return err
		}
		if _, err := gateway.RecoverWebhookFinalizations(scoped, authority); err != nil {
			return err
		}
		if err := recoverPublishedAPITurns(scoped, r, authority); err != nil {
			return err
		}
		if r.sessionControlServer != nil {
			return gateway.EnsureAuthorityHostedService(ctx, r, authority)
		}
		return nil
	}(); err != nil {
		retired, retireErr := retireProfileAuthority(ctx, authority, nil)
		if retireErr != nil {
			return nil, errors.Join(err, retireErr)
		}
		if retired {
			if _, removeErr := registry.Remove(resolved); removeErr != nil {
				return nil, errors.Join(err, removeErr)
			}
		}
		return nil, err
	}
	r.publishServedSet()
	if err := gateway.StartReadyHostedServices(r); err != nil {
		return nil, err
	}
	return authority, nil
}

// Drops a local adapter still bound to the profile's fully retired previous authority
func dropRetiredLocalAdapter(ctx context.Context, authority *gateway.SessionAuthority) error {
	adapters, err := gateway.LocalAdapterMap(ctx, authority)
	if err != nil {
		return err
	}
	adapter, _ := adapters.Get(gateway.PlatformLocal)
	local, ok := adapter.(*gateway.LocalSessionAdapter)
	if !ok || local == nil {
		return nil
	}
	if previous := local.Authority; previous != nil && previous != authority && previous.Retired {
		adapters.Delete(gateway.PlatformLocal)
	}
	return nil
}

// Recovers the profile's queued API turns when the launch API adapter is already published
func recoverPublishedAPITurns(ctx context.Context, r *GatewayRunner, authority *gateway.SessionAuthority) error {
	if r.adapters == nil {
		return nil
	}
	if api, ok := r.adapters.Get(gateway.PlatformAPIServer); ok && api != nil {
		return gateway.RecoverAuthorityAPITurns(ctx, authority)
	}
	return nil
}

// Retires one profile's authority and shrinks the published set, false when a turn outlived its Stop
func unserveProfileRuntime(ctx context.Context, r *GatewayRunner, profileHome string) (bool, error) {
	resolved, err := pypath.Resolve(profileHome)
	if err != nil {
		return false, err
	}
	registry := r.sessionAuthorities
	authority := registry.ForHome(resolved)
	if authority == nil {
		return true, nil
	}
	retired, err := retireProfileAuthority(ctx, authority, nil)
	if err != nil || !retired {
		return false, err
	}
	if _, err := registry.Remove(resolved); err != nil {
		return false, err
	}
	r.publishServedSet()
	return true, nil
}

// Startup Bot Chat recovery of one served profile, a failing secondary parked instead of aborting the start
func recoverProfileBotMailbox(ctx context.Context, r *GatewayRunner, authority *gateway.SessionAuthority) error {
	err := gateway.RecoverBotMailbox(gateway.OwnerScope(ctx, authority, false), authority)
	if err == nil {
		return nil
	}
	registry := r.sessionAuthorities
	if registry == nil {
		return err
	}
	name, served := registry.ProfileName(authority)
	if !served {
		return err
	}
	runtimeLog().Error(fmt.Sprintf("[MULTIPLEX] Profile '%s' not served: startup recovery failed: %s", name, err), "exc_info", err)
	resolved, resolveErr := pypath.Resolve(authority.ProfileID)
	if resolveErr != nil {
		return resolveErr
	}
	retired, retireErr := retireProfileAuthority(ctx, authority, nil)
	if retireErr != nil {
		return retireErr
	}
	if !retired {
		r.withdrawProfileHome(resolved)
		r.parkProfile(ctx, name, fmt.Sprintf("startup recovery failed; ownership retained (a control-socket rescan, "+
			"`icebath -p %s gateway run`, serves it once its writer stops): %s", name, err))
		return nil
	}
	if _, removeErr := registry.Remove(resolved); removeErr != nil {
		return removeErr
	}
	r.publishServedSet()
	if releaseErr := r.releaseProfileHome(ctx, resolved); releaseErr != nil {
		return releaseErr
	}
	r.parkProfile(ctx, name, fmt.Sprintf("startup recovery failed (a control-socket rescan, "+
		"`icebath -p %s gateway run`, serves it again): %s", name, err))
	return nil
}

// Why the Webapp listener could not start, the gate's own message, the credential file or the bind failure
func webappListenerFailure(err error, host string, port int) string {
	var exit *gateway.DashboardExit
	var credential *gateway.BrowserCredentialError
	var pathErr *fs.PathError
	switch {
	case errors.As(err, &exit):
		return strings.TrimSpace(exit.Error())
	case errors.As(err, &credential):
		return credential.Error()
	case errors.As(err, &pathErr):
		return fmt.Sprintf("could not use %s (%s)", pathErr.Path, osStrerror(pathErr.Err))
	}
	cause := err.Error()
	if pypath.IsOSError(err) {
		cause = osStrerror(err)
	}
	return fmt.Sprintf("could not listen on %s:%d (%s)", host, port, cause)
}

// strerror of an OSError, the error text when it carries no errno
func osStrerror(err error) string {
	var errno syscall.Errno
	if errors.As(err, &errno) {
		return pypath.Strerror(errno)
	}
	return err.Error()
}

// Listener start of the runtime API, the real function swapped only where a hermes case installs a fake
var startGatewayAPIFn = startGatewayAPI

// Starts the session API listener, the Webapp's when configured, falling back to the private one
func startGatewayRuntimeAPI(ctx context.Context, r *GatewayRunner) error {
	h := home.Dir(ctx)
	registerControlVerbs(ctx, r, h, nil, nil)
	host, port, webapp, unusable := gateway.WebappListenerPlan(ctx, h)
	switch {
	case unusable != "":
		r.webappStatus.Store(&webappListenerStatus{State: "unavailable", Reason: &unusable})
	case webapp == nil:
		r.webappStatus.Store(&webappListenerStatus{State: "off"})
	default:
		r.webappStatus.Store(webappStarting())
	}
	var handle *gatewayAPIHandle
	var err error
	if webapp != nil {
		handle, err = startGatewayAPIFn(ctx, r, host, port, webapp)
	} else {
		handle, err = startGatewayAPIFn(ctx, r, "127.0.0.1", 0, nil)
	}
	if err != nil {
		if webapp == nil || isCancellation(err) {
			return err
		}
		reason := webappListenerFailure(err, host, port)
		runtimeLog().Error(fmt.Sprintf("Webapp unavailable: %s", reason))
		r.webappStatus.Store(&webappListenerStatus{State: "unavailable", Reason: &reason})
		if _, writeErr := gateway.BrowserSessionToken(ctx, h, true); writeErr != nil {
			var credential *gateway.BrowserCredentialError
			if !errors.As(writeErr, &credential) && !pypath.IsOSError(writeErr) {
				return writeErr
			}
			runtimeLog().Warn(fmt.Sprintf("Could not replace the Webapp's browser credential: %s", writeErr))
		}
		if handle, err = startGatewayAPIFn(ctx, r, "127.0.0.1", 0, nil); err != nil {
			return err
		}
	} else if webapp != nil {
		origin := gateway.WebappDialOrigin(host, port)
		r.webappStatus.Store(&webappListenerStatus{State: "serving", URL: &origin})
	}
	r.sessionAPI = handle
	r.SetRuntimeDescriptorField("api_origin", handle.apiOrigin)
	for _, authority := range gateway.AllAuthorities(r) {
		if err := recoverProfileBotMailbox(ctx, r, authority); err != nil {
			return err
		}
	}
	return nil
}
