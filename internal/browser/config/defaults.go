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
		About: "The browser the agent uses to open web pages",
		Group: config.GroupAbilities,
		Order: 40,
		Settings: []config.Setting{
			{Key: CloudProvider, Title: "Where pages open", About: "Where the agent's web pages open: on this computer or in a cloud browser.",
				Help: "Until you choose, an API key for Browser Use or Browserbase opens pages in that service.", Default: Local,
				Options: func() []config.Option {
					return append([]config.Option{{Value: Local, Title: "Local browser", About: "A browser on this computer"}}, services()...)
				}},
			{Key: Engine, Title: "Engine", About: "Which engine the local browser runs on.", Default: Chromium, Options: config.Options(
				config.Option{Value: Chromium, Title: "Chromium", About: "The standard browser, which most sites expect"},
				config.Option{Value: Lightpanda, Title: "Lightpanda", About: "A lighter browser with no window, quick to start"},
				config.Option{Value: Chrome, Title: "Chrome", About: "The same as Chromium"},
			)},
			{Key: Headed, Title: "Show window", About: "Show the browser window instead of running the browser hidden.", Default: false},
			{Key: UseRealProfile, Title: "Use your profile", About: "Let the agent use a copy of your own browser profile and sign-ins.", Default: false},
			{Key: RealProfileAutoclose, Title: "Close your browser", About: "Let the agent offer to close your browser when your profile is locked.",
				Help: "Closing the browser loses unsaved tabs. Only matters while Use your profile is on.", Default: false},
			{Key: TabMarker, Title: "Mark agent tab", About: "Mark the tab that the agent uses so you can tell the tab apart.", Default: true,
				Env: []string{TabMarkerEnv}},
			{Key: DialogPolicy, Title: "Page pop-ups", About: "What happens when a page shows an alert, confirm or prompt pop-up.", Default: MustRespond,
				Options: config.Options(
					config.Option{Value: MustRespond, Title: "Ask the agent", About: "The agent answers each pop-up"},
					config.Option{Value: AutoDismiss, Title: "Dismiss", About: "Close each pop-up without accepting it"},
					config.Option{Value: AutoAccept, Title: "Accept", About: "Accept every pop-up"},
				)},
			{Key: PrivateURLs, Title: "Open private addresses", About: "Let the browser open local and private network addresses.", Default: false},
			{Key: LocalForPrivate, Title: "Local browser for private", About: "Open private addresses in the local browser when pages open in a cloud browser.",
				Default: true},
			{Key: RealProfilePin, Title: "Profile to copy", About: "The folder name of your browser profile that the agent copies.",
				Help: "Such as Profile 2. A name that does not exist stops the copy.", Default: "", Zero: "Last one used", Advanced: true},
			{Key: UserDataDir, Title: "Profile folder", About: "The folder that keeps one browser profile for every session.",
				Help: "A relative path is placed in the profile's folder.", Default: "", Zero: "New each session", Advanced: true},
			{Key: CDPURL, Title: "Attach address", About: "The address of a running browser that the agent attaches to instead of starting one.",
				Help: "A browser connected with /browser connect wins over this.", Default: "", Env: []string{CDPURLEnv}, Advanced: true},
			{Key: ExecutablePath, Title: "Program", About: "The browser program that runs instead of the bundled Chromium.", Default: "",
				Zero: "Bundled Chromium", Advanced: true},
			{Key: ChromiumArgs, Title: "Extra flags", About: "Command line flags added to every Chromium start.",
				Help: "One flag per item, or one per line.", Default: []string{}, Advanced: true},
			{Key: CommandTimeout, Title: "Start time limit", About: "How long to wait for the browser to start and answer.", Default: 30 * time.Second,
				Min: config.Bound(5 * time.Second), Advanced: true},
			{Key: InactivityTimeout, Title: "Close when idle", About: "How long an unused browser session stays open.", Default: 2 * time.Minute,
				Min: config.Bound(30 * time.Second), Env: []string{InactivityTimeoutEnv}, Advanced: true},
			{Key: DialogTimeout, Title: "Pop-up wait", About: "How long a pop-up waits for an answer before it is dismissed.", Default: 5 * time.Minute,
				Zero: "5 minutes", Advanced: true},
			{Key: ExecWorkspace, Title: "Code folder", About: "The folder where browser code keeps its files for every task.",
				Help: "A relative path is placed in the profile's folder.", Default: "", Zero: "One per task", Env: []string{ExecWorkspaceEnv}, Advanced: true},
			{Key: SnapshotThreshold, Title: "Snapshot size", About: "How much of a page snapshot a browser tool returns before the rest goes to a file.",
				Help: "The agent can page through that file.", Default: 15000, Counts: "characters", Least: config.Num(1000), Advanced: true},
			{Key: RestrictEvaluate, Title: "Restrict page scripts", About: "Refuse page scripts that read cookies, storage or form values or reach the network.",
				Default: false, Advanced: true},
			{Key: AllowUnsafeEvaluate, Title: "Allow unsafe page scripts", About: "Let page scripts reach cookies, storage and form values even while scripts are restricted.",
				Default: false, Advanced: true},
		},
	}})
}
