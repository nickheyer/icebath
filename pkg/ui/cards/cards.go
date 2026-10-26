// Harness cards, each a window manager card fed by bridge messages and reading live state through a shared environment
package cards

import (
	"context"
	"strings"
	"time"

	"charm.land/lipgloss/v2"

	"github.com/nickheyer/icebath/pkg/harness"
	"github.com/nickheyer/icebath/pkg/policy"
	"github.com/nickheyer/icebath/pkg/policy/mode"
	"github.com/nickheyer/icebath/pkg/profiles"
	"github.com/nickheyer/icebath/pkg/protocol"
	"github.com/nickheyer/icebath/pkg/session"
	"github.com/nickheyer/icebath/pkg/tool"
	"github.com/nickheyer/icebath/pkg/tool/files"
	"github.com/nickheyer/icebath/pkg/tool/shell"
	"github.com/nickheyer/icebath/pkg/ui/keys"
	"github.com/nickheyer/icebath/pkg/ui/render"
	"github.com/nickheyer/icebath/pkg/ui/theme"
)

// Live state every card may read, nil members standing for parts the app did not build
type Env struct {
	Theme    theme.Theme
	Keys     *keys.Map
	Control  *harness.Controller
	Meter    *harness.Meter
	Work     *harness.Workspace
	Approver *harness.Approver
	PreImage *harness.PreImage
	MCP      *harness.MCP
	Shell    *shell.Shell
	Tools    *tool.Registry
	Mode     *mode.Switch
	Store    session.Store
	Journal  *harness.Journal
	// Writes profiles, rules, hooks, servers and tool switches to the files holding them
	Editor Editor
	// Lifetime of the app, the context servers connect under
	Ctx context.Context
	// Resolves file references the way the file tools do, against their directory and root
	Files files.Options
	// Sessions open in a workspace, in open order
	Lives func() []*harness.Live
	// Endpoint currently answering
	Primary func() harness.Endpoint
	// Lists models of a profile, hitting the endpoint
	Models func(ctx context.Context, p profiles.Profile) ([]profiles.Model, error)
	// Setting values as the app holds them keyed by name, empty while unset
	Settings func() map[string]string
	// Base prompt every session starts from
	DefaultBase func() string
	// Environment block the client adds for a run on the model
	Environment func(model string) string
	// Directory exports and the shell fallback use
	ProjectDir string
	Now        func() time.Time
}

// Configuration the editor tabs read and write, each change saved to the file holding the entry
type Editor interface {
	Instances() []harness.Instance
	Shadowed(name string) bool
	SaveProfile(old string, p profiles.Profile) error
	RemoveProfile(name string) error
	MoveProfile(name string, delta int) error
	SetProfileEnabled(name string, on bool) error
	RuleEntries() ([]harness.RuleEntry, error)
	AddRule(scope harness.Scope, d policy.Decision, rule string) error
	ReplaceRule(old harness.RuleEntry, d policy.Decision, rule string) error
	RemoveRule(e harness.RuleEntry) error
	HookEntries() ([]harness.HookEntry, error)
	AddHook(scope harness.Scope, e harness.HookEntry) error
	ReplaceHook(old, e harness.HookEntry) error
	RemoveHook(e harness.HookEntry) error
	ServerEntries() ([]harness.ServerEntry, error)
	SaveServer(scope harness.Scope, old string, cfg harness.ServerConfig) error
	RemoveServer(name string) error
	SetServerEnabled(name string, on bool) (harness.ServerConfig, error)
	RestartServer(ctx context.Context, old string, cfg harness.ServerConfig) error
	CloseServer(name string) error
	SetToolEnabled(name string, on bool) error
	ScopeOf(path string) string
}

// First key of the action as a legend shows it
func (e *Env) key(id string) string { return keys.Format(e.Keys.First(id)) }

// Text led by the first key of the action, the text alone while the action is unbound
func (e *Env) keyed(id, text string) string {
	if k := e.key(id); k != "" {
		return k + " " + text
	}
	return text
}

func (e *Env) now() time.Time {
	if e.Now != nil {
		return e.Now()
	}
	return time.Now()
}

// Input a session card sends to its agent, steering a run in progress when one is going
type SubmitMsg struct {
	Session string
	Parts   []protocol.Part
	Text    string
}

// Session card the manager closed, its session to be stored and released
type SessionClosedMsg struct {
	Session string
}

// Slash command a session composer parsed
type CommandMsg struct {
	Session string
	Name    string
	Args    string
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

// Request to resume stored sessions, each in its own workspace
type OpenSessionMsg struct {
	Sessions []string
}

// Request to fork stored sessions, each into a new workspace
type ForkMsg struct {
	Sessions []string
}

// Request to delete stored sessions
type DeleteMsg struct {
	Sessions []string
}

// Request to switch the model
type SetModelMsg struct {
	Model string
}

// Request to apply new tuning to a session
type TuneMsg struct {
	Session string
	Tuning  harness.Tuning
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

// Request to reconnect an MCP server
type ReconnectMsg struct {
	Server string
}

// Outcome of an export to disk
type ExportMsg struct {
	Path string
	Err  error
}

// Setting changed in the settings tab, an empty value unsetting it
type SettingMsg struct {
	Key   string
	Value string
	// Applies without a toast unless it fails
	Quiet bool
}

// Request to finish or resume a mission
type MissionStateMsg struct {
	Session string
	ID      string
	Status  string
}

// Request to forget an ended mission
type MissionRemoveMsg struct {
	Session string
	ID      string
}

// Row a list movement action leaves the cursor on, among n rows with page rows in view
func listMove(id string, sel, n, page int) int {
	last := max(n-1, 0)
	switch id {
	case "list.down":
		return min(sel+1, last)
	case "list.up":
		return max(sel-1, 0)
	case "list.top":
		return 0
	case "list.bottom":
		return last
	case "list.page.down":
		return min(sel+max(page, 1), last)
	case "list.page.up":
		return max(sel-max(page, 1), 0)
	}
	return sel
}

// Table lines cut to room rows keeping the header and the cursor row, with the first row index
func window(table []string, sel, room int) ([]string, int) {
	if len(table) <= room || room <= render.TableHead {
		return table, 0
	}
	body := room - render.TableHead
	start := 0
	if sel >= body {
		start = sel - body + 1
	}
	end := min(len(table), render.TableHead+start+body)
	return append(append([]string(nil), table[:render.TableHead]...), table[render.TableHead+start:end]...), start
}

// Dim text under the brand mark centered in w by h, for a card with nothing to show
func emptyView(w, h int, text string, t theme.Theme) string {
	body := render.Dim(text, t)
	if h >= 3 {
		body = lipgloss.NewStyle().Foreground(t.Steel).Render(t.Glyphs.Brand) + "\n" + body
	}
	return lipgloss.Place(w, max(h, 1), lipgloss.Center, lipgloss.Center, body)
}

// Lines cut or padded to h rows with the foot on the last row, as they are when there is no foot
func footed(lines []string, foot string, h int) string {
	if foot == "" {
		return strings.Join(lines[:min(len(lines), max(h, 0))], "\n")
	}
	body := lines[:min(len(lines), max(h-1, 0))]
	for len(body) < h-1 {
		body = append(body, "")
	}
	return strings.Join(append(body, foot), "\n")
}
