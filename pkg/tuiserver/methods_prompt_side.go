package tuiserver

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/tuigateway"
)

var promptSideRegistry = &handlerRegistry{}

func init() {
	promptSideRegistry.method("prompt.background", promptBackground)
	promptSideRegistry.method("prompt.btw", promptBtw)
	promptSideRegistry.install()
}

// Final response of a side turn result, the whole result as text when it is no dict
func finalResponseText(result any) string {
	if dict, isDict := result.(map[string]any); isDict {
		if final, present := dict["final_response"]; present {
			return pyrepr.Str(final)
		}
	}
	return pyrepr.Str(result)
}

// Fresh side task id under a prefix, six hex characters of a uuid4
func sideTaskID(prefix string) string {
	return prefix + "_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:6]
}

// Shared admission of the side agent RPCs, the session, the text, the parent sid and a fresh task id
func sideAgentArgs(ctx context.Context, rid any, params map[string]any, prefix string) (*tuigateway.Session, any, string, string, map[string]any) {
	session, errResp := sess(ctx, params, rid)
	if errResp != nil {
		return nil, nil, "", "", errResp
	}
	text := params["text"]
	if !truthy(text) {
		return nil, nil, "", "", rpcErr(rid, 4012, "text required", nil)
	}
	parent := ""
	if value, present := params["session_id"]; present {
		parent = pyrepr.Str(value)
	}
	return session, text, parent, sideTaskID(prefix), nil
}

// Runs body on a session work thread in the session profile and cwd, its text or error emitted on parent
func spawnSideAgent(ctx context.Context, rid any, session *tuigateway.Session, taskID, parent, event string,
	body func(ctx context.Context) (string, error), extra map[string]any) map[string]any {
	run := func(workCtx context.Context) {
		dir := sessionCwd(workCtx, session)
		bound, env := setSessionContext(workCtx, taskID, &dir, parent)
		defer clearSessionContext(env)
		payload := map[string]any{"task_id": taskID}
		for key, value := range extra {
			payload[key] = value
		}
		var text string
		var err error
		if failure := catchPanic(func() { text, err = body(sessionProfileRuntimeScope(bound, session, true)) }); failure != nil {
			err = errors.New(errorMessage(failure))
		}
		if err != nil {
			payload["text"] = "error: " + err.Error()
		} else {
			payload["text"] = text
		}
		tuigateway.Emit(bound, event, parent, payload)
	}
	startSessionWork(ctx, run, "side-agent-"+taskID, nil)
	return ok(rid, map[string]any{"task_id": taskID})
}

// Background prompt run to completion by a fresh side agent on the session's route, answered as background.complete
func promptBackground(ctx context.Context, rid any, params map[string]any) map[string]any {
	session, text, parent, taskID, errResp := sideAgentArgs(ctx, rid, params, "bg")
	if errResp != nil {
		return errResp
	}
	body := func(ctx context.Context) (string, error) {
		parentAgent := sessionAgent(session)
		if parentAgent == nil {
			return "", errors.New("'NoneType' object has no attribute 'model'")
		}
		opts := backgroundAgentKwargs(ctx, parentAgent, taskID)
		store, release, err := sideAgentSessionDB(ctx, opts.SessionDB)
		if err != nil {
			return "", err
		}
		defer release()
		opts.SessionDB = store
		side, err := agent.NewAIAgent(ctx, opts)
		if err != nil {
			return "", err
		}
		defer suppress(func() { _ = side.Close() })
		result, err := side.RunConversationContent(ctx, text, agent.RunConversationOptions{TaskID: taskID})
		if err != nil {
			return "", err
		}
		return finalResponseText(result), nil
	}
	return spawnSideAgent(ctx, rid, session, taskID, parent, "background.complete", body, nil)
}

// Side question over a snapshot of the live conversation, history and prompt cache untouched, answered as btw.complete
func promptBtw(ctx context.Context, rid any, params map[string]any) map[string]any {
	session, text, parent, taskID, errResp := sideAgentArgs(ctx, rid, params, "btw")
	if errResp != nil {
		return errResp
	}
	parentAgent := sessionAgent(session)
	var snapshot []map[string]any
	if parentAgent != nil {
		snapshot = agent.SessionMessageDicts(parentAgent.SessionMessages())
	}
	if len(snapshot) == 0 {
		for _, item := range historyList(session.Get("history")) {
			if row, isDict := item.(map[string]any); isDict {
				snapshot = append(snapshot, row)
			}
		}
	}
	mainRuntime := map[string]any{"model": nil, "provider": nil, "base_url": nil, "api_key": nil, "api_mode": nil, "session_id": nil}
	if parentAgent != nil {
		mainRuntime = map[string]any{"model": parentAgent.Model(), "provider": parentAgent.Provider(), "base_url": parentAgent.BaseURL(),
			"api_key": parentAgent.APIKey(), "api_mode": parentAgent.APIMode(), "session_id": parentAgent.SessionID()}
	}
	body := func(ctx context.Context) (string, error) {
		question, isText := text.(string)
		if !isText {
			return "", fmt.Errorf("'%s' object has no attribute 'strip'", pyrepr.TypeName(text))
		}
		opts := agent.NewSideQuestionOptions()
		opts.Parent, opts.MainRuntime = parentAgent, mainRuntime
		return agent.AnswerSideQuestion(ctx, question, snapshot, opts)
	}
	return spawnSideAgent(ctx, rid, session, taskID, parent, "btw.complete", body, map[string]any{"question": text})
}
