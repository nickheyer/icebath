package gateway

import (
	"context"
	"path/filepath"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/observability"
	"github.com/nickheyer/icebath/pkg/provider"
)

// Session and generation of the claim a chat route's turn runs under
type claimedTurnOwner struct {
	sessionID  string
	generation int64
}

// Claim the executing turn was admitted under, nil for an internal, API or local turn
func (e *MessageEvent) ClaimedOwner() (sessionID string, generation int64, ok bool) {
	eventTurnStateMu.Lock()
	defer eventTurnStateMu.Unlock()
	if e.claimedOwner == nil {
		return "", 0, false
	}
	return e.claimedOwner.sessionID, e.claimedOwner.generation, true
}

// Runner surface that executes an admitted event as one turn and returns its reply
type turnHandlerRunner interface {
	HandleMessage(ctx context.Context, event *MessageEvent) (any, error)
}

// Author of the admitted input for memory attribution, the producer's stamp else the forwarded peer of the policy
func rowTurnAuthor(policy *LocalSessionPolicy, row *pyjson.Object) any {
	payload := rowPayload(row)
	for _, key := range []string{"local_automation_v1", "api_turn_v1", "cli_turn_v1"} {
		value, _ := payload.Get(key)
		if stamped, ok := value.(*pyjson.Object); ok && stamped != nil {
			if author, _ := stamped.Get("turn_author"); author != nil {
				return author
			}
		}
	}
	if author := forwardAuthor(policy); author != nil {
		return author
	}
	return nil
}

// Text of an admitted row, the parts of an API turn standing until its prepared content replaces them
func admittedText(payload *pyjson.Object) (string, bool, error) {
	text, ok := payload.Get("text")
	if !ok {
		return "", false, &pyKeyError{"text"}
	}
	switch value := text.(type) {
	case string:
		return value, false, nil
	case []any:
		return "", true, nil
	}
	return "", false, &PyError{Kind: "TypeError", Msg: "admitted text must be str or list, not " + pyTypeName(text)}
}

// Event of a claimed row rebuilt from its committed payload
func admittedEvent(ctx context.Context, a *SessionAuthority, ref SessionRef, live *LiveSession, row *pyjson.Object) (*MessageEvent, bool, error) {
	payload := rowPayload(row)
	if _, native := payload.Get("native_text_v1"); native {
		event, err := restoreNative(ctx, payload, a.Runner)
		return event, false, err
	}
	text, parts, err := admittedText(payload)
	if err != nil {
		return nil, false, err
	}
	urls, types, err := restoreAttachments(ctx, payload)
	if err != nil {
		return nil, false, err
	}
	admissionID := rowString(row, "admission_id")
	event := NewMessageEvent(text)
	event.Source, event.MessageID, event.MediaURLs, event.MediaTypes = live.Source, &admissionID, urls, types
	if _, automation := payload.Get("local_automation_v1"); automation {
		event, err = restoreLocalAutomation(ctx, a, ref, row)
		return event, false, err
	}
	return event, parts, nil
}

// Text parts of a prepared API execution joined into the transcript text of its turn
func preparedText(prepared *pyjson.Object) (string, error) {
	content, ok := prepared.Get("content")
	if !ok {
		return "", &pyKeyError{"content"}
	}
	items, _ := content.([]any)
	lines := []string{}
	for _, item := range items {
		part, isPart := item.(*pyjson.Object)
		if !isPart {
			return "", &PyError{Kind: "AttributeError", Msg: "'" + pyTypeName(item) + "' object has no attribute 'get'"}
		}
		if kind, _ := part.Get("type"); kind != "text" {
			continue
		}
		text, has := part.Get("text")
		if !has {
			return "", &pyKeyError{"text"}
		}
		line, isText := text.(string)
		if !isText {
			return "", &PyError{Kind: "TypeError", Msg: "sequence item 0: expected str instance, " + pyTypeName(text) + " found"}
		}
		lines = append(lines, line)
	}
	return strings.Join(lines, "\n"), nil
}

// Runs a claimed admission through the runner's turn handler and records its result, reply and crash marker for the drain
func executeAdmission(ctx context.Context, a *SessionAuthority, ref SessionRef, row *pyjson.Object) (reply any, retErr error) {
	admissionID := rowString(row, "admission_id")
	observability.BusyTurnStarted(ctx, admissionID)
	noted, err := noteIfUnviewed(ctx, a, ref, row)
	if err != nil {
		return nil, err
	}
	if noted {
		return "", nil
	}
	live, ok := a.live(ref.SessionID)
	if !ok {
		return nil, &pyKeyError{ref.SessionID}
	}
	policy, err := policyForSource(ctx, a.Runner, live.Source)
	if err != nil {
		return nil, err
	}
	if policy != nil && policy.Source == "cron" {
		return CronExecute(ctx, a, ref, row, policy)
	}
	localPolicy := policy
	managed, err := managedPolicy(ctx, a, ref)
	if err != nil {
		return nil, err
	}
	if managed != nil {
		return admitManaged(ctx, a, ref, row, managed)
	}
	a.mu.Lock()
	route := live.Route
	native := a.nativeWaiters[admissionID]
	delete(a.nativeWaiters, admissionID)
	a.mu.Unlock()
	payload := rowPayload(row)
	envelope, isNative := payload.Get("native_text_v1")
	event, textParts, err := admittedEvent(ctx, a, ref, live, row)
	if err != nil {
		return nil, err
	}
	if sessionID, stamped := event.Metadata["gateway_session_id"]; event.Internal && stamped {
		proven := sessionID
		if isNative {
			target, err := a.PhysicalTarget(ctx, ref)
			if err != nil {
				return nil, err
			}
			proven = target
		}
		event.Metadata["gateway_proven_session_id"] = proven
	}
	var provenance any
	if wrapped, isEnvelope := envelope.(*pyjson.Object); isEnvelope && wrapped != nil {
		provenance, _ = wrapped.Get("provenance")
	}
	var home *string
	if provenance != nil {
		if home, err = restoreProvenance(ctx, a.Runner, event.Source, provenance); err != nil {
			return nil, err
		}
	}
	if live.Source == nil {
		return nil, &PyError{Kind: "AttributeError", Msg: "'NoneType' object has no attribute 'platform'"}
	}
	isAPI := live.Source.Platform == PlatformAPIServer
	if !(event.Internal || isAPI || live.Source.Platform == PlatformLocal) {
		eventTurnStateMu.Lock()
		event.claimedOwner = &claimedTurnOwner{sessionID: ref.SessionID, generation: rowGeneration(row)}
		eventTurnStateMu.Unlock()
	}
	author := event.Metadata["turn_author"]
	if author == nil {
		author = rowTurnAuthor(localPolicy, row)
	}
	ctx = context.WithValue(ctx, admissionAuthorKey{}, author)
	var viewer *LiveSession
	if isViewerInput(row) {
		viewer = live
	}
	ctx = context.WithValue(ctx, viewerTurnKey{}, viewer)
	ctx = bindAPIExecution(ctx, nil)
	captured := pyjson.NewObject()
	ctx = withExecutionResult(ctx, captured)
	ctx = context.WithValue(ctx, executingAdmissionKey{}, true)
	event.setTurnMarkerHandoff(true)
	handed := false
	defer func() {
		closing := context.WithoutCancel(ctx)
		if !native {
			if err := takePostDelivery(closing, a, admissionID, event.Source, route); err != nil {
				reply, retErr = nil, err
				return
			}
		}
		if !handed {
			if err := releaseAdmissionTurnMarker(closing, a, event); err != nil {
				reply, retErr = nil, err
			}
		}
	}()
	var response any
	err = admissionScope(ctx, a, home, row, event, captured, func(scope context.Context) error {
		if isAPI {
			prepared, err := prepareAPIExecution(scope, a, ref, payload)
			if err != nil {
				return err
			}
			scope = bindAPIExecution(scope, prepared)
			if textParts {
				if event.Text, err = preparedText(prepared); err != nil {
					return err
				}
			}
			event.AllowGatewayControl = false
			event.Internal = true
		} else if textParts {
			return &PyError{Kind: "TypeError", Msg: "admitted text must be str, not list"}
		}
		handler, canHandle := a.Runner.(turnHandlerRunner)
		if !canHandle {
			return &PyError{Kind: "AttributeError", Msg: "'" + pyTypeName(a.Runner) + "' object has no attribute '_handle_message'"}
		}
		var err error
		if response, err = handler.HandleMessage(scope, event); err != nil {
			return err
		}
		result, has := captured.Get("terminal_result")
		if !has {
			result, _ = captured.Get("result")
		}
		if result == nil {
			final := response
			if !pyTruthy(final) {
				final = ""
			}
			result = pyjson.ObjectOf("final_response", final, "messages", []any{})
			command, isCommand := event.GetCommand()
			if finite, _ := payload.Get("finite"); isAPI || (finite == true && !(isCommand && command != "")) {
				failure := response
				if !pyTruthy(failure) {
					failure = "The admitted turn failed."
				}
				result = pyjson.ObjectOf("final_response", "", "messages", []any{}, "failed", true, "completed", false, "error", failure)
			}
		}
		usage, has := captured.Get("usage")
		if !has {
			usage = pyjson.NewObject()
		}
		pending := pyjson.ObjectOf("result", result, "usage", usage)
		if sessionUsage, has := captured.Get("session_usage"); has {
			pending.Set("session_usage", sessionUsage)
		}
		a.mu.Lock()
		a.PendingResults[admissionID] = pending
		a.mu.Unlock()
		if !isAPI && pyTruthy(response) {
			adapter, err := a.Runner.AdapterForSource(scope, event.Source)
			if err != nil {
				return err
			}
			if adapter != nil {
				a.mu.Lock()
				delivery := &settledDelivery{adapter: adapter, event: event, route: live.Route, response: response, home: home}
				if native {
					waiterReplies(a)[admissionID] = delivery
				} else {
					deliveryRegistry(&a.pendingDeliveries)[admissionID] = delivery
				}
				a.mu.Unlock()
			}
		}
		handed = true
		return handOverTurnMarker(scope, a, admissionID, event)
	})
	if err != nil {
		return nil, err
	}
	return response, nil
}

// Context of a standalone launch profile's turn marked as the launch body, unchanged for any other owner
func launchBodyIfLaunchOwner(ctx context.Context, a *SessionAuthority) context.Context {
	if !filepath.IsAbs(a.ProfileID) || !provider.IsProcessHome(a.ProfileID) {
		return ctx
	}
	return provider.WithLaunchBody(ctx)
}

// Runs the execution inside the producer bracket of its row and under the execution scope
func admissionScope(ctx context.Context, a *SessionAuthority, home *string, row *pyjson.Object, event *MessageEvent, captured *pyjson.Object, run func(ctx context.Context) error) error {
	return producerTurn(ctx, a, row, event, captured, func(turn context.Context) error {
		return run(executionScope(turn, a, home))
	})
}

// Context of owner side execution, the owning profile's home under multiplex and the ambient scope otherwise
func executionScope(ctx context.Context, a *SessionAuthority, home *string) context.Context {
	multiplex := a.Runner != nil && a.Runner.MultiplexProfiles()
	if home == nil && !multiplex {
		return launchBodyIfLaunchOwner(ctx, a)
	}
	scopeHome := ""
	if home != nil {
		scopeHome = *home
	} else if filepath.IsAbs(a.ProfileID) && isDir(a.ProfileID) {
		scopeHome = a.ProfileID
	}
	if scopeHome == "" {
		return ctx
	}
	return profileRuntimeScope(ctx, scopeHome, true)
}
