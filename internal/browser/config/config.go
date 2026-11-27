package config

const (
	Key = "browser"
	// Where pages open: local, or the cloud browser service of the name
	CloudProvider = Key + ".cloud_provider"
	// Local and private network addresses the browser may open, and whether a cloud browser leaves those to the local one
	PrivateURLs     = Key + ".allow_private_urls"
	LocalForPrivate = Key + ".auto_local_for_private_urls"
	// Running browser to attach to, in place of starting one
	CDPURL = Key + ".cdp_url"
	// Flags added to every Chromium launch
	ChromiumArgs = Key + ".chromium_args"
	// Seconds to wait for the browser to start and answer, never under five
	CommandTimeout = Key + ".command_timeout"
	// What a page's alert, confirm and prompt get, and the seconds one waits for the agent
	DialogPolicy  = Key + ".dialog_policy"
	DialogTimeout = Key + ".dialog_timeout_s"
	// Local engine: Chromium, or Lightpanda
	Engine = Key + ".engine"
	// One folder browser code keeps its files in for every task
	ExecWorkspace = Key + ".exec_workspace"
	// Browser program in place of the bundled Chromium
	ExecutablePath = Key + ".executable_path"
	// Shows the browser window
	Headed = Key + ".headed"
	// Seconds an unused session is kept, never under thirty
	InactivityTimeout = Key + ".inactivity_timeout"
	// The user's own browser profile: whether the agent may use a copy of it, which one, and whether the browser holding it is closed
	UseRealProfile       = Key + ".use_real_profile"
	RealProfilePin       = Key + ".real_profile_pin"
	RealProfileAutoclose = Key + ".real_profile_autoclose"
	// Marks the tab the agent drives
	TabMarker = Key + ".tab_marker"
	// Longest page snapshot a browser tool returns, the rest stored aside, and whether page scripts that reach sensitive state are refused
	SnapshotThreshold   = Key + ".snapshot_threshold"
	RestrictEvaluate    = Key + ".restrict_evaluate"
	AllowUnsafeEvaluate = Key + ".allow_unsafe_evaluate"
	// One browser profile kept for every session
	UserDataDir = Key + ".user_data_dir"
)

// Values of browser.cloud_provider that keep pages on this machine
const Local = "local"

// Engines
const (
	Chromium   = "auto"
	Lightpanda = "lightpanda"
	Chrome     = "chrome"
)

// Dialog policies
const (
	MustRespond = "must_respond"
	AutoDismiss = "auto_dismiss"
	AutoAccept  = "auto_accept"
)

// Environment variables of the settings
const (
	CDPURLEnv            = "BROWSER_CDP_URL"
	ExecWorkspaceEnv     = "BH_AGENT_WORKSPACE"
	InactivityTimeoutEnv = "BROWSER_INACTIVITY_TIMEOUT"
	TabMarkerEnv         = "BH_TAB_MARKER"
	// Longest a cloud browser session runs, for every service that takes a limit
	SessionTimeoutEnv = "BROWSER_SESSION_TIMEOUT"
)
