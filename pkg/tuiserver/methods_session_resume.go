package tuiserver

import (
	"context"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/state"
)

// Per call session.resume state, ownsDB marking a dedicated profile handle that is still ours to close
type resumeCtx struct {
	ctx                                                  context.Context
	rid                                                  any
	params                                               map[string]any
	target                                               string
	db                                                   *state.DB
	ownsDB                                               bool
	found                                                *pyjson.Object
	profileResumeCwd                                     string
	cols                                                 int
	profile                                              string
	profileHome                                          string
	lazy, deferHistory, omitMessages, eagerBuild, inline bool
}

// Fields of a common resume payload
type resumeResponseOptions struct {
	info         map[string]any
	display      []any
	countSource  []any
	messages     []any
	messageCount *int
	running      bool
	status       string
	hydrating    *bool
	startedAt    any
	autoContinue map[string]any
}
