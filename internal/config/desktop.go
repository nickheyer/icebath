package config

import "gopkg.in/yaml.v3"

// Headless per-profile screen section
type BotDesktopConfig struct {
	Geometry        string           `yaml:"geometry"`
	AutoStart       bool             `yaml:"auto_start" coerce:"bool"`
	MinFreeMemoryMB int              `yaml:"min_free_memory_mb"`
	IdleStopMinutes float64          `yaml:"idle_stop_minutes"`
	Placement       DesktopPlacement `yaml:"placement"`
	rest            rest
}

type botDesktopFields BotDesktopConfig

// Decodes the mapping and keeps unknown keys
func (b *BotDesktopConfig) UnmarshalYAML(n *yaml.Node) error {
	return decodeMapping(n, (*botDesktopFields)(b), &b.rest)
}

// Writes the mapping with unknown keys after the named ones
func (b BotDesktopConfig) MarshalYAML() (any, error) {
	return encodeMapping(botDesktopFields(b), b.rest)
}

// Where the bot screen runs
type DesktopPlacement string

// Screen placements
const (
	PlacementAuto     DesktopPlacement = "auto"
	PlacementTerminal DesktopPlacement = "terminal"
	PlacementGateway  DesktopPlacement = "gateway"
)

// Accepted screen placements
func DesktopPlacements() []DesktopPlacement {
	return []DesktopPlacement{PlacementAuto, PlacementTerminal, PlacementGateway}
}

// Computer use driver section
type ComputerUseConfig struct {
	CUATelemetry        bool              `yaml:"cua_telemetry" coerce:"bool"`
	Autostart           bool              `yaml:"autostart" coerce:"bool"`
	NativeWayland       bool              `yaml:"native_wayland" coerce:"bool"`
	MaxImageDimension   int               `yaml:"max_image_dimension"`
	CaptureAfterMode    CaptureMode       `yaml:"capture_after_mode"`
	AXMaxElements       int               `yaml:"ax_max_elements"`
	NoOverlay           *bool             `yaml:"no_overlay" coerce:"boolkeep"`
	PermissionMode      CUAPermissionMode `yaml:"permission_mode"`
	CapabilityManifest  string            `yaml:"capability_manifest"`
	AllowUnsignedDriver bool              `yaml:"allow_unsigned_driver" coerce:"exact"`
	rest                rest
}

type computerUseFields ComputerUseConfig

// Decodes the mapping and keeps unknown keys
func (c *ComputerUseConfig) UnmarshalYAML(n *yaml.Node) error {
	return decodeMapping(n, (*computerUseFields)(c), &c.rest)
}

// Writes the mapping with unknown keys after the named ones
func (c ComputerUseConfig) MarshalYAML() (any, error) {
	return encodeMapping(computerUseFields(c), c.rest)
}

// What a capture after an action returns
type CaptureMode string

// Capture modes
const (
	CaptureSOM    CaptureMode = "som"
	CaptureAX     CaptureMode = "ax"
	CaptureVision CaptureMode = "vision"
)

// Accepted capture modes
func CaptureModes() []CaptureMode {
	return []CaptureMode{CaptureSOM, CaptureAX, CaptureVision}
}

// Driver approval boundary, unrestricted never accepted from config
type CUAPermissionMode string

// Driver approval boundaries
const (
	PermissionStandard CUAPermissionMode = "standard"
	PermissionBounded  CUAPermissionMode = "bounded"
)

// Accepted driver approval boundaries
func CUAPermissionModes() []CUAPermissionMode {
	return []CUAPermissionMode{PermissionStandard, PermissionBounded}
}

// Egress credential-injection proxy section
type ProxyConfig struct {
	Enabled           bool             `yaml:"enabled" coerce:"bool"`
	TunnelPort        int              `yaml:"tunnel_port"`
	AutoInstall       bool             `yaml:"auto_install"`
	CredentialSource  CredentialSource `yaml:"credential_source"`
	EnforceOnDocker   bool             `yaml:"enforce_on_docker" coerce:"bool"`
	AllowEnvFallback  bool             `yaml:"allow_env_fallback" coerce:"bool"`
	UpstreamDenyCIDRs *[]string        `yaml:"upstream_deny_cidrs"`
	ExtraAllowedHosts []string         `yaml:"extra_allowed_hosts"`
	rest              rest
}

type proxyFields ProxyConfig

// Decodes the mapping and keeps unknown keys such as a leftover fail_on_uncovered_providers
func (p *ProxyConfig) UnmarshalYAML(n *yaml.Node) error {
	return decodeMapping(n, (*proxyFields)(p), &p.rest)
}

// Writes the mapping with unknown keys after the named ones
func (p ProxyConfig) MarshalYAML() (any, error) {
	return encodeMapping(proxyFields(p), p.rest)
}

// Where the egress proxy reads upstream secrets
type CredentialSource string

// Upstream secret sources
const (
	CredentialEnv       CredentialSource = "env"
	CredentialBitwarden CredentialSource = "bitwarden"
)

// Accepted upstream secret sources
func CredentialSources() []CredentialSource {
	return []CredentialSource{CredentialEnv, CredentialBitwarden}
}

// Desktop app launch options section
type DesktopConfig struct {
	FontFamily            string              `yaml:"font_family"`
	RepoScanEnabled       bool                `yaml:"repo_scan_enabled" coerce:"exact"`
	RepoScanRoots         []string            `yaml:"repo_scan_roots"`
	RepoScanExcludePaths  []string            `yaml:"repo_scan_exclude_paths"`
	ElectronFlags         StringList          `yaml:"electron_flags"`
	RendererMaxOldSpaceMB int                 `yaml:"renderer_max_old_space_mb"`
	OzonePlatformHint     OzoneHint           `yaml:"ozone_platform_hint"`
	DisableGPU            Scalar              `yaml:"disable_gpu"`
	PasswordStore         string              `yaml:"password_store"`
	RendererAccessibility bool                `yaml:"renderer_accessibility" coerce:"notoff"`
	ManageLauncherEntry   bool                `yaml:"manage_launcher_entry" coerce:"notfalse"`
	MacOSSigningIdentity  string              `yaml:"macos_signing_identity"`
	SSHPath               string              `yaml:"ssh_path"`
	AutoContinue          DesktopAutoContinue `yaml:"auto_continue"`
	rest                  rest
}

type desktopFields DesktopConfig

// Decodes the mapping and keeps unknown keys
func (d *DesktopConfig) UnmarshalYAML(n *yaml.Node) error {
	return decodeMapping(n, (*desktopFields)(d), &d.rest)
}

// Writes the mapping with unknown keys after the named ones
func (d DesktopConfig) MarshalYAML() (any, error) {
	return encodeMapping(desktopFields(d), d.rest)
}

// Linux display backend of the desktop app
type OzoneHint string

// Display backends
const (
	OzoneAuto    OzoneHint = "auto"
	OzoneX11     OzoneHint = "x11"
	OzoneWayland OzoneHint = "wayland"
)

// Accepted display backends
func OzoneHints() []OzoneHint {
	return []OzoneHint{OzoneAuto, OzoneX11, OzoneWayland}
}

// Linux keychains for desktop token storage
func PasswordStores() []string {
	return []string{"auto", "gnome-libsecret", "kwallet", "kwallet5", "kwallet6", "basic"}
}

// Crash auto-continue of interrupted desktop turns
type DesktopAutoContinue struct {
	Enabled          bool    `yaml:"enabled"`
	FreshnessMinutes float64 `yaml:"freshness_minutes"`
	MaxAttempts      int     `yaml:"max_attempts"`
	rest             rest
}

type desktopAutoContinueFields DesktopAutoContinue

// Decodes the mapping and keeps unknown keys
func (d *DesktopAutoContinue) UnmarshalYAML(n *yaml.Node) error {
	return decodeMapping(n, (*desktopAutoContinueFields)(d), &d.rest)
}

// Writes the mapping with unknown keys after the named ones
func (d DesktopAutoContinue) MarshalYAML() (any, error) {
	return encodeMapping(desktopAutoContinueFields(d), d.rest)
}

// Icebath Portal account section
type PortalConfig struct {
	KeepaliveIntervalSeconds float64       `yaml:"keepalive_interval_seconds"`
	AnthropicWire            AnthropicWire `yaml:"anthropic_wire"`
	Guest                    bool          `yaml:"guest" coerce:"bool"`
	rest                     rest
}

type portalFields PortalConfig

// Decodes the mapping and keeps unknown keys
func (p *PortalConfig) UnmarshalYAML(n *yaml.Node) error {
	return decodeMapping(n, (*portalFields)(p), &p.rest)
}

// Writes the mapping with unknown keys after the named ones
func (p PortalConfig) MarshalYAML() (any, error) {
	return encodeMapping(portalFields(p), p.rest)
}

// Portal route carrying anthropic models
type AnthropicWire string

// Portal routes
const (
	WireChat   AnthropicWire = "chat"
	WireNative AnthropicWire = "native"
	WireAuto   AnthropicWire = "auto"
)

// Accepted portal routes
func AnthropicWires() []AnthropicWire {
	return []AnthropicWire{WireChat, WireNative, WireAuto}
}

// Google Vertex AI section
type VertexConfig struct {
	ProjectID string `yaml:"project_id"`
	Region    string `yaml:"region"`
	rest      rest
}

type vertexFields VertexConfig

// Decodes the mapping and keeps unknown keys
func (v *VertexConfig) UnmarshalYAML(n *yaml.Node) error {
	return decodeMapping(n, (*vertexFields)(v), &v.rest)
}

// Writes the mapping with unknown keys after the named ones
func (v VertexConfig) MarshalYAML() (any, error) {
	return encodeMapping(vertexFields(v), v.rest)
}

// Managed llama.cpp runtime section
type LocalRuntimeConfig struct {
	Enabled     bool           `yaml:"enabled"`
	Backend     RuntimeBackend `yaml:"backend"`
	ModelsMax   int            `yaml:"models_max"`
	Port        int            `yaml:"port"`
	DetectPorts []int          `yaml:"detect_ports"`
	rest        rest
}

type localRuntimeFields LocalRuntimeConfig

// Decodes the mapping and keeps unknown keys
func (l *LocalRuntimeConfig) UnmarshalYAML(n *yaml.Node) error {
	return decodeMapping(n, (*localRuntimeFields)(l), &l.rest)
}

// Writes the mapping with unknown keys after the named ones
func (l LocalRuntimeConfig) MarshalYAML() (any, error) {
	return encodeMapping(localRuntimeFields(l), l.rest)
}

// Compute backend of the managed llama.cpp runtime
type RuntimeBackend string

// Runtime compute backends
const (
	RuntimeAuto   RuntimeBackend = "auto"
	RuntimeCUDA   RuntimeBackend = "cuda"
	RuntimeMetal  RuntimeBackend = "metal"
	RuntimeVulkan RuntimeBackend = "vulkan"
	RuntimeHIP    RuntimeBackend = "hip"
	RuntimeCPU    RuntimeBackend = "cpu"
)

// Accepted runtime compute backends
func RuntimeBackends() []RuntimeBackend {
	return []RuntimeBackend{RuntimeAuto, RuntimeCUDA, RuntimeMetal, RuntimeVulkan, RuntimeHIP, RuntimeCPU}
}

// Defaults of the desktop and device sections
func defaultDesktop() DesktopConfig {
	return DesktopConfig{
		RepoScanEnabled:       true,
		RepoScanRoots:         []string{},
		RepoScanExcludePaths:  []string{},
		ElectronFlags:         ListOf(),
		OzonePlatformHint:     OzoneAuto,
		DisableGPU:            Text("auto"),
		PasswordStore:         "auto",
		RendererAccessibility: true,
		ManageLauncherEntry:   true,
		AutoContinue:          DesktopAutoContinue{Enabled: true, FreshnessMinutes: 15, MaxAttempts: 2},
	}
}

// Documentation of the screen, computer use, proxy, desktop, portal, vertex and local runtime sections
func desktopDocs() map[string]string {
	return map[string]string{
		"bot_desktop":                             "Bot Desktop: a headless Xfce screen per profile on the gateway host (Linux), streamed to the icebath desktop app where a human can watch, take over (logins, 2FA, CAPTCHAs) and hand back. `icebath computer-use screen`.",
		"bot_desktop.auto_start":                  "Opt-in: start the screen automatically the first time computer_use needs a display on a headless host. Off by default so installing TigerVNC for other reasons never yields a screen nobody asked for; the desktop app's Screen pane offers Start and this toggle.",
		"bot_desktop.min_free_memory_mb":          "Refuse to start below this much free memory (MB), measured on the host or its container cgroup, whichever is tighter. Xvnc + Xfce idle at ~220 MB and a takeover's browser adds 0.5-1 GB, so on a small instance the OOM killer's victim is the dashboard or the gateway rather than the desktop. 0 disables the check.",
		"bot_desktop.idle_stop_minutes":           "Stop a screen nobody has used (no computer_use action, browser spawn, viewer or takeover) for this long; it restarts on the next use. 0 keeps screens up until stopped.",
		"bot_desktop.placement":                   "Where the screen (and with it computer_use and the bot's browser) runs. auto: follow the terminal backend, inside the docker, ssh or streaming plugin sandbox when one is configured, on the gateway host when terminal.backend is local; a sandbox backend that cannot host a screen (modal, daytona, vercel) REFUSES rather than running the desktop on the host. terminal: always inside the terminal backend (error when it cannot host one). gateway: always on the gateway host, even with a sandbox terminal, so the agent's screen, browser and computer_use act OUTSIDE the sandbox; explicit opt-in. The sandbox image needs the desktop stack: ghcr.io/nickheyer/icebath-sandbox:desktop.",
		"computer_use.cua_telemetry":              "cua-driver's upstream telemetry defaults ON; icebath sets CUA_DRIVER_RS_TELEMETRY_ENABLED=0 in every child env unless this is true.",
		"computer_use.autostart":                  "Windows only: opt IN to the per-boot cua-driver-serve logon task. false (default) keeps the driver on-demand; true registers (or repairs) the task at install time, needed to drive Windows over SSH, where Session 0 has no interactive desktop.",
		"computer_use.max_image_dimension":        "Cap the driver screenshot's longest edge (pixels) at session start; shrinks multimodal payloads. 0 disables.",
		"computer_use.capture_after_mode":         "som = screenshot + overlays; ax = elements only, no PNG (faster); vision = pixels only.",
		"computer_use.ax_max_elements":            "Bound the driver's accessibility-tree WALK on every capture. The surfaced element list is capped at 100 with the rest spilled to a cache file, so an unbounded walk pays for nodes the model never sees. 0 = driver default (2,000 elements, depth 25). Keep it in the low hundreds; this caps the nodes COLLECTED, not the walk's wall clock.",
		"computer_use.no_overlay":                 "Disable the driver's cursor overlay, which can peg a core when idle. null = auto (off on macOS and headless or WSL2 Linux, on elsewhere); true = always disable; false = always enable.",
		"computer_use.permission_mode":            "standard = the driver's own approval boundary; bounded = no runtime prompts, anything outside capability_manifest fails closed. `unrestricted` is NOT accepted here: it stays on the per-session YOLO toggle so config can't bypass approvals.",
		"computer_use.capability_manifest":        "Path (~ ok) to the reviewed manifest for permission_mode bounded; passed as --capability-manifest.",
		"computer_use.allow_unsigned_driver":      "macOS only: allow an UNSIGNED CuaDriver.app for the private-session daemon. false fails closed unless signed with the official identity. Only for local driver development from source.",
		"proxy":                                   "Egress credential-injection proxy (iron-proxy) for remote terminal sandboxes (Docker): the sandbox sees opaque tokens and the proxy swaps in real credentials at egress, so a compromised sandbox leaks only tokens that work behind the trusted proxy. Configure with `icebath egress setup`.",
		"proxy.enabled":                           "When false, nothing starts, no docker mounts, no binary installs.",
		"proxy.tunnel_port":                       "Tunnel listener port; sandboxes get HTTPS_PROXY=http://<host>:<port>.",
		"proxy.auto_install":                      "Auto-download the pinned binary into ~/.icebath/bin/; false = iron-proxy on PATH.",
		"proxy.credential_source":                 "Upstream secret source: env = process env; bitwarden = refetch on each proxy restart (requires secrets.bitwarden.enabled).",
		"proxy.enforce_on_docker":                 "true: the Docker backend refuses to start a sandbox if the proxy is enabled but not running. false: fall back to direct outbound with real credentials.",
		"proxy.allow_env_fallback":                "With credential_source bitwarden, a missing token or project_id or an empty fetch makes the daemon raise. true silently falls back to host env (useful mid-migration). A leftover fail_on_uncovered_providers key is ignored.",
		"proxy.upstream_deny_cidrs":               "SSRF deny list. null or empty = loopback, link-local (incl. 169.254.169.254 metadata), RFC1918. An explicit [] opts out (only for hermetic loopback tests).",
		"proxy.extra_allowed_hosts":               "Extra allowed upstream hosts beyond the bundled major-provider defaults; wildcards (`*.foo.com`) supported.",
		"desktop":                                 "icebath desktop app launch options; only affect `icebath desktop`.",
		"desktop.font_family":                     "CSS font-family for the app's chat and UI text (e.g. \"OpenDyslexic\"). Layered in front of the active theme's own sans stack so missing glyphs still fall through. Empty = the theme's face. The terminal pane is terminal.font_family.",
		"desktop.repo_scan_enabled":               "Git repo discovery for the Projects sidebar. Empty roots are a safe no-op; users must explicitly configure roots for filesystem scanning. Session-derived projects remain available.",
		"desktop.electron_flags":                  "Extra web engine flags per launch, e.g. [\"--ozone-platform=x11\"] or GPU fixes. A list of strings; a single string is shell-split.",
		"desktop.renderer_max_old_space_mb":       "Script heap ceiling (MB) for the renderer, applied as --js-flags=--max-old-space-size=N by the app itself (also for Start-menu and .desktop launches). 0 = the engine's default limit. A ceiling turns a machine-wide freeze into a bounded renderer reload.",
		"desktop.ozone_platform_hint":             "Linux Ozone backend, bridged to ELECTRON_OZONE_PLATFORM_HINT (explicit env wins). auto = engine default; x11 = XWayland, for compositors that ignore always-on-top for Wayland clients (e.g. COSMIC), which also puts the HUD on the solid-window input path; wayland = force a native Wayland surface.",
		"desktop.disable_gpu":                     "Bridged to ICEBATH_DESKTOP_DISABLE_GPU: auto = disable GPU only on remote displays (SSH, VNC, RDP); true = always software rendering (no-GPU VMs where the GPU path hangs); false = always keep GPU on.",
		"desktop.password_store":                  "Linux keychain for token storage. auto = detect KWallet (KDE env) or any org.freedesktop.secrets provider via D-Bus; gnome-libsecret|kwallet|kwallet5|kwallet6|basic force one (basic = unencrypted). Bridged to ICEBATH_DESKTOP_PASSWORD_STORE; ignored off-Linux.",
		"desktop.renderer_accessibility":          "Expose the renderer's accessibility tree to the OS (macOS, Windows) so dictation and IME tools that insert text via the accessibility APIs can reach the composer. false bridges to ICEBATH_DESKTOP_RENDERER_ACCESSIBILITY=0 and skips the tree (perf opt-out).",
		"desktop.manage_launcher_entry":           "Linux: false preserves an existing custom XDG launcher entry; missing entries are still created. true keeps the generated entry current on each launch.",
		"desktop.macos_signing_identity":          "macOS only: code-signing identity (login-keychain cert; self-signed works) to re-sign locally rebuilt apps so the Designated Requirement, and thus TCC grants, survives updates. Empty = default ad-hoc identifier-pinned signing.",
		"desktop.ssh_path":                        "Windows only: explicit ssh client for SSH connections, the -G config probe and SSH terminals, e.g. \"C:\\\\Program Files\\\\Git\\\\usr\\\\bin\\\\ssh.exe\" when the in-box OpenSSH is missing or broken. Empty = System32 OpenSSH, then Git for Windows' ssh.exe, then PATH. Read by the app before its first window; restart to apply. Ignored off-Windows.",
		"desktop.auto_continue":                   "Auto-continue a turn killed by a crash: resuming re-submits the interrupted prompt if fresh; a stale one just shows the recovered partial transcript.",
		"desktop.auto_continue.freshness_minutes": "How recent the interruption must be to auto-continue (minutes).",
		"desktop.auto_continue.max_attempts":      "Crash-loop breaker: max automatic re-runs of one interrupted turn.",
		"portal.keepalive_interval_seconds":       "Upper bound (seconds) on the Portal auth keepalive tick, which derives from the server-issued credential lifetime (raising above it has no effect). 0 disables the keepalive thread.",
		"portal.anthropic_wire":                   "Which Portal route carries anthropic/* models. \"chat\" = /v1/chat/completions (default); \"native\" = /v1/messages, the Anthropic Messages wire (signed thinking passthrough, native cache_control scopes); \"auto\" = start on chat and, per session, switch to native from the first response when the Portal upstream serving the model is one where native is known clean. Native is the better wire but on the OpenRouter-served path it re-writes the previous turn's cache on 14-20% of consecutive calls in concurrent tool loops, so chat stays the default until that is fixed.",
		"portal.guest":                            "Portal free tier: with no other provider configured, icebath sets up a free Portal identity on first use (inference on the welcome model + connectors) and offers `/login` (terminal: `icebath auth upgrade`) to sign in. false turns the free tier off entirely: nothing is set up and nothing is used.",
		"vertex":                                  "Google Vertex AI (Gemini). Auth is OAuth2 from a service-account JSON or ADC, NOT an API key; the credential path lives in .env (VERTEX_CREDENTIALS_PATH or GOOGLE_APPLICATION_CREDENTIALS). Bridged to VERTEX_PROJECT_ID and VERTEX_REGION.",
		"vertex.project_id":                       "GCP project ID. Empty -> project_id from the service-account JSON (or ADC).",
		"vertex.region":                           "\"global\" is required for Gemini 3.x preview models (regional endpoints silently 404); use e.g. \"us-central1\" only if your models are region-pinned.",
		"local_runtime":                           "Managed llama.cpp runtime: official binaries, one supervised llama-server in router mode. No context or VRAM knobs by design.",
		"local_runtime.enabled":                   "Off = detection-only (icebath still finds an external llama-server you run).",
		"local_runtime.backend":                   "Engine versions and every dependent library are pinned by the toolchain lock. auto = CUDA on NVIDIA, Metal on macOS, Vulkan on other GPUs, else CPU. Explicit: cuda|metal|vulkan|hip|cpu.",
		"local_runtime.models_max":                "Router process: how many models may be resident at once.",
		"local_runtime.port":                      "Port for the managed server. 0 = pick a free port at spawn.",
		"local_runtime.detect_ports":              "Extra ports detection probes for an external llama-server (besides 8080).",
	}
}
