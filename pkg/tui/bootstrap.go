package tui

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/pm"
)

const tuiBootstrapRemedy = "gateway unavailable: run `icebath pm repair`, then restart the TUI"

// Launch variables that must not leak into a gateway owner the bootstrap starts
var tuiBootstrapScrubbedEnv = []string{"ICEBATH_MODEL", "ICEBATH_INFERENCE_MODEL", "ICEBATH_TUI_PROVIDER",
	"ICEBATH_INFERENCE_PROVIDER", "ICEBATH_TUI_TOOLSETS", "ICEBATH_TUI_SKILLS",
	"ICEBATH_CWD", "TERMINAL_CWD", "ICEBATH_YOLO", "ICEBATH_ACCEPT_HOOKS",
	"ICEBATH_TUI_CHECKPOINTS", "ICEBATH_TUI_PASS_SESSION_ID", "ICEBATH_KANBAN_BOARD",
	"ICEBATH_TUI_CLIENT_ENV"}

// Dependency environment the bootstrap could not activate, reported with the repair remedy
type tuiDependencyError struct{ msg string }

func (e *tuiDependencyError) Error() string { return e.msg }

// Source root the helpers activate, the launcher's ICEBATH_PYTHON_SRC_ROOT or the binary's tree
func HelperRoot() string {
	if root := os.Getenv("ICEBATH_PYTHON_SRC_ROOT"); root != "" {
		return root
	}
	return pm.RepoRoot()
}

// Verdict line the TUI shows when no owner can be attached, each detail line kept a verdict
func tuiBootstrapFailureMessage(receipt gateway.ObservedEndpoint) string {
	reason := receipt.ReasonCode
	if reason == "" {
		reason = "not ready"
	}
	head := fmt.Sprintf("gateway %s: %s", receipt.State, reason)
	if receipt.Detail == "" {
		return head
	}
	if receipt.ReasonCode == "runtime_exited" {
		return head + "\n" + receipt.Detail
	}
	out := []string{head}
	for _, line := range strings.Split(strings.ReplaceAll(receipt.Detail, "\r\n", "\n"), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			out = append(out, fmt.Sprintf("gateway %s: %s", receipt.State, line))
		}
	}
	return strings.Join(out, "\n")
}

// Python type name of a decoded JSON value for the attribute error a non object raises
func pyTypeNameOf(v any) string {
	switch v.(type) {
	case nil:
		return "NoneType"
	case bool:
		return "bool"
	case string:
		return "str"
	case []any:
		return "list"
	case float64:
		return "float"
	default:
		return "int"
	}
}

// Field of a decoded JSON object, the attribute error text when the value is not an object
func pyGet(v any, key string) (any, error) {
	obj, ok := v.(*pyjson.Object)
	if !ok {
		return nil, fmt.Errorf("'%s' object has no attribute 'get'", pyTypeNameOf(v))
	}
	return obj.Vals[key], nil
}

// Ensures or discovers the gateway owner and mints the private interactive grant the TUI attaches with
func GatewayBootstrap(ctx context.Context, start, recover bool) (*pyjson.Object, error) {
	if err := pm.ActivateDependencies(ctx, HelperRoot()); err != nil {
		var none *pm.NoCommittedEnvironment
		if errors.As(err, &none) {
			return nil, &tuiDependencyError{msg: "dependency environment unavailable: " + none.Error()}
		}
		return nil, err
	}
	h, err := pypath.Resolve(home.Dir(ctx))
	if err != nil {
		return nil, err
	}
	for _, key := range tuiBootstrapScrubbedEnv {
		_ = os.Unsetenv(key)
	}
	var receipt gateway.ObservedEndpoint
	if start {
		if receipt, err = gateway.EnsureGatewayRuntime(ctx, h, 30, true); err != nil {
			return nil, err
		}
	} else {
		receipt = gateway.DiscoverGatewayEndpoint(ctx, h, 5*time.Second)
	}
	if !start && recover && !gateway.Attachable(receipt) {
		if receipt, err = gateway.EnsureGatewayRuntime(ctx, h, 30, true); err != nil {
			return nil, err
		}
	}
	if !gateway.Attachable(receipt) {
		return nil, errors.New(tuiBootstrapFailureMessage(receipt))
	}
	endpoint := receipt.Endpoint
	control := gateway.ControlHomeFor(h, endpoint)
	text, err := pyjson.Dumps(pyjson.ObjectOf("protocol", 1, "id", 1, "verb", "session-ticket", "params",
		pyjson.ObjectOf("profile_id", endpoint.ProfileID, "instance_id", endpoint.InstanceID, "purpose", "interactive")))
	if err != nil {
		return nil, err
	}
	raw, err := gateway.TUIBootstrapControlQuery(control, []byte(text+"\n"), 5*time.Second)
	if err != nil {
		return nil, err
	}
	result, err := pyjson.LoadsBytes(raw)
	if err != nil {
		return nil, err
	}
	rejected := errors.New("gateway private bootstrap rejected")
	ok, err := pyGet(result, "ok")
	if err != nil {
		return nil, err
	}
	if ok != true {
		return nil, rejected
	}
	id, _ := pyGet(result, "id")
	if !pyjson.Equal(id, int64(1)) {
		return nil, rejected
	}
	grant := any(pyjson.NewObject())
	if v, present := result.(*pyjson.Object).Vals["result"]; present {
		grant = v
	}
	instance, err := pyGet(grant, "instance_id")
	if err != nil {
		return nil, err
	}
	profile, _ := pyGet(grant, "profile_id")
	ticketValue, _ := pyGet(grant, "ticket")
	ticket, isStr := ticketValue.(string)
	if !pyjson.Equal(instance, endpoint.InstanceID) || !pyjson.Equal(profile, endpoint.ProfileID) || !isStr {
		return nil, rejected
	}
	url, protocols := gateway.GatewayWSTarget(endpoint, ticket)
	protocolValues := make([]any, len(protocols))
	for i, p := range protocols {
		protocolValues[i] = p
	}
	return pyjson.ObjectOf("url", url, "protocols", protocolValues, "profile_id", endpoint.ProfileID,
		"instance_id", endpoint.InstanceID), nil
}

// Verdict text of a failed bootstrap, the dependency remedy for an environment that could not activate
func BootstrapVerdict(err error) string {
	var dependency *tuiDependencyError
	if errors.As(err, &dependency) {
		return "gateway unavailable: bootstrap_dependencies\n" + dependency.msg + "\n" + tuiBootstrapRemedy
	}
	return err.Error()
}
