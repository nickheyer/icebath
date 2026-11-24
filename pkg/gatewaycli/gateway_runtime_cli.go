package gatewaycli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"strconv"
	"strings"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/selfupdate/updaterecord"
	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
)

// Process exit status of an ensure verdict, deadline 5, authorization 4, else by state
func EnsureExitCode(result gateway.ObservedEndpoint) int {
	switch result.ReasonCode {
	case "deadline":
		return 5
	case "authorization", "profile_mismatch", "gateway_elevated", "gateway_other_session":
		return 4
	}
	return map[string]int{"ready": 0, "incompatible": 3, "draining": 6, "inaccessible": 7, "conflict": 7, "starting": 5, "absent": 5}[result.State]
}

// None for an empty string field, the dataclass field otherwise
func noneIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// asdict of an ensure verdict and its endpoint
func ensurePayload(result gateway.ObservedEndpoint) *jsonutil.Object {
	payload := jsonutil.NewObject()
	payload.Set("state", result.State)
	payload.Set("reason_code", noneIfEmpty(result.ReasonCode))
	payload.Set("detail", noneIfEmpty(result.Detail))
	if ep := result.Endpoint; ep != nil {
		endpoint := jsonutil.NewObject()
		endpoint.Set("profile_id", ep.ProfileID)
		endpoint.Set("instance_id", ep.InstanceID)
		endpoint.Set("authority_epoch", ep.AuthorityEpoch)
		endpoint.Set("runtime_protocol", int64(ep.RuntimeProtocol))
		endpoint.Set("api_origin", ep.APIOrigin)
		endpoint.Set("supervisor", ep.Supervisor)
		endpoint.Set("control_home", noneIfEmpty(ep.ControlHome))
		endpoint.Set("code_sha", noneIfEmpty(ep.CodeSHA))
		payload.Set("endpoint", endpoint)
	} else {
		payload.Set("endpoint", nil)
	}
	return payload
}

// Whether err is the ValueError or TypeError a bad invocation raises
func invalidInvocation(err error) bool {
	var typed interface{ ErrorKind() string }
	if errors.As(err, &typed) {
		name := typed.ErrorKind()
		return name == "ValueError" || name == "TypeError"
	}
	return false
}

// Ensures a local runtime for this home and prints one credential free JSON verdict, exiting with its code
func CmdGatewayEnsure(ctx context.Context, ns map[string]any, w io.Writer) (int, error) {
	invalid := func() (int, error) {
		payload := jsonutil.NewObject()
		payload.Set("endpoint", nil)
		payload.Set("reason_code", "invalid_invocation")
		payload.Set("state", "inaccessible")
		text, err := jsonutil.Encode(payload, jsonutil.SortKeys())
		if err != nil {
			return 1, err
		}
		fmt.Fprintln(w, text)
		return 2, nil
	}
	raw, _ := ns["timeout"].(string)
	timeout, err := strconv.ParseFloat(strings.TrimSpace(raw), 64)
	if err != nil || math.IsNaN(timeout) || math.IsInf(timeout, 0) || timeout <= 0 {
		return invalid()
	}
	result, err := gateway.EnsureGatewayRuntime(ctx, home.Dir(ctx), timeout, false)
	if err != nil {
		if invalidInvocation(err) {
			return invalid()
		}
		return 1, err
	}
	payload := ensurePayload(result)
	identity, err := updaterecord.GetCodeIdentity(false)
	if err != nil {
		return 1, err
	}
	payload.Set("client_code_sha", identity.ValueOr("sha", nil))
	text, err := jsonutil.Encode(payload, jsonutil.SortKeys())
	if err != nil {
		return 1, err
	}
	fmt.Fprintln(w, text)
	return EnsureExitCode(result), nil
}
