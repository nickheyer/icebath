package config

import (
	"time"

	"github.com/nickheyer/icebath/internal/config"
)

// Registers the settings of the browser, the cloud services offered as given
func Register(cfg *config.Config, services func() []config.Option) {
	cfg.Register(config.Sections{{
		Key:   Key,
		Title: "Browser",
		About: "The browser the agent drives: a local Chromium or Lightpanda, the user's own browser, or a cloud browser service",
		Settings: []config.Setting{
			{Key: CloudProvider, Title: "Browser service", About: "Where web pages open: local, or a cloud browser service. Left unset, a Browser Use or " +
				"Browserbase key picks that service", Default: Local, Options: func() []config.Option {
				return append([]config.Option{{Value: Local, Title: "Local browser", About: "Chromium or Lightpanda on this machine."}}, services()...)
			}},
			{Key: PrivateURLs, Title: "Allow private addresses", About: "Let the browser open local and private network addresses", Default: false},
			{Key: LocalForPrivate, Title: "Local for private", About: "Use the local browser for private addresses instead of a cloud one", Default: true},
			{Key: CDPURL, Title: "Browser attach address", About: "Attach to an already running browser at this address; a live /browser connect wins " +
				"over this", Default: "", Env: []string{CDPURLEnv}},
			{Key: ChromiumArgs, Title: "Extra browser flags", About: "Extra command line flags added to every Chromium launch, one per item or per line", Default: []string{}},
			{Key: CommandTimeout, Title: "Browser start timeout", About: "Seconds to wait for the browser to start and answer; never less than 5", Default: 30 * time.Second,
				Min: config.Bound(5 * time.Second)},
			{Key: DialogPolicy, Title: "Page dialogs", About: "How the browser handles alert, confirm and prompt pop-ups", Default: MustRespond, Options: config.Options(
				config.Option{Value: MustRespond, Title: "Ask the agent", About: "Agent must answer each dialog."},
				config.Option{Value: AutoDismiss, Title: "Dismiss", About: "Close dialogs without accepting."},
				config.Option{Value: AutoAccept, Title: "Accept", About: "Accept every dialog."},
			)},
			{Key: DialogTimeout, Title: "Dialog wait", About: "Seconds a pop-up waits for an answer before it is dismissed; zero or less uses the default " +
				"of 300", Default: 5 * time.Minute},
			{Key: Engine, Title: "Browser engine", About: "Which local browser engine the tools drive", Default: Chromium, Options: config.Options(
				config.Option{Value: Chromium, Title: "Chromium", About: "Standard Chromium browser."},
				config.Option{Value: Lightpanda, Title: "Lightpanda", About: "Lightweight headless engine."},
				config.Option{Value: Chrome, Title: "Chrome", About: "Same as Chromium."},
			)},
			{Key: ExecWorkspace, Title: "Browser code folder", About: "One folder browser code keeps its files in for every task. Empty gives each task its own " +
				"folder in the icebath cache; a relative path is placed under the profile folder", Default: "", Env: []string{ExecWorkspaceEnv}},
			{Key: ExecutablePath, Title: "Browser program", About: "Use this browser program instead of the bundled Chromium", Default: ""},
			{Key: Headed, Title: "Show browser window", About: "Show the browser window instead of running it hidden", Default: false},
			{Key: InactivityTimeout, Title: "Browser idle close", About: "Seconds before an unused browser session is closed; never less than 30", Default: 2 * time.Minute,
				Min: config.Bound(30 * time.Second), Env: []string{InactivityTimeoutEnv}},
			{Key: RealProfileAutoclose, Title: "Close real browser", About: "Close your own browser by itself when the agent needs its profile; only matters " +
				"when using your real browser profile is allowed", Default: false},
			{Key: RealProfilePin, Title: "Real profile name", About: "Folder name of the browser profile to borrow, such as Profile 2. Empty uses the profile " +
				"you used last; a name that does not exist stops the borrow", Default: ""},
			{Key: SnapshotThreshold, Title: "Snapshot size", About: "Characters of a page snapshot a browser tool returns before the rest is stored in a " +
				"file the agent can page through; never less than 1000", Default: 15000},
			{Key: RestrictEvaluate, Title: "Restrict page scripts", About: "Refuse page scripts that read cookies, storage or form values or make network " +
				"requests", Default: false},
			{Key: AllowUnsafeEvaluate, Title: "Allow unsafe page scripts", About: "Let page scripts reach sensitive state even with restricted page scripts on",
				Default: false},
			{Key: TabMarker, Title: "Mark agent tab", About: "Mark the tab the agent drives so you can tell it apart", Default: true, Env: []string{TabMarkerEnv}},
			{Key: UseRealProfile, Title: "Use real profile", About: "Allow the agent to use a copy of your own browser profile and logins", Default: false},
			{Key: UserDataDir, Title: "Browser profile folder", About: "Keep one browser profile in this folder for every session; a relative path is placed " +
				"under the profile folder", Default: ""},
		},
	}})
}
