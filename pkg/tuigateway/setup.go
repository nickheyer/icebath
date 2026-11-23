package tuigateway

// Side effect a setup form starts on the background service
type SetupAction string

func (SetupAction) Values() []string {
	return []string{"xai.oauth.login", "tts.install.neutts", "tts.install.kittentts", "terminal.ssh.test", "terminal.backend.plugin"}
}

// Action to start and the words it takes, such as the server a connection test reaches
type SetupActionStartParams struct {
	Action SetupAction       `json:"action"`
	Params map[string]string `json:"params,omitzero"`
}

// Job that follows the action
type SetupActionStartResult struct {
	Job string `json:"job"`
}

// Job to follow, the lines already shown and the line redrawn in place last shown
type SetupActionPollParams struct {
	Job   string  `json:"job"`
	After int64   `json:"after"`
	Live  *string `json:"live,omitzero"`
}

// Output past after, the line redrawn in place, and the outcome once done
type SetupActionPollResult struct {
	Lines []string `json:"lines"`
	Live  string   `json:"live"`
	Done  bool     `json:"done"`
	Ok    bool     `json:"ok"`
}

var (
	// Starts a side effect of a setup form: a sign in, an engine install or a connection test
	SetupActionStart = NewMethod[SetupActionStartParams, SetupActionStartResult]("setup.action.start")
	// Waits up to two seconds for new output of a setup job or a change of its redrawn line
	SetupActionPoll = NewMethod[SetupActionPollParams, SetupActionPollResult]("setup.action.poll")
)
