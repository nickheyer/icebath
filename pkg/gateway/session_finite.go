package gateway

import (
	"context"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

type finiteTurnKey struct{}

// Finite flags bound to the executing admission
type finiteTurnState struct {
	finite     any
	unattended bool
}

// Reply a clarify prompt gets on a finite turn nobody can answer
const finiteClarifyNotice = "single-query mode: no user available to answer. Pick the best choices using your own " +
	"judgment, or make the most reasonable assumption you can, and continue."

// Finite flag of the bound owner turn, nil outside one so a standalone CLI keeps its own marker
func finiteTurnRequired(ctx context.Context) any {
	if bound, ok := ctx.Value(finiteTurnKey{}).(finiteTurnState); ok {
		return bound.finite
	}
	return nil
}

// Headless answer of the clarify tool on a finite turn
func finiteClarifyReply() *pyjson.Object {
	return pyjson.ObjectOf("answers", pyjson.NewObject(), "outcome", "undelivered", "notice", finiteClarifyNotice)
}

// True while the bound turn is a finite one that auto approves because nobody can answer a prompt
func UnattendedTurn(ctx context.Context) bool {
	bound, _ := ctx.Value(finiteTurnKey{}).(finiteTurnState)
	return bound.unattended
}

// Context binding an admission's finite flag, unattended only when the turn is finite too
func finiteTurnScope(ctx context.Context, finite any, unattended bool) context.Context {
	return context.WithValue(ctx, finiteTurnKey{}, finiteTurnState{finite: finite, unattended: pyTruthy(finite) && unattended})
}

// Wire client of a submission, the connection and session an unattended turn is judged against
type finiteSubmitter struct {
	connection any
	ref        SessionRef
}

// Connection surface an unattended turn is judged against
type unattendedConnection interface {
	ConnectionAuthority() *SessionAuthority
	NativeOwner() bool
	Actor() Principal
}

// Committed finite flags of a submission, an unattended turn refused unless it is finite and allowed
func admitFiniteFor(ctx context.Context, params map[string]any, submitter *finiteSubmitter) (*pyjson.Object, error) {
	unattended, hasUnattended := params["unattended"]
	flag, isFlag := unattended.(bool)
	if hasUnattended && (!isFlag || (flag && params["finite"] != true)) {
		return nil, runtimeRefusal("invalid_params")
	}
	if pyTruthy(unattended) && submitter != nil {
		allowed, err := mayRunUnattended(ctx, submitter.connection, submitter.ref)
		if err != nil {
			return nil, err
		}
		if !allowed {
			return nil, runtimeRefusal("invalid_params")
		}
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
	return admitFiniteFor(context.Background(), params, nil)
}

// True for the local CLI principal submitting on its own CLI surface session over the private bootstrap ticket
func mayRunUnattended(ctx context.Context, connection any, ref SessionRef) (bool, error) {
	submitter, ok := connection.(unattendedConnection)
	if !ok {
		return false, &PyError{Kind: "AttributeError", Msg: "'" + pyTypeName(connection) + "' object has no attribute 'authority'"}
	}
	authority := submitter.ConnectionAuthority()
	live, ok := authority.live(ref.SessionID)
	if !ok {
		return false, &pyKeyError{ref.SessionID}
	}
	source := live.Source
	if !submitter.NativeOwner() || source == nil || source.UserID == nil || *source.UserID != submitter.Actor().Subject {
		return false, nil
	}
	policy, err := policyForSource(ctx, authority.Runner, source)
	if err != nil {
		return false, err
	}
	return policy != nil && policy.Platform == "cli", nil
}

// Execution of a claimed admission, replaced by tests that settle a turn without a runner
var executeFiniteAdmission func(ctx context.Context, a *SessionAuthority, ref SessionRef, row *pyjson.Object) (any, error)

func init() { executeFiniteAdmission = runFiniteAdmission }

// Runs a claimed admission with its finite, surface, display, submission and CLI turn fields bound
func runFiniteAdmission(ctx context.Context, a *SessionAuthority, ref SessionRef, row *pyjson.Object) (any, error) {
	payload := rowPayload(row)
	field := func(key string) any {
		value, _ := payload.Get(key)
		return value
	}
	finite, hasFinite := payload.Get("finite")
	if !hasFinite {
		finite = false
	}
	admissionID := rowString(row, "admission_id")
	ctx = finiteTurnScope(ctx, finite, field("unattended") == true)
	ctx = surfaceTurnScope(ctx, field("surface_v1"))
	display, _ := pyjson.Plain(field("display_v1")).(map[string]any)
	ctx = DisplayTurnScope(ctx, admissionID, display)
	ctx = SubmissionTurnScope(ctx, admissionID, rowString(row, "request_id"))
	ctx = cliTurnScope(ctx, admissionID, field("cli_turn_v1"))
	return executeAdmission(ctx, a, ref, row)
}
