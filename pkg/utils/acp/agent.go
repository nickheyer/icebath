package acputil

import (
	"context"
	"encoding/json"
)

// Agent method answering initialize
type Initializer interface {
	Initialize(ctx context.Context, req *InitializeRequest) (*InitializeResponse, error)
}

// Agent method answering authenticate, a nil response sent as an empty object
type Authenticator interface {
	Authenticate(ctx context.Context, req *AuthenticateRequest) (*AuthenticateResponse, error)
}

// Agent method answering session/new
type SessionCreator interface {
	NewSession(ctx context.Context, req *NewSessionRequest) (*NewSessionResponse, error)
}

// Agent method answering session/load, a nil response sent as an empty object
type SessionLoader interface {
	LoadSession(ctx context.Context, req *LoadSessionRequest) (*LoadSessionResponse, error)
}

// Agent method answering session/list
type SessionLister interface {
	ListSessions(ctx context.Context, req *ListSessionsRequest) (*ListSessionsResponse, error)
}

// Agent method answering session/close, a nil response sent as an empty object
type SessionCloser interface {
	CloseSession(ctx context.Context, req *CloseSessionRequest) (*CloseSessionResponse, error)
}

// Agent method answering session/set_mode, a nil response sent as an empty object
type SessionModeSetter interface {
	SetSessionMode(ctx context.Context, req *SetSessionModeRequest) (*SetSessionModeResponse, error)
}

// Agent method answering session/prompt
type Prompter interface {
	Prompt(ctx context.Context, req *PromptRequest) (*PromptResponse, error)
}

// Agent method answering session/set_model, a nil response sent as an empty object
type SessionModelSetter interface {
	SetSessionModel(ctx context.Context, req *SetSessionModelRequest) (*SetSessionModelResponse, error)
}

// Agent method answering session/set_config_option for boolean and select options alike, a nil response sent as an empty object
type ConfigOptionSetter interface {
	SetConfigOption(ctx context.Context, req *SetConfigOptionParams) (*SetSessionConfigOptionResponse, error)
}

// Agent method answering session/fork
type SessionForker interface {
	ForkSession(ctx context.Context, req *ForkSessionRequest) (*ForkSessionResponse, error)
}

// Agent method answering session/resume
type SessionResumer interface {
	ResumeSession(ctx context.Context, req *ResumeSessionRequest) (*ResumeSessionResponse, error)
}

// Agent method receiving session/cancel
type Canceller interface {
	Cancel(ctx context.Context, req *CancelNotification) error
}

// Agent method answering extension requests, those whose method starts with an underscore, given the name after it and the params object
type ExtensionMethodHandler interface {
	ExtMethod(ctx context.Context, name string, params json.RawMessage) (any, error)
}

// Agent method receiving extension notifications, given the name after the underscore and the params object
type ExtensionNotificationHandler interface {
	ExtNotification(ctx context.Context, name string, params json.RawMessage) error
}

// Agent hook given the connection it is served on before any frame is read
type Connector interface {
	OnConnect(conn *AgentSideConnection)
}
