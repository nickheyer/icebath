package messagetools

import (
	"context"
	"fmt"
	"math/big"
	"strings"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Python exceptions a handler raises uncaught, named so the dispatcher reports their class
type ValueError struct{ Msg string }
type TypeError struct{ Msg string }
type OverflowError struct{ Msg string }
type AttributeError struct{ Msg string }

func (e *ValueError) Error() string     { return e.Msg }
func (e *TypeError) Error() string      { return e.Msg }
func (e *OverflowError) Error() string  { return e.Msg }
func (e *AttributeError) Error() string { return e.Msg }

// Python int of a tool argument, raising the int conversion error to the dispatcher
func mustPyInt(v any) *big.Int {
	n, err := pyint.Of(v)
	if err == nil {
		return n
	}
	pe, ok := err.(*pyint.Error)
	if !ok {
		panic(err)
	}
	switch pe.Kind {
	case "ValueError":
		panic(&ValueError{pe.Msg})
	case "OverflowError":
		panic(&OverflowError{pe.Msg})
	}
	panic(&TypeError{pe.Msg})
}

// Tool error envelope with the bounded message and non ASCII text kept
func toolError(message string) string {
	out := pyjson.NewObject()
	out.Set("error", tools.BoundErrorText(message))
	text, err := pyjson.Dumps(out, pyjson.EnsureASCII(false))
	if err != nil {
		return tools.ErrorResult(err)
	}
	return text
}

// Message reaction surface of the session database
type reactionStore interface {
	LatestMessageRowID(ctx context.Context, sessionID, role string, offset int, requireText bool) (int64, bool, error)
	GetMessageRole(ctx context.Context, sessionID string, rowID int64) (string, bool, error)
	SetMessageReaction(ctx context.Context, sessionID string, rowID int64, emoji *string, author string) ([]any, bool, error)
}

// Session database of the profile owning the turn with its release, nil when it cannot be opened
var openReactionSessionDB = func(ctx context.Context, env *tools.Env) (reactionStore, func()) {
	path := ""
	if env != nil && env.Home != "" {
		path = home.StatePath(env.Home)
	}
	db, err := state.Acquire(ctx, path)
	if err != nil {
		return nil, nil
	}
	return db, func() { state.ReleaseOrClose(db) }
}

// Sets or with an empty emoji retracts the agent reaction on a message of the durable session
func reactToMessageTool(ctx context.Context, env *tools.Env, emojiArg, messageRowID, messagesBack any) string {
	emoji := ""
	switch e := emojiArg.(type) {
	case string:
		emoji = pystr.Strip(e)
	default:
		if pyrepr.Truthy(e) {
			panic(&AttributeError{fmt.Sprintf("'%s' object has no attribute 'strip'", pyrepr.TypeName(e))})
		}
	}
	sessionKey := tools.GetSessionEnv(env, "ICEBATH_SESSION_ID", "")
	if sessionKey == "" {
		sessionKey = tools.GetSessionEnv(env, "ICEBATH_SESSION_KEY", "")
	}
	if sessionKey == "" {
		return toolError("No active session — reactions need a persisted conversation.")
	}
	db, release := openReactionSessionDB(ctx, env)
	if db == nil {
		return toolError("Session storage is unavailable.")
	}
	defer release()
	var rowID int64
	targetRole := "user"
	if messageRowID == nil {
		back := int64(0)
		if pyrepr.Truthy(messagesBack) {
			back = max(0, mustPyInt(messagesBack).Int64())
		}
		id, found, err := db.LatestMessageRowID(ctx, sessionKey, "user", int(back), true)
		if err != nil {
			panic(err)
		}
		if !found {
			if back > 0 {
				return toolError(fmt.Sprintf("No user message found %d back.", back))
			}
			return toolError("No user message to react to yet.")
		}
		rowID = id
	} else {
		rowID = mustPyInt(messageRowID).Int64()
		role, found, err := db.GetMessageRole(ctx, sessionKey, rowID)
		if err != nil {
			panic(err)
		}
		if found && role != "" {
			targetRole = role
		}
	}
	var emojiArgOut *string
	if emoji != "" {
		emojiArgOut = &emoji
	}
	reactions, found, err := db.SetMessageReaction(ctx, sessionKey, rowID, emojiArgOut, "agent")
	if err != nil {
		return toolError("Failed to set the reaction: " + err.Error())
	}
	if !found {
		shown := pyrepr.Str(messageRowID)
		if messageRowID == nil {
			shown = fmt.Sprint(rowID)
		}
		return toolError(fmt.Sprintf("Message %s is not part of this conversation.", shown))
	}
	if reactions == nil {
		reactions = []any{}
	}
	tools.DesktopEmitEvent(env, "message.reaction", map[string]any{"row_id": rowID, "reactions": reactions, "role": targetRole})
	text, err := state.DumpsUnicode(reactions)
	if err != nil {
		panic(err)
	}
	return fmt.Sprintf(`{"success": true, "row_id": %d, "reactions": %s}`, rowID, text)
}

// Opt in Appearance switch, the desktop_ui toolset already limiting the tool to GUI sessions
func checkReactRequirements(env *tools.Env) bool {
	return tools.DesktopUserEnabled(env, "message_reactions", false)
}

var reactToMessageSchema = map[string]any{
	"name": "react_to_message",
	"description": strings.Join([]string{
		"React to a message with a single emoji, the way you'd tapback in iMessage. ",
		"Reach for it when a reaction is what a person would do: something funny gets ",
		"a 😂, warmth gets a ❤️, a plan you're on board with gets a 👍 — then just ",
		"carry on with whatever the message actually needs. If a reaction says it ",
		"all, it can BE the reply (skip the redundant 'sounds good!' turn). Use it ",
		"like a person would: occasionally, when felt — not on every message, and ",
		"never as a status signal. NEVER narrate or explain a reaction ('I reacted ",
		"with...', 'Reacting now') — the emoji appearing on the bubble is the whole ",
		"point, and commentary kills it. Defaults to the user's most recent message. ",
		"One reaction per message: a different emoji replaces yours, an empty string ",
		"retracts it.",
	}, ""),
	"parameters": map[string]any{
		"type": "object",
		"properties": map[string]any{
			"emoji": map[string]any{
				"type": "string",
				"description": "The emoji to react with (e.g. '❤️', '😂', '👍'). Pass an empty " +
					"string to remove your reaction.",
			},
			"message_row_id": map[string]any{
				"type": "integer",
				"description": "Optional. The specific message to react to. Omit to react to the " +
					"user's latest message, which is almost always what you want.",
			},
			"messages_back": map[string]any{
				"type": "integer",
				"description": "Optional. React to an EARLIER user message: 1 = the one before " +
					"the latest, 2 = two before, and so on. For when something lands " +
					"late — the joke you only got after answering.",
			},
		},
		"required": []any{"emoji"},
	},
}

var reactToMessageDef = tools.Def{Name: "react_to_message", Toolset: "desktop_ui", Schema: reactToMessageSchema,
	Check: checkReactRequirements, Emoji: "\U0001F49B",
	Handler: func(ctx context.Context, call *tools.Call) string {
		emoji, ok := call.Args["emoji"]
		if !ok {
			emoji = ""
		}
		return reactToMessageTool(ctx, call.Env, emoji, call.Args["message_row_id"], call.Args["messages_back"])
	}}

func init() { tools.Register(reactToMessageDef) }
