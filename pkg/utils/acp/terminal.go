package acputil

import jsonutil "github.com/nickheyer/icebath/pkg/utils/json"

// Params of terminal/create: the command to run in a client terminal
type CreateTerminalRequest struct {
	Meta    *jsonutil.Object `json:"_meta,omitempty"`
	Args    []string         `json:"args,omitzero"`
	Command string           `json:"command"`
	Cwd     *string          `json:"cwd,omitempty"`
	Env     []EnvVariable    `json:"env,omitzero"`
	// Most output bytes the client keeps, the oldest dropped first
	OutputByteLimit *int   `json:"outputByteLimit,omitempty"`
	SessionID       string `json:"sessionId"`
}

// Fills the request from its JSON object, which must carry command and sessionId
func (r *CreateTerminalRequest) UnmarshalJSON(data []byte) error {
	type plain CreateTerminalRequest
	return decodeObject(data, (*plain)(r), "CreateTerminalRequest", "command", "sessionId")
}

// Result of terminal/create
type CreateTerminalResponse struct {
	Meta       *jsonutil.Object `json:"_meta,omitempty"`
	TerminalID string           `json:"terminalId"`
}

// Fills the response from its JSON object, which must carry terminalId
func (r *CreateTerminalResponse) UnmarshalJSON(data []byte) error {
	type plain CreateTerminalResponse
	return decodeObject(data, (*plain)(r), "CreateTerminalResponse", "terminalId")
}

// Params of terminal/output
type TerminalOutputRequest struct {
	Meta       *jsonutil.Object `json:"_meta,omitempty"`
	SessionID  string           `json:"sessionId"`
	TerminalID string           `json:"terminalId"`
}

// Fills the request from its JSON object, which must carry sessionId and terminalId
func (r *TerminalOutputRequest) UnmarshalJSON(data []byte) error {
	type plain TerminalOutputRequest
	return decodeObject(data, (*plain)(r), "TerminalOutputRequest", "sessionId", "terminalId")
}

// Result of terminal/output: the output so far and, once the command ended, how it exited
type TerminalOutputResponse struct {
	Meta       *jsonutil.Object    `json:"_meta,omitempty"`
	ExitStatus *TerminalExitStatus `json:"exitStatus,omitempty"`
	Output     string              `json:"output"`
	Truncated  bool                `json:"truncated"`
}

// Fills the response from its JSON object, which must carry output and truncated
func (r *TerminalOutputResponse) UnmarshalJSON(data []byte) error {
	type plain TerminalOutputResponse
	return decodeObject(data, (*plain)(r), "TerminalOutputResponse", "output", "truncated")
}

// Exit code or signal a terminal command ended with
type TerminalExitStatus struct {
	Meta     *jsonutil.Object `json:"_meta,omitempty"`
	ExitCode *int             `json:"exitCode,omitempty"`
	Signal   *string          `json:"signal,omitempty"`
}

// Params of terminal/release
type ReleaseTerminalRequest struct {
	Meta       *jsonutil.Object `json:"_meta,omitempty"`
	SessionID  string           `json:"sessionId"`
	TerminalID string           `json:"terminalId"`
}

// Fills the request from its JSON object, which must carry sessionId and terminalId
func (r *ReleaseTerminalRequest) UnmarshalJSON(data []byte) error {
	type plain ReleaseTerminalRequest
	return decodeObject(data, (*plain)(r), "ReleaseTerminalRequest", "sessionId", "terminalId")
}

// Result of terminal/release
type ReleaseTerminalResponse struct {
	Meta *jsonutil.Object `json:"_meta,omitempty"`
}

// Params of terminal/wait_for_exit
type WaitForTerminalExitRequest struct {
	Meta       *jsonutil.Object `json:"_meta,omitempty"`
	SessionID  string           `json:"sessionId"`
	TerminalID string           `json:"terminalId"`
}

// Fills the request from its JSON object, which must carry sessionId and terminalId
func (r *WaitForTerminalExitRequest) UnmarshalJSON(data []byte) error {
	type plain WaitForTerminalExitRequest
	return decodeObject(data, (*plain)(r), "WaitForTerminalExitRequest", "sessionId", "terminalId")
}

// Result of terminal/wait_for_exit: the exit code or signal the command ended with
type WaitForTerminalExitResponse struct {
	Meta     *jsonutil.Object `json:"_meta,omitempty"`
	ExitCode *int             `json:"exitCode,omitempty"`
	Signal   *string          `json:"signal,omitempty"`
}

// Params of terminal/kill
type KillTerminalRequest struct {
	Meta       *jsonutil.Object `json:"_meta,omitempty"`
	SessionID  string           `json:"sessionId"`
	TerminalID string           `json:"terminalId"`
}

// Fills the request from its JSON object, which must carry sessionId and terminalId
func (r *KillTerminalRequest) UnmarshalJSON(data []byte) error {
	type plain KillTerminalRequest
	return decodeObject(data, (*plain)(r), "KillTerminalRequest", "sessionId", "terminalId")
}

// Result of terminal/kill
type KillTerminalResponse struct {
	Meta *jsonutil.Object `json:"_meta,omitempty"`
}
