package gatewayrun

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pyurl"
	"github.com/nickheyer/icebath/pkg/plugins"
	"github.com/nickheyer/icebath/pkg/profiles"
)

// File in the default home recording the pre migration footprint
const MigrationManifestName = "gateway_migration.json"

// Service manager kind and scope of one installed gateway unit
type ServiceUnit struct {
	Kind   string
	System bool
}

// One profile's own gateway footprint, live pid and every installed service unit
type ProfileGateway struct {
	Name        string
	Home        string
	PID         int
	Services    []ServiceUnit
	RunAsUser   string
	UID         *int
	RuntimeHome string
}

func (p *ProfileGateway) IsDefault() bool { return p.Name == "default" }

// Unit the default gateway is restarted through, nil when nothing is installed
func (p *ProfileGateway) Service() *ServiceUnit {
	if len(p.Services) == 0 {
		return nil
	}
	return &p.Services[0]
}

func (p *ProfileGateway) HasGateway() bool { return p.PID != 0 || len(p.Services) > 0 }

func (p *ProfileGateway) HasSystemUnit() bool {
	for _, s := range p.Services {
		if s == (ServiceUnit{"systemd", true}) {
			return true
		}
	}
	return false
}

// Every installed unit joined with a plus, none when nothing is installed
func (p *ProfileGateway) ServiceLabel() string {
	if len(p.Services) == 0 {
		return "none"
	}
	labels := make([]string, len(p.Services))
	for i, s := range p.Services {
		labels[i] = serviceUnitLabel(s)
	}
	return strings.Join(labels, " + ")
}

// Manifest record of the footprint with None for every unknown field
func (p *ProfileGateway) ToDict() *pyjson.Object {
	var pid, service, runAs, uid, runtimeHome any
	if p.PID != 0 {
		pid = int64(p.PID)
	}
	if s := p.Service(); s != nil {
		service = serviceDict(*s)
	}
	services := make([]any, len(p.Services))
	for i, s := range p.Services {
		services[i] = serviceDict(s)
	}
	if p.RunAsUser != "" {
		runAs = p.RunAsUser
	}
	if p.UID != nil {
		uid = int64(*p.UID)
	}
	if p.RuntimeHome != "" {
		runtimeHome = p.RuntimeHome
	}
	return pyjson.ObjectOf("profile", p.Name, "home", p.Home, "pid", pid, "service", service, "services", services,
		"run_as_user", runAs, "uid", uid, "runtime_home", runtimeHome)
}

// Human label of one unit kind and scope
func serviceUnitLabel(s ServiceUnit) string {
	switch s.Kind {
	case "systemd":
		if s.System {
			return "systemd (system)"
		}
		return "systemd (user)"
	case "s6":
		return "s6 slot"
	case "windows":
		return "Windows scheduled task"
	}
	return s.Kind
}

// Park for an s6 slot, which stays registered as a start target, uninstall for every other unit
func removeVerb(s ServiceUnit) string {
	if s.Kind == "s6" {
		return "park"
	}
	return "uninstall"
}

func serviceDict(s ServiceUnit) *pyjson.Object {
	return pyjson.ObjectOf("kind", s.Kind, "system", s.System)
}

// Profiles, their gateway footprints, the flag, the live served record and the preflight verdicts
type MigrationPlan struct {
	DefaultHome        string
	Profiles           []*ProfileGateway
	MultiplexFlagOn    bool
	LiveServed         []string
	LiveServedRecorded bool
	Manifest           *pyjson.Object
	Interrupted        bool
	Blockers           []string
	Notices            []string
	StandaloneByConfig []string
}

func (m *MigrationPlan) Secondaries() []*ProfileGateway {
	out := []*ProfileGateway{}
	for _, p := range m.Profiles {
		if !p.IsDefault() {
			out = append(out, p)
		}
	}
	return out
}

// Default profile footprint, nil when the inventory has none
func (m *MigrationPlan) Default() *ProfileGateway {
	for _, p := range m.Profiles {
		if p.IsDefault() {
			return p
		}
	}
	return nil
}

// Converged by topology, no secondary owning a gateway and the flag on or several profiles served
func (m *MigrationPlan) AlreadyMultiplexed() bool {
	if m.Interrupted || len(m.StandaloneSecondaries()) > 0 {
		return false
	}
	return m.MultiplexFlagOn || (m.LiveServedRecorded && len(m.LiveServed) > 1)
}

func (m *MigrationPlan) StandaloneSecondaries() []*ProfileGateway {
	out := []*ProfileGateway{}
	for _, p := range m.Secondaries() {
		if p.HasGateway() {
			out = append(out, p)
		}
	}
	return out
}

// Default plus every profile that is not parked
func (m *MigrationPlan) ExpectedServedNames() map[string]bool {
	out := map[string]bool{}
	for _, p := range m.Profiles {
		if p.IsDefault() || !profiles.ProfileIsParked(p.Home) {
			out[p.Name] = true
		}
	}
	return out
}

func (m *MigrationPlan) Blocked() bool { return len(m.Blockers) > 0 }

// Service manager the default ends up on, its own else the first one a secondary used
func (m *MigrationPlan) TargetServiceKind() *ServiceUnit {
	if s := m.Default().Service(); s != nil {
		return s
	}
	for _, p := range m.Secondaries() {
		if s := p.Service(); s != nil {
			return s
		}
	}
	return nil
}

// System unit User the default unit keeps, its own else the first a secondary recorded
func (m *MigrationPlan) TargetRunAsUser() string {
	if m.Default().RunAsUser != "" {
		return m.Default().RunAsUser
	}
	for _, p := range m.Secondaries() {
		if p.RunAsUser != "" {
			return p.RunAsUser
		}
	}
	return ""
}

// Plan as the JSON shape the migrate command reports
func (m *MigrationPlan) ToDict() *pyjson.Object {
	profileDicts := make([]any, len(m.Profiles))
	for i, p := range m.Profiles {
		profileDicts[i] = p.ToDict()
	}
	var liveServed any
	if m.LiveServedRecorded {
		liveServed = stringsToAny(m.LiveServed)
	}
	return pyjson.ObjectOf("default_home", m.DefaultHome, "profiles", profileDicts,
		"standalone_by_config", stringsToAny(m.StandaloneByConfig), "multiplex_flag_on", m.MultiplexFlagOn,
		"live_served", liveServed, "already_multiplexed", m.AlreadyMultiplexed(), "interrupted", m.Interrupted,
		"blockers", stringsToAny(m.Blockers), "notices", stringsToAny(m.Notices),
		"eligible", m.EligibleForMigration(), "command", MigrateCommand)
}

func stringsToAny(items []string) []any {
	out := make([]any, len(items))
	for i, s := range items {
		out[i] = s
	}
	return out
}

// Update hook bar, two profiles with a standalone secondary, not converged and nothing blocking
func (m *MigrationPlan) EligibleForMigration() bool {
	return len(m.Profiles) >= 2 && len(m.StandaloneSecondaries()) > 0 && !m.AlreadyMultiplexed() && !m.Blocked()
}

// Runs service helpers as if h were the active home, the launch identity pinned while swapped
func HomeEnv(ctx context.Context, h string) (context.Context, func()) {
	previous, hadPrevious := os.LookupEnv(home.EnvHome)
	pinnedHere := !home.ProcessPinned()
	if pinnedHere {
		home.PinProcess(home.RoutingProcess(), false)
	}
	scoped := home.WithOverride(ctx, h)
	os.Setenv(home.EnvHome, h)
	return scoped, func() {
		if pinnedHere {
			home.PinProcess("", true)
		}
		if hadPrevious {
			os.Setenv(home.EnvHome, previous)
		} else {
			os.Unsetenv(home.EnvHome)
		}
	}
}

// Every profile the multiplexer would serve, parked ones included
func migrateProfileHomes(ctx context.Context) ([]profiles.ServedProfile, error) {
	return profiles.ProfilesToServe(ctx, true, false, true)
}

// Verified pid of a gateway serving h, the host multiplexer included, zero when none or the probe fails
var migrateLiveGatewayPid = func(h string) int {
	pid, ok, err := gateway.LiveGatewayPidForHome(h)
	if err != nil || !ok {
		return 0
	}
	return pid
}

// The one live host gateway, nil when none answers or the probe fails
func hostGatewayOwner() (owner *gateway.HostGateway) {
	defer func() {
		if recover() != nil {
			owner = nil
		}
	}()
	return gateway.HostGatewayNow()
}

// Pid of a gateway h owns, the host process counting only for the home it was launched from
func ownGatewayPid(h string, owner *gateway.HostGateway) int {
	pid := migrateLiveGatewayPid(h)
	if pid == 0 || owner == nil || pid != owner.PID {
		return pid
	}
	same, err := gateway.SameHermesHome(owner.Home, h)
	if err != nil || !same {
		return 0
	}
	return pid
}

// Every installed service of the home's gateway, user scope first, the up slot under s6
var migrateInstalledServices = func(ctx context.Context, h string) ([]ServiceUnit, error) {
	found := []ServiceUnit{}
	if MigrateS6.Running() {
		name, ok := gateway.ProfileNameForHome(h)
		if !ok || name == "" {
			name = "default"
		}
		slotDir := filepath.Join(MigrateS6.Scandir(), MigrateS6.NamedSlotName(name))
		if info, err := os.Stat(slotDir); err == nil && info.IsDir() && (name == "default" || MigrateS6.SlotIsUp(name)) {
			found = append(found, ServiceUnit{"s6", false})
		}
		return found, nil
	}
	scoped, restore := HomeEnv(ctx, h)
	defer restore()
	if gateway.SupportsSystemdServices(scoped) {
		for _, system := range []bool{false, true} {
			unit, err := migrateSystemdUnitPath(scoped, system)
			if err != nil {
				return nil, err
			}
			if pathExists(unit) {
				found = append(found, ServiceUnit{"systemd", system})
			}
		}
	}
	if gateway.IsMacOS() {
		plist, err := gateway.GetLaunchdPlistPath(scoped)
		if err != nil {
			return nil, err
		}
		if pathExists(plist) {
			found = append(found, ServiceUnit{"launchd", false})
		}
	}
	if gateway.IsWindows() && windowsTaskInstalled(scoped) {
		found = append(found, ServiceUnit{"windows", false})
	}
	return found, nil
}

// Unit file path of the active home's gateway service in system or user scope
var migrateSystemdUnitPath = gateway.GetSystemdUnitPath

// Whether the active home's scheduled task, then its Startup folder entry, is installed
var (
	migrateTaskRegistered        = gateway.WindowsIsTaskRegistered
	migrateStartupEntryInstalled = gateway.WindowsStartupEntryInstalled
)

// Whether the scheduled task or its Startup folder fallback is installed for the active home
func windowsTaskInstalled(ctx context.Context) bool {
	registered, err := migrateTaskRegistered(ctx)
	if err != nil {
		return false
	}
	if registered {
		return true
	}
	installed, err := migrateStartupEntryInstalled(ctx)
	return err == nil && installed
}

// User of the system scope systemd unit read before migration removes it, empty without one
var migrateSystemdServiceUser = func(ctx context.Context, h string, services []ServiceUnit) (string, error) {
	if !containsUnit(services, ServiceUnit{"systemd", true}) {
		return "", nil
	}
	scoped, restore := HomeEnv(ctx, h)
	defer restore()
	unit, err := migrateSystemdUnitPath(scoped, true)
	if err != nil {
		return "", err
	}
	return gateway.ReadSystemdUserFromUnit(unit)
}

func containsUnit(services []ServiceUnit, unit ServiceUnit) bool {
	for _, s := range services {
		if s == unit {
			return true
		}
	}
	return false
}

// Only the operator's explicit true counts, the unset default reads as not yet multiplexed
func readMultiplexFlag(ctx context.Context, defaultHome string) (bool, error) {
	flag, err := gateway.ExplicitMultiplexFlag(ctx, defaultHome)
	if err != nil {
		return false, err
	}
	return flag != nil && *flag, nil
}

// Whether the platform's adapter class declares the profile prefix ingress, builtin table then plugin registry
var platformServesProfilePrefix = func(ctx context.Context, platformValue string) bool {
	declares := func(t gateway.AdapterTraits) bool { return t.ServesProfilePrefix }
	if spec, ok := gateway.BuiltinAdapters[gateway.Platform(platformValue)]; ok {
		cls, _, err := gateway.BuiltinAdapterImport(spec.Module, spec.AdapterName, spec.Requirement)
		if err == nil {
			return cls != nil && declares(cls.Traits)
		}
	}
	plugins.DiscoverPlugins(ctx, false)
	if entry := gateway.Registry.Get(ctx, platformValue); entry != nil {
		return gateway.FactoryModuleDeclares(entry, declares)
	}
	return false
}

// Default listener URL a secondary's ingress moves to under its profile prefix
func listenerURL(defaultCfg *gateway.GatewayConfig, platformValue, profile string) string {
	extra := map[string]any{}
	if defaultCfg != nil {
		if pc := defaultCfg.Platforms[gateway.Platform(platformValue)]; pc != nil && pc.Extra != nil {
			extra = pc.Extra
		}
	}
	var host, port any = "<host>", "<port>"
	switch platformValue {
	case "api_server":
		host, port = "127.0.0.1", int64(8642)
	case "webhook":
		host, port = "0.0.0.0", int64(8644)
	}
	if v := extra["host"]; pyrepr.Truthy(v) {
		host = v
	}
	if v := extra["port"]; pyrepr.Truthy(v) {
		port = v
	}
	tail := map[string]string{"api_server": "/v1/...", "webhook": "/webhooks/<route>"}[platformValue]
	if tail == "" {
		tail = "/..."
	}
	return fmt.Sprintf("http://%s:%s/p/%s%s", pyurl.FormatURLHost(pyrepr.Str(host)), pyrepr.Str(port), profile, tail)
}

// Blocker for a secondary port binder without profile prefix ingress, a URL change notice when it has one
func checkSecondaryPortBinders(ctx context.Context, plan *MigrationPlan, configs map[string]*gateway.GatewayConfig) {
	defaultCfg := configs["default"]
	for _, profile := range plan.Secondaries() {
		cfg := configs[profile.Name]
		if cfg == nil {
			continue
		}
		names := make([]string, 0, len(cfg.Platforms))
		for p := range cfg.Platforms {
			names = append(names, string(p))
		}
		sort.Strings(names)
		for _, value := range names {
			pc := cfg.Platforms[gateway.Platform(value)]
			if pc == nil || !pc.Enabled || !gateway.PlatformBindsPort(value, pc.Extra) {
				continue
			}
			if platformServesProfilePrefix(ctx, value) {
				plan.Notices = append(plan.Notices, fmt.Sprintf("Profile '%s': %s moves onto the default listener at "+
					"%s (its key/secret is unchanged; update clients that call the old per-profile port).",
					profile.Name, value, listenerURL(defaultCfg, value, profile.Name)))
				continue
			}
			plan.Blockers = append(plan.Blockers, fmt.Sprintf("Profile '%s' enables %s, which binds its own port and has no "+
				"/p/%s/ ingress on the default listener yet; the multiplexer would skip the whole profile. "+
				"Disable it there (platforms.%s.enabled: false) or add a /p/<profile>/ ingress for it first.",
				profile.Name, value, profile.Name, value))
		}
	}
}

// Every profile's gateway config, an unreadable one recorded as its own blocker
func loadProfileConfigs(ctx context.Context, plan *MigrationPlan) map[string]*gateway.GatewayConfig {
	configs := map[string]*gateway.GatewayConfig{}
	withMultiplexReadMode(func() {
		for _, p := range plan.Profiles {
			cfg, err := profileGatewayConfigFor(ctx, p.Home)
			if err != nil {
				plan.Blockers = append(plan.Blockers, fmt.Sprintf("Profile '%s': could not load its gateway config (%s).",
					p.Name, err.Error()))
				continue
			}
			configs[p.Name] = cfg
		}
	})
	return configs
}

// Profiles and their own gateway footprints with every preflight check run against them
func BuildMigrationPlan(ctx context.Context) (*MigrationPlan, error) {
	defaultHome := home.Root()
	owner := hostGatewayOwner()
	homes, err := migrateProfileHomes(ctx)
	if err != nil {
		return nil, err
	}
	gateways := []*ProfileGateway{}
	for _, entry := range homes {
		pid := ownGatewayPid(entry.Home, owner)
		services, err := migrateInstalledServices(ctx, entry.Home)
		if err != nil {
			return nil, err
		}
		uid, runtimeHome, err := migrateGatewayIdentity(ctx, entry.Home, pid, services)
		if err != nil {
			return nil, err
		}
		runAs, err := migrateSystemdServiceUser(ctx, entry.Home, services)
		if err != nil {
			return nil, err
		}
		p := &ProfileGateway{Name: entry.Name, Home: entry.Home, PID: pid, Services: services, RunAsUser: runAs, UID: uid}
		if runtimeHome != entry.Home {
			p.RuntimeHome = runtimeHome
		}
		gateways = append(gateways, p)
	}
	flag, err := readMultiplexFlag(ctx, defaultHome)
	if err != nil {
		return nil, err
	}
	served, recorded, err := gateway.RecordedServedProfiles(defaultHome)
	if err != nil {
		return nil, err
	}
	plan := &MigrationPlan{DefaultHome: defaultHome, Profiles: gateways, MultiplexFlagOn: flag, LiveServed: served,
		LiveServedRecorded: recorded, Manifest: readManifest(defaultHome), StandaloneByConfig: []string{}}
	foldable := map[string]bool{}
	again, err := migrateProfileHomes(ctx)
	if err != nil {
		return nil, err
	}
	for _, entry := range again {
		foldable[entry.Name] = true
	}
	withStandalone, err := profiles.ProfilesToServe(ctx, true, true, false)
	if err != nil {
		return nil, err
	}
	for _, entry := range withStandalone {
		if entry.Name != "default" && !foldable[entry.Name] {
			plan.StandaloneByConfig = append(plan.StandaloneByConfig, entry.Name)
		}
	}
	plan.Interrupted = plan.MultiplexFlagOn && manifestNotYetServed(plan.Manifest, plan.LiveServed)
	if len(plan.Profiles) < 2 {
		plan.Notices = append(plan.Notices, "Only one profile exists: nothing to multiplex.")
		return plan, nil
	}
	configs := loadProfileConfigs(ctx, plan)
	checkDuplicateCredentials(ctx, plan, configs)
	checkSecondaryPortBinders(ctx, plan, configs)
	for _, b := range AutoMigrationBlockers(plan) {
		plan.Notices = append(plan.Notices, "Not migrated automatically by `icebath update`: "+b)
	}
	plan.Notices = append(plan.Notices, "Profiles created after the migration are served by the running multiplexer as soon as "+
		"they exist (it rescans profiles/ on create/delete and every 30s).")
	return plan, nil
}
