package acputil

// Protocol version this package speaks
const ProtocolVersion = 1

// Methods an agent serves
const (
	// Request opening the connection: protocol version and capabilities
	MethodInitialize = "initialize"
	// Request running one of the auth methods initialize advertised
	MethodAuthenticate = "authenticate"
	// Request creating a session
	MethodSessionNew = "session/new"
	// Request loading a session and replaying its history
	MethodSessionLoad = "session/load"
	// Request listing sessions
	MethodSessionList = "session/list"
	// Request closing a session, part of the unstable protocol
	MethodSessionClose = "session/close"
	// Request forking a session, part of the unstable protocol
	MethodSessionFork = "session/fork"
	// Request resuming a session without replaying its history, part of the unstable protocol
	MethodSessionResume = "session/resume"
	// Request running one prompt turn
	MethodSessionPrompt = "session/prompt"
	// Notification cancelling the running prompt turn
	MethodSessionCancel = "session/cancel"
	// Request switching the session mode
	MethodSessionSetMode = "session/set_mode"
	// Request switching the session model, part of the unstable protocol
	MethodSessionSetModel = "session/set_model"
	// Request setting a session config option
	MethodSessionSetConfigOption = "session/set_config_option"
)

// Methods a client serves
const (
	// Notification reporting a session update
	MethodSessionUpdate = "session/update"
	// Request asking the user to permit a tool call
	MethodSessionRequestPermission = "session/request_permission"
	// Request reading a text file through the client
	MethodFSReadTextFile = "fs/read_text_file"
	// Request writing a text file through the client
	MethodFSWriteTextFile = "fs/write_text_file"
	// Request starting a command in a client terminal
	MethodTerminalCreate = "terminal/create"
	// Request reading the output of a client terminal
	MethodTerminalOutput = "terminal/output"
	// Request releasing a client terminal
	MethodTerminalRelease = "terminal/release"
	// Request waiting for the command of a client terminal to exit
	MethodTerminalWaitForExit = "terminal/wait_for_exit"
	// Request killing the command of a client terminal
	MethodTerminalKill = "terminal/kill"
)
