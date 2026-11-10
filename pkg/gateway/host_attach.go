package gateway

import (
	"context"
	"fmt"
	"iter"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pydeadline"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/profiles"
)

const (
	HostAttachStart       = "start"
	HostAttachAttach      = "attach"
	HostAttachRefuse      = "refuse"
	HostAttachReplaceHost = "replace-host"
	HostMigrateCommand    = "icebath gateway migrate --multiplex"
	channelPollInterval   = 250 * time.Millisecond
	hostGatewayCacheTTL   = 2 * time.Second
	hostRescanTimeout     = 8 * time.Second
)

// Seams the attach tests replace, the owner channel wait and the owner control socket verbs
var (
	attachChannelWait         = 5 * time.Second
	hostIdentifyGateway       = IdentifyGateway
	hostRescanGatewayProfiles = RescanGatewayProfiles
	hostProfileIsStandalone   = profiles.ProfileIsStandalone
	hostLiveGatewayPidForHome = LiveGatewayPidForHome
)

func attachLog() *slog.Logger { return slog.Default().With("logger", "gateway.host_attach") }

// Normalized profile name, a lowered trimmed name when it is not a valid profile id
func normalizeAttachProfile(name string) string {
	if name == "" {
		name = "default"
	}
	if n, err := profiles.NormalizeProfileName(name); err == nil {
		return n
	}
	return strings.ToLower(pystr.Strip(name))
}

// Profile a home belongs to, default for the root home
func hostProfileNameForHome(h string) string {
	if name, ok := profileNameForHomeDir(h); ok && name != "" {
		return name
	}
	return "default"
}

// Profile a home belongs to, default for the root home
func HostProfileNameForHome(h string) string { return hostProfileNameForHome(h) }

// The one live host gateway, where it was launched from and what it serves
type HostGateway struct {
	PID         int
	Home        string
	Profiles    []string
	ServedKnown bool
	Standalone  bool
}

// Whether the owner positively serves the profile, never while its served set is unknown
func (g *HostGateway) Serves(profile string) bool {
	if !g.ServedKnown {
		return false
	}
	wanted := normalizeAttachProfile(profile)
	for _, p := range g.Profiles {
		if normalizeAttachProfile(p) == wanted {
			return true
		}
	}
	return false
}

// Profile of the home that launched the owner
func (g *HostGateway) ProfileLabel() string { return hostProfileNameForHome(g.Home) }

// Pid, launching profile and served set of the owner in one line
func (g *HostGateway) Describe() string {
	served := "nothing"
	if !g.ServedKnown {
		served = "not published yet (its control socket has not answered)"
	} else if len(g.Profiles) > 0 {
		served = strings.Join(g.Profiles, ", ")
	}
	return fmt.Sprintf("PID %d (launched by profile '%s'; serves: %s)", g.PID, g.ProfileLabel(), served)
}

// Home the owner was launched from, the default root for records predating the field
func hostRecordHome(r *HostRecord) string {
	if r.Home != "" {
		return r.Home
	}
	return home.Root()
}

// Identify answer of the gateway serving home, nil when it does not answer
func identifyHost(h string) *pyjson.Object { return hostIdentifyGateway(h, 2*time.Second) }

// Served set from a live identify answer, a standalone gateway serving only its own profile
func servedFromIdentity(identity *pyjson.Object) []string {
	if served, _ := identity.Get("served_profiles"); served != nil {
		if list, ok := served.([]any); ok && len(list) > 0 {
			names := make([]string, 0, len(list))
			for _, entry := range list {
				obj, isDict := entry.(*pyjson.Object)
				if !isDict {
					names = append(names, pyrepr.Str(entry))
					continue
				}
				h, _ := obj.Get("home")
				if !pyTruthy(h) {
					h, _ = obj.Get("profile_id")
				}
				if pyTruthy(h) {
					names = append(names, hostProfileNameForHome(pyrepr.Str(h)))
				} else {
					names = append(names, "default")
				}
			}
			return names
		}
	}
	profile, _ := identity.Get("profile")
	if !pyTruthy(profile) {
		return []string{"default"}
	}
	return []string{pyrepr.Str(profile)}
}

// Whether the identify answer is the owner with this pid, launched from the home it was dialled at
func identityMatches(identity *pyjson.Object, pid int, h string) bool {
	if identity == nil {
		return false
	}
	if got, _ := identity.Get("pid"); !jsonEqualsInt(got, pid) {
		return false
	}
	reported, _ := identity.Get("hermes_home")
	if !pyTruthy(reported) {
		return true
	}
	same, err := sameHermesHome(pyrepr.Str(reported), h)
	if err != nil {
		return pyrepr.Str(reported) == h
	}
	return same
}

var hostProbe = probeHostGateway

var hostProbeCache struct {
	mu     sync.Mutex
	set    bool
	at     time.Time
	result *HostGateway
}

// Forgets the memoized host gateway probe, every record write calling it
func invalidateHostGatewayCache() {
	hostProbeCache.mu.Lock()
	hostProbeCache.set = false
	hostProbeCache.result = nil
	hostProbeCache.mu.Unlock()
}

// Forgets the memoized host gateway probe, every record write calling it
func InvalidateHostGatewayCache() { invalidateHostGatewayCache() }

// Live owner from the record proven first, its served set only from a matching identify answer
func probeHostGateway(wait time.Duration) *HostGateway {
	record := hostReadRecord(roleGateway, false)
	if record == nil || !hostRecordLive(record) {
		return nil
	}
	h := hostRecordHome(record)
	identity := pydeadline.PollUntil(func() (*pyjson.Object, bool) {
		id := identifyHost(h)
		if identityMatches(id, record.PID, h) {
			return id, true
		}
		return nil, false
	}, max(0, wait), channelPollInterval)
	if identity != nil {
		return &HostGateway{PID: record.PID, Home: h, Profiles: servedFromIdentity(identity), ServedKnown: true}
	}
	return &HostGateway{PID: record.PID, Home: h, Profiles: []string{}, ServedKnown: false}
}

// The one live host gateway or nil, memoized briefly when no channel wait is asked
func hostGatewayWait(wait time.Duration) *HostGateway {
	hostProbeCache.mu.Lock()
	if wait <= 0 && hostProbeCache.set && time.Since(hostProbeCache.at) < hostGatewayCacheTTL {
		result := hostProbeCache.result
		hostProbeCache.mu.Unlock()
		return result
	}
	hostProbeCache.mu.Unlock()
	result := hostProbe(wait)
	hostProbeCache.mu.Lock()
	hostProbeCache.set, hostProbeCache.at, hostProbeCache.result = true, time.Now(), result
	hostProbeCache.mu.Unlock()
	return result
}

// The one live host gateway or nil, served set unknown when its channel has not answered
func hostGateway() *HostGateway { return hostGatewayWait(0) }

// The one live host gateway or nil, waiting up to wait for its channel to answer
func HostGatewayProbe(wait time.Duration) *HostGateway { return hostGatewayWait(wait) }

// Host gateway when it is live and serves the profile, default included
func hostGatewayServingProfile(profile string, wait time.Duration) *HostGateway {
	g := hostGatewayWait(wait)
	if g != nil && g.Serves(profile) {
		return g
	}
	return nil
}

// Asks the host gateway to rescan profiles, returning it once it serves the profile or answers standalone
func RequestServeProfile(profile string, timeout time.Duration, owner *HostGateway) *HostGateway {
	g := owner
	if g == nil {
		g = hostGatewayWait(attachChannelWait)
	}
	if g == nil || g.Serves(profile) {
		return g
	}
	answer := hostRescanGatewayProfiles(g.Home, timeout)
	if answer == nil {
		return nil
	}
	served := []string{}
	if raw, _ := answer.Get("served_profiles"); raw != nil {
		if list, ok := raw.([]any); ok {
			for _, p := range list {
				served = append(served, pyrepr.Str(p))
			}
		}
	}
	multiplex, present := answer.Get("multiplex")
	standalone := present && multiplex == false
	rescanned := &HostGateway{PID: g.PID, Home: g.Home, Profiles: served, ServedKnown: true, Standalone: standalone}
	if rescanned.Standalone || rescanned.Serves(profile) {
		return rescanned
	}
	return nil
}

// Attach verdict with the owner it concerns, transient when a supervisor must retry it
type HostAttachDecision struct {
	Outcome   string
	Message   string
	Owner     *HostGateway
	Transient bool
}

func AttachMessage(g *HostGateway, profile string) string {
	return fmt.Sprintf("✓ The host gateway already serves profile '%s' — nothing to start.\n"+
		"  %s\n"+
		"  One gateway per host serves every profile; manage it with "+
		"`icebath -p %s gateway restart`.", profile, g.Describe(), g.ProfileLabel())
}

func unknownServedMessage(g *HostGateway, profile string) string {
	return fmt.Sprintf("⏳ A gateway already owns this host and has not published its served set yet.\n"+
		"   %s\n"+
		"   Whether it will serve profile '%s' is unknown, so starting a second gateway\n"+
		"   now could double-bind this profile's platforms. Nothing was started; this is a\n"+
		"   transient state and a service supervisor will retry.\n"+
		"   Take the host over (only from the home that launched it):  icebath gateway run --replace\n"+
		"   Start anyway:  icebath gateway run --force", g.Describe(), profile)
}

func refuseMessage(g *HostGateway, profile string) string {
	return fmt.Sprintf("❌ A gateway already owns this host and will not serve profile '%s'.\n"+
		"   %s\n"+
		"   Exactly one gateway per host serves every profile, so starting a second one\n"+
		"   would double-bind this profile's platforms.\n"+
		"   Fold this profile into it:   %s\n"+
		"   Or start one anyway:         icebath gateway run --force\n"+
		"   (--replace only replaces an owner that serves this profile, so it would not take this one over.)",
		profile, g.Describe(), HostMigrateCommand)
}

func StandaloneRescanMessage(profile string) string {
	return fmt.Sprintf("The host gateway still serves profile '%s'; gateway.standalone is not live yet. "+
		"Wait for the host gateway to rescan (<=30s), or send the rescan-profiles control verb "+
		"to the host gateway before starting this profile's gateway.", profile)
}

// Every live gateway beside a standalone lock owner, asked over its own per home channel what it serves
func coexistingGateways(ctx context.Context, owner *HostGateway) iter.Seq2[*HostGateway, error] {
	return func(yield func(*HostGateway, error) bool) {
		seen := map[int]bool{os.Getpid(): true}
		if owner != nil {
			seen[owner.PID] = true
			if !yield(owner, nil) {
				return
			}
		}
		roster, err := hostProfilesToServe(ctx, true, true, true)
		if err != nil {
			yield(nil, err)
			return
		}
		for _, sp := range roster {
			pid, live, err := hostLiveGatewayPidForHome(sp.Home)
			if err != nil {
				yield(nil, err)
				return
			}
			if !live || seen[pid] {
				continue
			}
			seen[pid] = true
			peer := &HostGateway{PID: pid, Home: sp.Home, Profiles: []string{}, ServedKnown: false}
			if identity := identifyHost(sp.Home); identity != nil && identityMatches(identity, pid, sp.Home) {
				multiplex, present := identity.Get("multiplex")
				peer = &HostGateway{PID: pid, Home: sp.Home, Profiles: servedFromIdentity(identity), ServedKnown: true,
					Standalone: present && multiplex == false}
			}
			if !yield(peer, nil) {
				return
			}
		}
	}
}

// Coexistence for a standalone profile only after every live gateway confirms it does not serve it
func StandaloneAttachDecision(ctx context.Context, ourHome string, owner *HostGateway) (*HostAttachDecision, error) {
	standalone, err := hostProfileIsStandalone(ctx, ourHome)
	if err != nil || !standalone {
		return nil, err
	}
	profile := hostProfileNameForHome(ourHome)
	for peer, err := range coexistingGateways(ctx, owner) {
		if err != nil {
			return nil, err
		}
		if !peer.ServedKnown {
			return &HostAttachDecision{Outcome: HostAttachRefuse, Message: unknownServedMessage(peer, profile), Owner: peer, Transient: true}, nil
		}
		if peer.Serves(profile) {
			return &HostAttachDecision{Outcome: HostAttachRefuse, Message: StandaloneRescanMessage(profile), Owner: peer, Transient: true}, nil
		}
	}
	attachLog().Info(fmt.Sprintf("Profile '%s' is standalone by config; starting beside the host multiplexer", profile))
	return &HostAttachDecision{Outcome: HostAttachStart, Owner: owner}, nil
}

// Attach, rescan then attach, replace or refuse, a configured standalone profile allowed to coexist
func DecideHostAttach(ctx context.Context, ourHome string, replace bool) (HostAttachDecision, error) {
	profile := hostProfileNameForHome(ourHome)
	g := hostGateway()
	other := false
	if g != nil && g.PID != os.Getpid() {
		var err error
		if other, err = launchedByOtherTenant(g.Home, ourHome); err != nil {
			return HostAttachDecision{}, err
		}
	}
	if g == nil || g.PID == os.Getpid() || other {
		d, err := StandaloneAttachDecision(ctx, ourHome, nil)
		if err != nil || d != nil {
			return derefDecision(d), err
		}
		return HostAttachDecision{Outcome: HostAttachStart}, nil
	}
	if replace && (g.Serves(profile) || !g.ServedKnown) {
		return HostAttachDecision{Outcome: HostAttachReplaceHost, Owner: g}, nil
	}
	if g.ServedKnown {
		d, err := StandaloneAttachDecision(ctx, ourHome, g)
		if err != nil || d != nil {
			return derefDecision(d), err
		}
	}
	if g.Serves(profile) {
		return HostAttachDecision{Outcome: HostAttachAttach, Message: AttachMessage(g, profile), Owner: g, Transient: true}, nil
	}
	if !g.ServedKnown {
		waited := hostGatewayWait(attachChannelWait)
		if waited == nil {
			return HostAttachDecision{Outcome: HostAttachStart}, nil
		}
		g = waited
		d, err := StandaloneAttachDecision(ctx, ourHome, g)
		if err != nil || d != nil {
			return derefDecision(d), err
		}
		if g.Serves(profile) {
			return HostAttachDecision{Outcome: HostAttachAttach, Message: AttachMessage(g, profile), Owner: g, Transient: true}, nil
		}
	}
	attached := RequestServeProfile(profile, hostRescanTimeout, g)
	if attached != nil && attached.Serves(profile) {
		return HostAttachDecision{Outcome: HostAttachAttach, Message: AttachMessage(attached, profile), Owner: attached, Transient: true}, nil
	}
	if attached != nil && attached.Standalone {
		attachLog().Info(fmt.Sprintf("Another profile's standalone gateway owns this host (%s); starting profile '%s' beside it. "+
			"Fold every profile onto one gateway with: %s", attached.Describe(), profile, HostMigrateCommand))
		return HostAttachDecision{Outcome: HostAttachStart}, nil
	}
	if !g.ServedKnown {
		return HostAttachDecision{Outcome: HostAttachRefuse, Message: unknownServedMessage(g, profile), Owner: g, Transient: true}, nil
	}
	return HostAttachDecision{Outcome: HostAttachRefuse, Message: refuseMessage(g, profile), Owner: g}, nil
}

func derefDecision(d *HostAttachDecision) HostAttachDecision {
	if d == nil {
		return HostAttachDecision{}
	}
	return *d
}

// Label of the host gateway launching profile when it serves the profile for this tenant, the current profile when empty
func HostMultiplexerServing(ctx context.Context, profileName string) (string, bool, error) {
	name := profileName
	if name == "" {
		current, err := currentProfileName(ctx)
		if err != nil {
			attachLog().Debug("Host multiplexer probe failed", "error", err)
			return "", false, nil
		}
		name = current
	}
	if name == "" {
		name = "default"
	}
	g := hostGatewayServingProfile(name, 0)
	if g == nil {
		return "", false, nil
	}
	other, err := launchedByOtherTenant(g.Home, home.Dir(ctx))
	if err != nil {
		attachLog().Debug("Host multiplexer probe failed", "error", err)
		return "", false, nil
	}
	if other {
		attachLog().Debug(fmt.Sprintf("Host gateway %s belongs to another Icebath home; not ours", g.Describe()))
		return "", false, nil
	}
	return g.ProfileLabel(), true, nil
}
