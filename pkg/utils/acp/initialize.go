package acputil

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"

	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
)

// Params of initialize: the protocol version the client speaks, its capabilities and who it is
type InitializeRequest struct {
	Meta               *jsonutil.Object    `json:"_meta,omitempty"`
	ClientCapabilities *ClientCapabilities `json:"clientCapabilities,omitempty"`
	ClientInfo         *Implementation     `json:"clientInfo,omitempty"`
	ProtocolVersion    int                 `json:"protocolVersion"`
}

// Fills the request from its JSON object, which must carry protocolVersion: an integer, or a string from a client older than version 1
func (r *InitializeRequest) UnmarshalJSON(data []byte) error {
	type plain InitializeRequest
	var w struct {
		*plain
		ProtocolVersion json.RawMessage `json:"protocolVersion"`
	}
	w.plain = (*plain)(r)
	if err := decodeObject(data, &w, "InitializeRequest", "protocolVersion"); err != nil {
		return err
	}
	version, err := decodeProtocolVersion(w.ProtocolVersion)
	r.ProtocolVersion = version
	return err
}

// Protocol version of a JSON value: an integer, also when written with a zero fraction or as a string of digits; any other string comes from a peer older than version 1, which named versions by strings, and reads as 0
func decodeProtocolVersion(raw []byte) (int, error) {
	raw = bytes.TrimSpace(raw)
	if s, ok := unquote(raw); ok {
		if n, err := strconv.Atoi(strings.TrimSpace(s)); err == nil {
			return n, nil
		}
		return 0, nil
	}
	text := string(raw)
	if n, err := strconv.Atoi(text); err == nil {
		return n, nil
	}
	f, err := strconv.ParseFloat(text, 64)
	if err != nil || f != math.Trunc(f) || f < math.MinInt || f >= math.MaxInt {
		return 0, fmt.Errorf("protocolVersion is %s, not an integer", describe(raw))
	}
	return int(f), nil
}

// Result of initialize: the protocol version the agent speaks, its capabilities, who it is and how a user signs in
type InitializeResponse struct {
	Meta              *jsonutil.Object   `json:"_meta,omitempty"`
	AgentCapabilities *AgentCapabilities `json:"agentCapabilities,omitempty"`
	AgentInfo         *Implementation    `json:"agentInfo,omitempty"`
	AuthMethods       []AuthMethod       `json:"authMethods,omitzero"`
	ProtocolVersion   int                `json:"protocolVersion"`
}

// Fills the response from its JSON object, which must carry protocolVersion: an integer, or a string from an agent older than version 1
func (r *InitializeResponse) UnmarshalJSON(data []byte) error {
	type plain InitializeResponse
	var w struct {
		*plain
		ProtocolVersion json.RawMessage   `json:"protocolVersion"`
		AuthMethods     []json.RawMessage `json:"authMethods"`
	}
	w.plain = (*plain)(r)
	if err := decodeObject(data, &w, "InitializeResponse", "protocolVersion"); err != nil {
		return err
	}
	version, err := decodeProtocolVersion(w.ProtocolVersion)
	if err != nil {
		return err
	}
	r.ProtocolVersion = version
	methods, err := decodeEach(w.AuthMethods, "authMethods", decodeAuthMethod)
	r.AuthMethods = methods
	return err
}

// What a client supports: auth, file system and terminal calls
type ClientCapabilities struct {
	Meta     *jsonutil.Object        `json:"_meta,omitempty"`
	Auth     *AuthCapabilities       `json:"auth,omitempty"`
	FS       *FileSystemCapabilities `json:"fs,omitempty"`
	Terminal *bool                   `json:"terminal,omitempty"`
}

// Auth features a client supports
type AuthCapabilities struct {
	Meta     *jsonutil.Object `json:"_meta,omitempty"`
	Terminal *bool            `json:"terminal,omitempty"`
}

// File system calls a client serves
type FileSystemCapabilities struct {
	Meta          *jsonutil.Object `json:"_meta,omitempty"`
	ReadTextFile  *bool            `json:"readTextFile,omitempty"`
	WriteTextFile *bool            `json:"writeTextFile,omitempty"`
}

// What an agent supports: loading sessions, MCP transports, prompt content and session methods
type AgentCapabilities struct {
	Meta                *jsonutil.Object     `json:"_meta,omitempty"`
	LoadSession         *bool                `json:"loadSession,omitempty"`
	MCPCapabilities     *MCPCapabilities     `json:"mcpCapabilities,omitempty"`
	PromptCapabilities  *PromptCapabilities  `json:"promptCapabilities,omitempty"`
	SessionCapabilities *SessionCapabilities `json:"sessionCapabilities,omitempty"`
}

// MCP transports an agent connects over besides stdio
type MCPCapabilities struct {
	Meta *jsonutil.Object `json:"_meta,omitempty"`
	HTTP *bool            `json:"http,omitempty"`
	SSE  *bool            `json:"sse,omitempty"`
}

// Prompt content an agent accepts besides text and resource links
type PromptCapabilities struct {
	Meta            *jsonutil.Object `json:"_meta,omitempty"`
	Audio           *bool            `json:"audio,omitempty"`
	EmbeddedContext *bool            `json:"embeddedContext,omitempty"`
	Image           *bool            `json:"image,omitempty"`
}

// Session methods an agent serves beyond the required ones, each present when served
type SessionCapabilities struct {
	Meta   *jsonutil.Object           `json:"_meta,omitempty"`
	Close  *SessionCloseCapabilities  `json:"close,omitempty"`
	Fork   *SessionForkCapabilities   `json:"fork,omitempty"`
	List   *SessionListCapabilities   `json:"list,omitempty"`
	Resume *SessionResumeCapabilities `json:"resume,omitempty"`
}

// Marker that an agent serves session/close
type SessionCloseCapabilities struct {
	Meta *jsonutil.Object `json:"_meta,omitempty"`
}

// Marker that an agent serves session/fork
type SessionForkCapabilities struct {
	Meta *jsonutil.Object `json:"_meta,omitempty"`
}

// Marker that an agent serves session/list
type SessionListCapabilities struct {
	Meta *jsonutil.Object `json:"_meta,omitempty"`
}

// Marker that an agent serves session/resume
type SessionResumeCapabilities struct {
	Meta *jsonutil.Object `json:"_meta,omitempty"`
}

// Name, display title and version of a client or agent program
type Implementation struct {
	Meta    *jsonutil.Object `json:"_meta,omitempty"`
	Name    string           `json:"name"`
	Title   *string          `json:"title,omitempty"`
	Version string           `json:"version"`
}

// Fills the implementation from its JSON object, which must carry name and version
func (i *Implementation) UnmarshalJSON(data []byte) error {
	type plain Implementation
	return decodeObject(data, (*plain)(i), "Implementation", "name", "version")
}

// Way a user signs in to the agent: *AuthMethodAgent, *EnvVarAuthMethod or *TerminalAuthMethod
type AuthMethod interface{ isAuthMethod() }

func (*AuthMethodAgent) isAuthMethod()    {}
func (*EnvVarAuthMethod) isAuthMethod()   {}
func (*TerminalAuthMethod) isAuthMethod() {}

// Sign-in the agent runs itself when the client calls authenticate
type AuthMethodAgent struct {
	Meta        *jsonutil.Object `json:"_meta,omitempty"`
	Description *string          `json:"description,omitempty"`
	ID          string           `json:"id"`
	Name        string           `json:"name"`
}

// Fills the method from its JSON object, which must carry id and name
func (m *AuthMethodAgent) UnmarshalJSON(data []byte) error {
	type plain AuthMethodAgent
	return decodeObject(data, (*plain)(m), "AuthMethodAgent", "id", "name")
}

// Sign-in through environment variables the client sets before starting the agent
type EnvVarAuthMethod struct {
	Meta        *jsonutil.Object `json:"_meta,omitempty"`
	Description *string          `json:"description,omitempty"`
	ID          string           `json:"id"`
	Link        *string          `json:"link,omitempty"`
	Name        string           `json:"name"`
	Vars        []AuthEnvVar     `json:"vars"`
}

// JSON object of the method with type "env_var"
func (m EnvVarAuthMethod) MarshalJSON() ([]byte, error) {
	type plain EnvVarAuthMethod
	return withTag(plain(m), "type", "env_var")
}

// Fills the method from its JSON object, which must carry id, name and vars
func (m *EnvVarAuthMethod) UnmarshalJSON(data []byte) error {
	type plain EnvVarAuthMethod
	return decodeObject(data, (*plain)(m), "EnvVarAuthMethod", "id", "name", "vars")
}

// Environment variable an env var sign-in sets
type AuthEnvVar struct {
	Meta     *jsonutil.Object `json:"_meta,omitempty"`
	Label    *string          `json:"label,omitempty"`
	Name     string           `json:"name"`
	Optional *bool            `json:"optional,omitempty"`
	Secret   *bool            `json:"secret,omitempty"`
}

// Fills the variable from its JSON object, which must carry name
func (v *AuthEnvVar) UnmarshalJSON(data []byte) error {
	type plain AuthEnvVar
	return decodeObject(data, (*plain)(v), "AuthEnvVar", "name")
}

// Sign-in through the agent's own command run in a terminal with extra arguments and environment
type TerminalAuthMethod struct {
	Meta        *jsonutil.Object `json:"_meta,omitempty"`
	Args        []string         `json:"args,omitzero"`
	Description *string          `json:"description,omitempty"`
	Env         *jsonutil.Object `json:"env,omitempty"`
	ID          string           `json:"id"`
	Name        string           `json:"name"`
}

// JSON object of the method with type "terminal"
func (m TerminalAuthMethod) MarshalJSON() ([]byte, error) {
	type plain TerminalAuthMethod
	return withTag(plain(m), "type", "terminal")
}

// Fills the method from its JSON object, which must carry id and name
func (m *TerminalAuthMethod) UnmarshalJSON(data []byte) error {
	type plain TerminalAuthMethod
	return decodeObject(data, (*plain)(m), "TerminalAuthMethod", "id", "name")
}

// Auth method of a JSON object: env_var and terminal by their type, anything else the agent's own sign-in
func decodeAuthMethod(data []byte) (AuthMethod, error) {
	var method AuthMethod
	switch kind, _ := tagOf(data, "type"); kind {
	case "env_var":
		method = new(EnvVarAuthMethod)
	case "terminal":
		method = new(TerminalAuthMethod)
	default:
		method = new(AuthMethodAgent)
	}
	if err := json.Unmarshal(data, method); err != nil {
		return nil, err
	}
	return method, nil
}

// Params of authenticate: the auth method to run
type AuthenticateRequest struct {
	Meta     *jsonutil.Object `json:"_meta,omitempty"`
	MethodID string           `json:"methodId"`
}

// Fills the request from its JSON object, which must carry methodId
func (r *AuthenticateRequest) UnmarshalJSON(data []byte) error {
	type plain AuthenticateRequest
	return decodeObject(data, (*plain)(r), "AuthenticateRequest", "methodId")
}

// Result of authenticate
type AuthenticateResponse struct {
	Meta *jsonutil.Object `json:"_meta,omitempty"`
}
