// Package gatewaycreate has the gateway that serves this terminal create a profile, one request answered by one receipt
package gatewaycreate

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/profiles"
	"github.com/nickheyer/icebath/pkg/profiles/manage"
	"github.com/nickheyer/icebath/pkg/tools"
	"github.com/nickheyer/icebath/pkg/tuigateway"
	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
	pathutil "github.com/nickheyer/icebath/pkg/utils/path"
	urlutil "github.com/nickheyer/icebath/pkg/utils/url"
)

// Environment variable naming a gateway to attach to instead of this home's own
const gatewayURLEnv = "ICEBATH_TUI_GATEWAY_URL"

// Answer budget of a create, skill seeding and the multiplexer's rescan answer both taking long
const createRPCTimeout = 300 * time.Second

// How long discovery of this home's gateway may take
const discoveryTimeout = 2 * time.Second

// Code and reason of a gateway refusal, the pair the refusals answered before any handler ran are matched by
type refusalKey struct {
	code   int64
	reason string
}

// Error codes answered before any handler ran, a gateway that does not know a sent field and one retiring
var olderOrRetiringCodes = map[int64]bool{4000: true, 5035: true}

// Refusal of the request's fields by the session authority, which checks them before it routes to any handler
var fieldsRefused = refusalKey{4001, "invalid_params"}

// Routing refusals, matched by code and reason since a handler failure after the create can carry the same text
var profileRefused = map[refusalKey]bool{{4001, "profile_mismatch"}: true, {4064, "profile_unavailable"}: true}

// Refusal of a draining owner, answered before routing
var draining = refusalKey{4001, "runtime_draining"}

// GatewayCreateError says the named gateway cannot take the create, or took it and its outcome is unknown
type GatewayCreateError struct{ Msg string }

func (e *GatewayCreateError) Error() string { return e.Msg }

// Code and reason of a received refusal, the code zero when it carried none and the reason empty unless it is text
func keyOf(e *gateway.GatewayRPCError) refusalKey {
	var key refusalKey
	if e.Code != nil {
		key.code = *e.Code
	}
	key.reason, _ = e.Data["reason"].(string)
	return key
}

// Params of the create request, the wrapper always left to this terminal and the terminal's profile named for a gateway reached by URL
func wire(req manage.ProfileCreateRequest, profile string) map[string]any {
	params := map[string]any{"name": req.Name, "description": nil, "clone_from": nil, "clone_all": req.CloneAll,
		"clone_config": req.CloneConfig, "clone_channels": req.CloneChannels, "no_skills": req.NoSkills, "no_alias": true,
		"sync_imports": req.SyncImports, "mirror_credentials": !req.NoCredentials, "share_auth": !req.IsolatedAuth, "report": true}
	if req.Description != nil {
		params["description"] = *req.Description
	}
	if req.CloneFrom != nil {
		params["clone_from"] = *req.CloneFrom
	}
	if profile != "" {
		params["profile"] = profile
	}
	return params
}

// How messages name the gateway, never with its token, password or path, an error for a URL that is not a ws or wss URL with a host
func where(remote string) (string, error) {
	if remote == "" {
		return "the running gateway", nil
	}
	parts, err := urlutil.Split(remote)
	dialable := err == nil && (parts.Scheme == "ws" || parts.Scheme == "wss") && parts.Hostname() != ""
	if dialable {
		port, named, portErr := parts.Port()
		dialable = portErr == nil && (!named || port > 0)
	}
	if !dialable {
		return "", &GatewayCreateError{gatewayURLEnv + " is not a ws:// or wss:// URL with a host; nothing was created"}
	}
	if shown := gateway.SafeProxyURLForLog(remote); strings.HasPrefix(shown, "ws://") || strings.HasPrefix(shown, "wss://") {
		return "the gateway at " + shown, nil
	}
	return "the " + gatewayURLEnv + " gateway", nil
}

// Name of the profile this terminal runs in as a gateway resolves it, empty for a home outside the profiles tree
func terminalProfile(ctx context.Context) string {
	served, err := gateway.GatewayProfileHome(home.Dir(ctx))
	if err != nil {
		return ""
	}
	name, _ := gateway.ProfileNameForHome(served)
	return name
}

// Ready gateway serving this home, nil when none is, discovery never starting one
func liveOwner(ctx context.Context) *gateway.GatewayEndpoint {
	resolved, err := pathutil.Resolve(home.Dir(ctx))
	if err != nil {
		resolved = home.Dir(ctx)
	}
	found, err := gateway.DiscoverGatewayEndpointChecked(ctx, resolved, discoveryTimeout)
	if err != nil {
		pkgLog().Debug("profile create: gateway discovery failed", "error", err)
		return nil
	}
	if found.State != "ready" {
		return nil
	}
	return found.Endpoint
}

// Directory the named profile has on this machine, empty for a name that is none
func localProfileDir(name string) string {
	dir, err := profiles.GetProfileDir(name)
	if err != nil {
		return ""
	}
	return dir
}

// Reports whether a directory is a profile, one a deleted profile left behind carrying no identity file
func isProfile(local string) bool {
	if local == "" {
		return false
	}
	st, err := os.Stat(local)
	return err == nil && st.IsDir() && home.ProfileHasIdentity(local)
}

// Reports whether the create made the profile the name selects on this machine, none before the request and one after it at the reported path
func madeHere(ctx context.Context, made *tuigateway.ProfileCreateReceipt, local string, wasProfile bool) bool {
	return !wasProfile && isProfile(local) && profiles.ProfileMatchesHome(ctx, filepath.Base(local), made.Path)
}

// Instance the connected gateway reports, empty when it reports none
func describedInstance(ctx context.Context, client *gateway.GatewayClient) string {
	described, err := client.RPC(ctx, "runtime.describe", nil)
	if err != nil {
		pkgLog().Debug("profile create: runtime.describe failed", "error", err)
		return ""
	}
	instance, _ := described["instance_id"].(string)
	return instance
}

// Type names of failures that carry nothing but a text
var untypedFailures = map[string]bool{"errorString": true, "wrapError": true, "wrapErrors": true, "joinError": true}

// Short type name of a failure, the only part shown since its text can quote the gateway URL
func kindName(err error) string {
	name := strings.TrimLeft(fmt.Sprintf("%T", err), "*")
	if name = name[strings.LastIndex(name, ".")+1:]; untypedFailures[name] {
		return "connection error"
	}
	return name
}

// Outcome of one create request over a gateway connection
type exchange struct {
	answer any
	// Set once the connection stood, from where on a failure stops proving that nothing was sent
	attached bool
	// Whether the gateway reached by URL reported the instance this home's discovery names
	sameOwner bool
}

// Dials the gateway, proves its identity when one is expected and sends the create
func send(ctx context.Context, endpoint *gateway.GatewayEndpoint, ownerHere string, params map[string]any) (exchange, error) {
	var out exchange
	client, err := gateway.ConnectGatewayWith(ctx, gateway.ConnectGatewayOptions{Endpoint: endpoint})
	if err != nil {
		return out, err
	}
	defer client.Close()
	out.attached = true
	if ownerHere != "" {
		out.sameOwner = describedInstance(ctx, client) == ownerHere
	}
	out.answer, err = client.RPCTimeout(ctx, tuigateway.ProfilesCreate.Name(), params, createRPCTimeout)
	return out, err
}

// Receipt of the gateway's create, nil to create here, a refused create returned as the gateway's own refusal and anything short of a confirmed answer as a GatewayCreateError
func CreateThroughGateway(ctx context.Context, req manage.ProfileCreateRequest) (*tuigateway.ProfileCreateReceipt, error) {
	remote := strings.TrimSpace(os.Getenv(gatewayURLEnv))
	named, err := where(remote)
	if err != nil {
		return nil, err
	}
	profile := ""
	if remote != "" {
		if profile = terminalProfile(ctx); profile == "" {
			return nil, &GatewayCreateError{fmt.Sprintf("this terminal's ICEBATH_HOME is not a named profile, so %s cannot "+
				"create from it; nothing was created", named)}
		}
	}
	var endpoint *gateway.GatewayEndpoint
	if remote == "" || !req.NoAlias {
		endpoint = liveOwner(ctx)
	}
	if remote == "" && endpoint == nil {
		return nil, nil
	}
	machine := ""
	if remote != "" {
		machine = " on the gateway's machine"
	}
	unknown := fmt.Sprintf("%s may have created '%s'; check `icebath profile list`%s before retrying", named, req.Name, machine)
	local := localProfileDir(req.Name)
	wasProfile := isProfile(local)
	ours, ownerHere, dial := remote == "", "", endpoint
	if remote != "" {
		dial = nil
		if endpoint != nil {
			ownerHere = endpoint.InstanceID
		}
	}
	runCtx, stop := signal.NotifyContext(ctx, os.Interrupt)
	sent, err := send(runCtx, dial, ownerHere, wire(req, profile))
	interrupted := runCtx.Err() != nil && ctx.Err() == nil
	stop()
	if ownerHere != "" {
		ours = sent.sameOwner
	}
	var refused *gateway.GatewayRPCError
	switch {
	case err == nil:
	case interrupted && !sent.attached:
		return nil, tools.ErrKeyboardInterrupt
	case interrupted:
		return nil, &GatewayCreateError{"interrupted; " + unknown}
	case errors.As(err, &refused):
		return nil, refusedCreate(refused, remote, named, profile, unknown)
	case !sent.attached || err.Error() == gateway.GatewayNotSent:
		if remote != "" {
			return nil, &GatewayCreateError{fmt.Sprintf("could not reach %s (%s)", named, kindName(err))}
		}
		pkgLog().Info(fmt.Sprintf("profile create: the running gateway is unreachable (%s); creating locally", manage.RedactCreateText(err.Error())))
		return nil, nil
	default:
		return nil, &GatewayCreateError{fmt.Sprintf("lost %s during the create (%s); %s", named, kindName(err), unknown)}
	}
	var wireReceipt any
	if answer, isObject := jsonutil.Plain(sent.answer).(map[string]any); isObject {
		wireReceipt = answer["receipt"]
	}
	made := tuigateway.ProfileCreateReceiptFromWire(wireReceipt)
	if made == nil {
		return nil, &GatewayCreateError{fmt.Sprintf("%s answered without the details this version needs; %s", named, unknown)}
	}
	for i, warning := range made.Warnings {
		made.Warnings[i] = manage.RedactCreateText(warning)
	}
	if !req.NoAlias && ours && madeHere(ctx, made, local, wasProfile) {
		withLocalAlias(made, req.Name)
	}
	return made, nil
}

// Error of a create the gateway refused, nil to create here when a gateway of this home predates the request or is retiring
func refusedCreate(refused *gateway.GatewayRPCError, remote, named, profile, unknown string) error {
	key := keyOf(refused)
	unknownMethod := key.code == -32601 || (refused.Code == nil && refused.Msg == "unknown_method")
	if olderOrRetiringCodes[key.code] || key == fieldsRefused || unknownMethod || key == draining {
		if remote == "" {
			return nil
		}
		why := "predates this version; update it, or unset " + gatewayURLEnv + " to create locally"
		if key.code == 5035 || key == draining {
			why = "is restarting; retry"
		}
		return &GatewayCreateError{fmt.Sprintf("%s cannot create profiles for this terminal (it %s)", named, why)}
	}
	if refusal := manage.RefusalFrom(refused.Data); refusal != nil {
		return refusal
	}
	if profileRefused[key] {
		if profile == "" {
			profile = "its profile"
		}
		return &GatewayCreateError{fmt.Sprintf("%s cannot serve this terminal's profile (%s); nothing was created", named, profile)}
	}
	reason := manage.MessageFrom(refused.Data)
	if reason == "" {
		reason = manage.RedactCreateText(refused.Msg)
	}
	return &GatewayCreateError{fmt.Sprintf("%s did not finish the create (%s); %s", named, reason, unknown)}
}

// Writes this terminal's wrapper for a profile the gateway made on this machine, a failure becoming a warning of the receipt
func withLocalAlias(made *tuigateway.ProfileCreateReceipt, name string) {
	var wrapper, collision string
	err := func() (err error) {
		defer func() {
			if failure := recover(); failure != nil {
				err = fmt.Errorf("%v", failure)
			}
		}()
		wrapper, collision, err = manage.WriteAlias(name)
		return err
	}()
	if err != nil {
		pkgLog().Warn(fmt.Sprintf("profile %s: alias wrapper failed", name), "error", err)
		made.Warnings = append(made.Warnings, "alias wrapper: "+manage.CreateErrorText(err))
		return
	}
	made.AliasPath, made.AliasCollision = nil, nil
	if wrapper != "" {
		made.AliasPath = &wrapper
	}
	if collision != "" {
		made.AliasCollision = &collision
	}
}
