// Harness cards, each a deck card fed by bridge messages and reading live state through a shared environment
package cards

import (
	"context"
	"time"

	"github.com/nickheyer/icebath/pkg/agent/hooks"
	"github.com/nickheyer/icebath/pkg/facts"
	"github.com/nickheyer/icebath/pkg/harness"
	"github.com/nickheyer/icebath/pkg/mission"
	"github.com/nickheyer/icebath/pkg/policy"
	"github.com/nickheyer/icebath/pkg/policy/mode"
	"github.com/nickheyer/icebath/pkg/policy/rules"
	"github.com/nickheyer/icebath/pkg/profiles"
	"github.com/nickheyer/icebath/pkg/protocol"
	"github.com/nickheyer/icebath/pkg/session"
	"github.com/nickheyer/icebath/pkg/tool"
	"github.com/nickheyer/icebath/pkg/tool/files"
	"github.com/nickheyer/icebath/pkg/tool/shell"
	"github.com/nickheyer/icebath/pkg/ui/theme"
)

// Live state every card may read, nil members standing for parts the app did not build
type Env struct {
	Theme    theme.Theme
	Control  *harness.Controller
	Meter    *harness.Meter
	Work     *harness.Workspace
	Facts    *facts.Store
	Missions *mission.Store
	Approver *harness.Approver
	PreImage *harness.PreImage
	MCP      *harness.MCP
	Shell    *shell.Shell
	Tools    *tool.Registry
	Rules    *rules.Policy
	Mode     *mode.Switch
	Store    session.Store
	// Resolves file references the way the file tools do, against their directory and root
	Files files.Options
	// Session the live transcript follows
	Session func() *session.Session
	// Profiles known to the app, the primary first
	Profiles func() []profiles.Profile
	// Endpoint currently answering
	Primary func() harness.Endpoint
	// Lists models of a profile, hitting the endpoint
	Models func(ctx context.Context, p profiles.Profile) ([]profiles.Model, error)
	// Hooks configured for the app
	Hooks func() hooks.Config
	// Knob values as the app holds them, keyed by knob name
	Knobs func() map[string]string
	// Directory the config files live in
	ConfigDir string
	// Directory rules and drafts are written to
	ProjectDir string
	Now        func() time.Time
}

func (e *Env) now() time.Time {
	if e.Now != nil {
		return e.Now()
	}
	return time.Now()
}

// Input the composer sends to the agent, steering a run in progress when one is going
type SubmitMsg struct {
	Parts []protocol.Part
	Text  string
}

// Slash command the composer parsed
type CommandMsg struct {
	Name string
	Args string
}

// Answer a person gave to a pending approval
type AnswerMsg struct {
	ID      int
	Verdict policy.Verdict
}

// Request to open the transcript of a run
type OpenRunMsg struct {
	Run string
}

// Request to stop a run
type KillRunMsg struct {
	Run string
}

// Request to stop a background job
type KillJobMsg struct {
	Job string
}

// Request to resume a stored session
type ResumeMsg struct {
	Session string
}

// Request to fork a stored session
type ForkMsg struct {
	Session string
}

// Request to delete a stored session
type DeleteMsg struct {
	Session string
}

// Request to switch the model
type SetModelMsg struct {
	Model string
}

// Request to switch the permission mode
type SetModeMsg struct {
	Mode mode.Mode
}

// Request to switch the profile
type SetProfileMsg struct {
	Profile string
	Model   string
}

// Rules as edited, to be applied and saved
type RulesMsg struct {
	Set rules.Set
}

// Request to reconnect an MCP server
type ReconnectMsg struct {
	Server string
}

// Request to fork the live session at an entry count
type ForkAtMsg struct {
	Entries int
}

// Outcome of an export to disk
type ExportMsg struct {
	Path string
	Err  error
}

// Knob changed in the configuration card
type KnobMsg struct {
	Key   string
	Value string
}

// Request to run a palette command by name
type PaletteMsg struct {
	Command string
}

// Request to start a mission from an objective
type MissionMsg struct {
	Objective string
}

// Request to finish or resume a mission
type MissionStateMsg struct {
	ID     string
	Status string
}

// Request to forget an ended mission
type MissionRemoveMsg struct {
	ID string
}
