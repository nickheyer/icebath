package tuigateway

import "encoding/json"

type SetupFlowStartParams struct {
	Flow   string            `json:"flow"`
	Params map[string]string `json:"params,omitzero"`
}

type SetupFlowStartResult struct {
	Session string `json:"session"`
}

// Answer to the open question, index of a choice, indices of a multi choice, text of a prompt or cancel
type SetupFlowAnswer struct {
	Question int64   `json:"question"`
	Index    *int64  `json:"index,omitzero"`
	Indices  []int64 `json:"indices,omitzero"`
	Text     string  `json:"text,omitzero"`
	Cancel   bool    `json:"cancel,omitzero"`
}

type SetupFlowStepParams struct {
	Session string           `json:"session"`
	Answer  *SetupFlowAnswer `json:"answer,omitzero"`
}

type SetupFlowQuestionKind string

func (SetupFlowQuestionKind) Values() []string { return []string{"choose", "check", "text"} }

type SetupFlowOption struct {
	Title  string `json:"title"`
	Detail string `json:"detail,omitzero"`
}

type SetupFlowItem struct {
	Title  string   `json:"title"`
	Detail string   `json:"detail,omitzero"`
	Tools  []string `json:"tools,omitzero"`
}

// Open question of a setup flow, a choice with its default row, a checklist with its weights or a text prompt
type SetupFlowQuestion struct {
	ID          int64                 `json:"id"`
	Kind        SetupFlowQuestionKind `json:"kind"`
	Question    string                `json:"question"`
	Options     []SetupFlowOption     `json:"options,omitzero"`
	Default     int64                 `json:"default,omitzero"`
	Items       []SetupFlowItem       `json:"items,omitzero"`
	Selected    []int64               `json:"selected,omitzero"`
	Tokens      map[string]int64      `json:"tokens,omitzero"`
	DefaultText string                `json:"default_text,omitzero"`
	Secret      bool                  `json:"secret,omitzero"`
}

type SetupFlowStepResult struct {
	Lines    []string           `json:"lines"`
	Question *SetupFlowQuestion `json:"question"`
	Done     bool               `json:"done"`
	Ok       bool               `json:"ok"`
	Error    string             `json:"error"`
}

type SetupHostedFlow string

func (SetupHostedFlow) Values() []string { return []string{"model", "gateway", "import", "quick", "blank"} }

type SetupHostedStartParams struct {
	Flow  SetupHostedFlow `json:"flow"`
	Entry string          `json:"entry,omitzero"`
}

type SetupHostedStartResult struct {
	Job string `json:"job"`
}

type SetupHostedHeading struct {
	At   int64  `json:"at"`
	Text string `json:"text"`
}

type SetupHostedQuestionKind string

func (SetupHostedQuestionKind) Values() []string {
	return []string{"choice", "checklist", "text", "secret", "command"}
}

// Question of a hosted section, a choice from default, a checklist from picked, a typed line or a terminal command
type SetupHostedQuestion struct {
	ID          int64                   `json:"id"`
	Kind        SetupHostedQuestionKind `json:"kind"`
	Title       string                  `json:"title"`
	Description string                  `json:"description"`
	Rows        []string                `json:"rows"`
	Headings    []SetupHostedHeading    `json:"headings"`
	Default     int64                   `json:"default"`
	Picked      []int64                 `json:"picked"`
	Argv        []string                `json:"argv"`
}

type SetupHostedPollParams struct {
	Job   string `json:"job"`
	After int64  `json:"after"`
}

// Output past after, the waiting question, the redrawn progress line, and once done the model config.yaml holds
type SetupHostedPollResult struct {
	Lines    []string             `json:"lines"`
	Question *SetupHostedQuestion `json:"question"`
	Done     bool                 `json:"done"`
	Ok       bool                 `json:"ok"`
	Model    string               `json:"model"`
	Provider string               `json:"provider"`
	Changed  bool                 `json:"changed"`
	Progress string               `json:"progress"`
}

// Answer to a hosted question, backed out of, a choice index, checklist rows, a typed line or a command outcome
type SetupHostedAnswerParams struct {
	Job       string  `json:"job"`
	Question  int64   `json:"question"`
	Cancelled bool    `json:"cancelled,omitzero"`
	Index     int64   `json:"index,omitzero"`
	Picked    []int64 `json:"picked,omitzero"`
	Text      string  `json:"text,omitzero"`
	Ran       bool    `json:"ran,omitzero"`
	Error     string  `json:"error,omitzero"`
}

type SetupHostedAnswerResult struct{}

type SetupHostedCancelParams struct {
	Job string `json:"job"`
}

type SetupHostedCancelResult struct{}

// Keys, secret keys, facts and named gateway values a setup section starts from
type SetupSectionLoadParams struct {
	ConfigKeys       []string `json:"config_keys,omitzero"`
	EnvKeys          []string `json:"env_keys,omitzero"`
	SecretConfigKeys []string `json:"secret_config_keys,omitzero"`
	SecretEnvKeys    []string `json:"secret_env_keys,omitzero"`
	Facts            []string `json:"facts,omitzero"`
	Values           []string `json:"values,omitzero"`
}

// Held config and env values, the secret keys holding a value, each fact and each named value by the name asked
type SetupSectionLoadResult struct {
	Config  map[string]any             `json:"config"`
	Env     map[string]string          `json:"env"`
	Secrets []string                   `json:"secrets"`
	Facts   map[string]bool            `json:"facts"`
	Values  map[string]json.RawMessage `json:"values"`
}

// Terminal backend a plugin registers, listed by the terminal.plugin_backends value
type SetupTerminalBackend struct {
	Name        string `json:"name"`
	DisplayName string `json:"display_name"`
	Description string `json:"description"`
}

// Container runtime found on the gateway host, the terminal.docker value
type SetupDockerRuntime struct {
	Path    string `json:"path"`
	Runtime string `json:"runtime"`
}

// Login name and default key file on the gateway host, the terminal.ssh_defaults value
type SetupSSHDefaults struct {
	User string `json:"user"`
	Key  string `json:"key"`
}

// Project and team ids of the nearest linked Vercel project, the terminal.vercel_linked value
type SetupVercelLink struct {
	ProjectID string `json:"projectId,omitzero"`
	OrgID     string `json:"orgId,omitzero"`
}

// Named value of a load decoded into its type, false when absent or of another shape
func SetupValue[T any](values map[string]json.RawMessage, name string) (T, bool) {
	var out T
	raw, held := values[name]
	if !held || Loose(raw, &out) != nil {
		return out, false
	}
	return out, true
}

type SetupTarget string

func (SetupTarget) Values() []string { return []string{"config", "env"} }

type SetupWriteKind string

func (SetupWriteKind) Values() []string { return []string{"bool", "whole", "decimal", "text", "held"} }

// One answer for a dotted config key or an env key, value a flag, whole number, decimal or text as kind names
type SetupSectionWrite struct {
	Target SetupTarget    `json:"target"`
	Key    string         `json:"key"`
	Kind   SetupWriteKind `json:"kind"`
	Value  any            `json:"value"`
}

type SetupSectionRemoval struct {
	Target SetupTarget `json:"target"`
	Key    string      `json:"key"`
}

type SetupSectionSaveParams struct {
	Writes   []SetupSectionWrite   `json:"writes,omitzero"`
	Removals []SetupSectionRemoval `json:"removals,omitzero"`
	FullSave bool                  `json:"full_save,omitzero"`
}

// What config.yaml holds for each written key, the env keys saved and the keys of either file removed
type SetupSectionSaveResult struct {
	Config  map[string]any `json:"config"`
	Env     []string       `json:"env"`
	Removed []string       `json:"removed"`
}

type SetupAction string

func (SetupAction) Values() []string {
	return []string{"xai.oauth.login", "tts.install.neutts", "tts.install.kittentts", "terminal.ssh.test", "terminal.backend.plugin"}
}

type SetupActionStartParams struct {
	Action SetupAction       `json:"action"`
	Params map[string]string `json:"params,omitzero"`
}

type SetupActionStartResult struct {
	Job string `json:"job"`
}

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

// Starts a setup flow on the gateway, one at a time, and returns the session that follows it
var SetupFlowStart = NewMethod[SetupFlowStartParams, SetupFlowStartResult]("setup.flow.start")

// Answers the open question of a setup flow and waits up to two seconds for its next output, question or end
var SetupFlowStep = NewMethod[SetupFlowStepParams, SetupFlowStepResult]("setup.flow.step")

// Runs a setup section on the gateway with its questions put to the terminal UI
var SetupHostedStart = NewMethod[SetupHostedStartParams, SetupHostedStartResult]("setup.hosted.start")

// Waits up to two seconds for output, a question or the end of a hosted setup section
var SetupHostedPoll = NewMethod[SetupHostedPollParams, SetupHostedPollResult]("setup.hosted.poll")

// Answers the question a hosted setup section waits on
var SetupHostedAnswer = NewMethod[SetupHostedAnswerParams, SetupHostedAnswerResult]("setup.hosted.answer")

// Stops a hosted setup section, its open question backed out of and its sign in waits ended
var SetupHostedCancel = NewMethod[SetupHostedCancelParams, SetupHostedCancelResult]("setup.hosted.cancel")

// Reads the values a setup section starts from
var SetupSectionLoad = NewMethod[SetupSectionLoadParams, SetupSectionLoadResult]("setup.section.load")

// Writes the answers of one setup section to config.yaml and .env, a full save rewriting config.yaml after a backup
var SetupSectionSave = NewMethod[SetupSectionSaveParams, SetupSectionSaveResult]("setup.section.save")

// Starts a side effect of a setup section, a login, an engine install or a connection test
var SetupActionStart = NewMethod[SetupActionStartParams, SetupActionStartResult]("setup.action.start")

// Waits up to two seconds for new output of a setup job or a change of its redrawn line
var SetupActionPoll = NewMethod[SetupActionPollParams, SetupActionPollResult]("setup.action.poll")
