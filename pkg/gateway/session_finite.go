package gateway

import "github.com/nickheyer/icebath/pkg/internal/pyjson"

// Wire client of a submission, the connection and session an unattended turn is judged against
type finiteSubmitter struct {
	connection any
	ref        SessionRef
}

// Committed finite flags of a submission, an unattended turn refused unless it is finite and allowed
func admitFiniteFor(params map[string]any, submitter *finiteSubmitter) (*pyjson.Object, error) {
	unattended, hasUnattended := params["unattended"]
	flag, isFlag := unattended.(bool)
	if hasUnattended && (!isFlag || (flag && params["finite"] != true)) {
		return nil, runtimeRefusal("invalid_params")
	}
	if pyTruthy(unattended) && submitter != nil && !mayRunUnattended(submitter.connection, submitter.ref) {
		return nil, runtimeRefusal("invalid_params")
	}
	finite, hasFinite := params["finite"]
	if !hasFinite {
		return pyjson.NewObject(), nil
	}
	bounded, isBool := finite.(bool)
	if !isBool {
		return nil, runtimeRefusal("invalid_params")
	}
	out := pyjson.ObjectOf("finite", bounded)
	if pyTruthy(unattended) {
		out.Set("unattended", true)
	}
	return out, nil
}

// Committed finite flags of an owner side submission no wire client sent
func admitFinite(params map[string]any) (*pyjson.Object, error) {
	return admitFiniteFor(params, nil)
}
